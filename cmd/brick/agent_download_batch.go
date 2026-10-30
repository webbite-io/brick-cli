package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// --- Batch download: many Drive files to local disk, queued and run one at a time ---
//
// POST /transfer/download-batch is upload-batch the other way round. It takes
// Drive node ids (files, or with recursive folders too) and a local
// destination folder, localDir, inside a writable root. The ids are resolved
// up front into a list of unique files; anything that can't be downloaded is
// left out and reported in "skipped" rather than failing the request. With
// dryRun the list and its total size are returned without downloading.
// Otherwise every file is registered as a queued transfer under one batch,
// the request answers 202 straight away, and the agent's transfer queue
// downloads them in order, one file at a time across all batches.
//
// A picked folder keeps its name: downloading Drive's Photos recursively into
// localDir puts its files under Photos/... there, creating the folders (or
// reusing ones that already exist) as the files reach them. Empty folders are
// not created. A file is never written over one already on disk: it is
// skipped with code exists, or fails if one appeared after it was queued.
//
// Progress and cancelling work as for upload-batch: GET /transfers/{batchId},
// GET /transfers?batchId=, DELETE /transfers/{batchId} or /transfers/{id}.

type downloadBatchReq struct {
	NodeIDs   []string `json:"nodeIds"`
	LocalDir  string   `json:"localDir"`
	Recursive bool     `json:"recursive"`
	DryRun    bool     `json:"dryRun"`
	BatchID   string   `json:"batchId"`
}

// downloadBatchFile is one file a batch download resolved to: a Drive node,
// or for a copy from another client (see agent_client_batch.go) a path there.
type downloadBatchFile struct {
	NodeID     string `json:"nodeId,omitempty"`
	SourcePath string `json:"sourcePath,omitempty"`
	Dir        string `json:"dir,omitempty"`       // folder path under localDir, "/"-separated
	LocalPath  string `json:"localPath,omitempty"` // where it goes, when localDir was given
	Name       string `json:"name"`
	Size       int64  `json:"size"`
}

// downloadResolver turns a batch download's node ids into its files.
type downloadResolver struct {
	ctx       context.Context
	sc        *storageClient
	localDir  string // absolute, or "" for a dry run without one
	recursive bool
	seen      map[string]bool // node ids (or source paths) taken so far
	dests     map[string]bool // local paths taken so far
	files     []downloadBatchFile
	skipped   []batchSkip
}

func (a *agentServer) resolveDownloadBatch(ctx context.Context, ids []string, localDir string, recursive bool) ([]downloadBatchFile, []batchSkip, error) {
	dr := &downloadResolver{
		ctx: ctx, sc: a.sc, localDir: localDir, recursive: recursive,
		seen: map[string]bool{}, dests: map[string]bool{},
	}
	for _, id := range ids {
		if err := dr.addNode(id); err != nil {
			return nil, nil, err
		}
	}
	return dr.files, dr.skipped, nil
}

func (dr *downloadResolver) skip(p, code, message string) {
	dr.skipped = append(dr.skipped, batchSkip{Path: p, Code: code, Message: message})
}

// addNode adds the file with id, or (recursive) every file under the folder
// with id. Only errTooManyBatchFiles and the request's cancellation are
// returned; every other problem is recorded as a skip.
func (dr *downloadResolver) addNode(id string) error {
	if strings.TrimSpace(id) == "" {
		dr.skip(id, "invalid_node_id", "node id is empty")
		return nil
	}
	node, err := dr.sc.getNode(dr.ctx, id)
	if err != nil {
		if ctxErr := dr.ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		dr.skip(id, "upstream_error", err.Error())
		return nil
	}
	if node.IsDeleted {
		dr.skip(node.Name, "not_found", "the node is in the trash")
		return nil
	}
	switch node.NodeType {
	case "file":
		return dr.addNodeFile(*node, "")
	case "folder":
		if !dr.recursive {
			dr.skip(node.Name, "is_a_directory", "node is a folder; set recursive to download its contents")
			return nil
		}
		if !validLocalName(node.Name) {
			dr.skip(node.Name, "invalid_name", "the folder's name can't be used on this device")
			return nil
		}
		return dr.addTree(*node)
	default:
		dr.skip(node.Name, "not_a_file", "only files and folders can be downloaded")
		return nil
	}
}

// addTree adds every file under folder, breadth first, under a local folder
// of the same name.
func (dr *downloadResolver) addTree(folder storageNode) error {
	type queued struct {
		id  string
		dir string
	}
	queue := []queued{{id: folder.ID, dir: folder.Name}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		children, err := dr.sc.listChildren(dr.ctx, cur.id)
		if err != nil {
			if ctxErr := dr.ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			dr.skip(cur.dir, "upstream_error", err.Error())
			continue
		}
		for _, ch := range children {
			if ch.IsDeleted {
				continue
			}
			switch ch.NodeType {
			case "folder":
				if !validLocalName(ch.Name) {
					dr.skip(path.Join(cur.dir, ch.Name), "invalid_name", "the folder's name can't be used on this device")
					continue
				}
				queue = append(queue, queued{id: ch.ID, dir: path.Join(cur.dir, ch.Name)})
			case "file":
				if err := dr.addNodeFile(ch, cur.dir); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (dr *downloadResolver) addNodeFile(node storageNode, dir string) error {
	return dr.addFile(node.ID, downloadBatchFile{NodeID: node.ID, Name: node.Name, Size: node.SizeBytes}, dir)
}

// addFile adds file f (its source, Name and Size set) to go in dir under
// localDir, unless key, its node id or source path, was already added, its
// name can't be used locally, or something is already in its place.
func (dr *downloadResolver) addFile(key string, f downloadBatchFile, dir string) error {
	display := path.Join(dir, f.Name)
	if !validLocalName(f.Name) {
		dr.skip(display, "invalid_name", "the file's name can't be used on this device")
		return nil
	}
	if dr.seen[key] {
		dr.skip(display, "duplicate", "the file is already in this batch")
		return nil
	}
	var dest string
	if dr.localDir != "" {
		dest = filepath.Join(dr.localDir, filepath.FromSlash(dir), f.Name)
		if dr.dests[dest] {
			dr.skip(dest, "duplicate", "another file in this batch goes to the same place")
			return nil
		}
		if _, err := os.Lstat(dest); err == nil {
			dr.skip(dest, "exists", "a file or folder with this name already exists")
			return nil
		}
	}
	if len(dr.files) == maxBatchFiles {
		return errTooManyBatchFiles
	}
	dr.seen[key] = true
	if dest != "" {
		dr.dests[dest] = true
	}
	f.Dir, f.LocalPath = dir, dest
	dr.files = append(dr.files, f)
	return nil
}

// validLocalName reports whether a Drive node's name can be used as one path
// segment on this device without leaving the folder it goes in.
func validLocalName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, "/\\\x00") && !strings.ContainsRune(name, os.PathSeparator)
}

func (a *agentServer) handleDownloadBatch(w http.ResponseWriter, r *http.Request) {
	var req downloadBatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if len(req.NodeIDs) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "nodeIds is required")
		return
	}
	if len(req.NodeIDs) > maxBatchFiles {
		writeErr(w, errTooManyBatchFiles)
		return
	}
	localDir, ok := a.resolveLocalDir(w, req.LocalDir, req.DryRun)
	if !ok {
		return
	}
	files, skipped, err := a.resolveDownloadBatch(r.Context(), req.NodeIDs, localDir, req.Recursive)
	if err != nil {
		writeErr(w, err)
		return
	}
	a.answerDownloadBatch(w, req.DryRun, req.BatchID, transferBatch{Direction: "download", LocalDir: localDir}, files, skipped,
		func(ctx context.Context, f downloadBatchFile, dest string, onProgress func(written, total int64)) error {
			return a.sc.downloadToFile(ctx, f.NodeID, dest, f.Size, onProgress)
		})
}

// resolveLocalDir resolves a batch download's destination folder, which must
// be a writable folder within the roots. A dry run may leave it out ("" is
// returned), but one that names it is checked as the real thing would be, so
// a read-only root shows up before anything is queued.
func (a *agentServer) resolveLocalDir(w http.ResponseWriter, localDir string, dryRun bool) (string, bool) {
	if strings.TrimSpace(localDir) == "" {
		if dryRun {
			return "", true
		}
		writeError(w, http.StatusBadRequest, "invalid_request", "localDir is required")
		return "", false
	}
	abs, err := a.resolveWritable(localDir)
	if err != nil {
		writeErr(w, err)
		return "", false
	}
	info, err := os.Stat(abs)
	if err != nil {
		writeOSError(w, err)
		return "", false
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, "not_a_directory", "localDir is not a directory")
		return "", false
	}
	return abs, true
}

// fetchFunc writes batch download file f's bytes to dest, reporting bytes
// written as it goes.
type fetchFunc func(ctx context.Context, f downloadBatchFile, dest string, onProgress func(written, total int64)) error

// answerDownloadBatch answers a batch download resolved to files: with the
// dry run's list, or by queueing them under a batch like dest, each fetched
// into its folder under dest.LocalDir by fetch.
func (a *agentServer) answerDownloadBatch(w http.ResponseWriter, dryRun bool, batchID string, dest transferBatch, files []downloadBatchFile, skipped []batchSkip, fetch fetchFunc) {
	if dryRun {
		var total int64
		for _, f := range files {
			total += f.Size
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"dryRun": true, "fileCount": len(files), "totalBytes": total, "files": files, "skipped": skipped,
		})
		return
	}
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":   map[string]string{"code": "no_files", "message": "nothing resolved to a file that can be downloaded"},
			"skipped": skipped,
		})
		return
	}

	items := make([]queuedTransfer, len(files))
	ctxs := make([]context.Context, len(files))
	for i, f := range files {
		ctx, cancel := context.WithCancel(a.ctx)
		ctxs[i] = ctx
		items[i] = queuedTransfer{
			rec: transferRecord{
				Direction: dest.Direction, LocalPath: f.LocalPath, NodeID: f.NodeID,
				SourceClientID: dest.SourceClientID, SourcePath: f.SourcePath,
				Name: f.Name, BytesTotal: f.Size,
			},
			cancel: cancel,
		}
	}
	batch, err := a.transfers.beginBatch(batchID, dest, items)
	if err != nil {
		for _, it := range items {
			it.cancel()
		}
		writeErr(w, err)
		return
	}

	jobs := make([]queuedJob, len(files))
	transfers := make([]transferRecord, len(files))
	for i, id := range batch.TransferIDs {
		id, f, ctx := id, files[i], ctxs[i]
		jobs[i] = queuedJob{id: id, ctx: ctx, run: func() (*storageNode, error) {
			return nil, a.downloadBatchFile(ctx, id, dest.LocalDir, f, fetch)
		}}
		transfers[i], _ = a.transfers.get(id)
	}
	a.queue.push(jobs...)
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"batch": batch, "transfers": transfers, "skipped": skipped,
	})
}

// downloadBatchFile fetches queued file f into its folder under localDir.
func (a *agentServer) downloadBatchFile(ctx context.Context, id, localDir string, f downloadBatchFile, fetch fetchFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := a.mkdirWithin(localDir, f.Dir)
	if err != nil {
		return err
	}
	dest := filepath.Join(dir, f.Name)
	if _, err := a.resolveWritable(dest); err != nil {
		return err
	}
	// Something may have taken its place since the batch was queued.
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fetch(ctx, f, dest, func(written, _ int64) {
		a.transfers.progress(id, written)
	})
}

// mkdirWithin creates folder rel ("a/b") under base one level at a time,
// reusing levels that exist, and returns its absolute path. Each level is
// checked to be a writable folder within the roots once it exists, so a
// symlink along the way can't lead the download out of them.
func (a *agentServer) mkdirWithin(base, rel string) (string, error) {
	dir := base
	if rel == "" {
		return dir, nil
	}
	for _, name := range strings.Split(rel, "/") {
		dir = filepath.Join(dir, name)
		if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if _, err := a.resolveWritable(dir); err != nil {
			return "", err
		}
		info, err := os.Stat(dir)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%s is not a folder", dir)
		}
	}
	return dir, nil
}
