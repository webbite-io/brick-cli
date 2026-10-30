package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// --- Batch upload: many local files to Drive, queued and run one at a time ---
//
// POST /transfer/upload-batch takes local paths (files, or with recursive
// folders too) and a destination parentId. The paths are resolved up front
// into a list of unique, readable files; anything that can't be uploaded is
// left out and reported in "skipped" rather than failing the request. With
// dryRun the list and its total size are returned without uploading, so the
// caller can e.g. check it against the quota first. Otherwise every file is
// registered as a queued transfer under one batch and the request answers
// 202 straight away; the agent's upload queue then uploads them in order,
// one file at a time across all batches.
//
// A picked folder keeps its name: uploading ~/Photos recursively into
// parentId puts its files under Photos/... there, creating the folders (or
// reusing ones that already exist) as the files reach them. Empty folders
// are not created.
//
// Progress: GET /transfers/{batchId} for the batch's summary, or
// GET /transfers?batchId= for its files' records. DELETE /transfers/{batchId}
// cancels what's left of it; DELETE /transfers/{id} cancels a single file.

// maxBatchFiles caps how many files one batch upload may resolve to.
const maxBatchFiles = 1000

type uploadBatchReq struct {
	Paths     []string `json:"paths"`
	ParentID  string   `json:"parentId"`
	Recursive bool     `json:"recursive"`
	DryRun    bool     `json:"dryRun"`
	BatchID   string   `json:"batchId"`
}

// batchFile is one file a batch upload resolved to.
type batchFile struct {
	LocalPath string `json:"localPath"`
	RemoteDir string `json:"remoteDir,omitempty"` // folder path under parentId, "/"-separated
	Name      string `json:"name"`
	Size      int64  `json:"size"`
}

// batchSkip is a requested path, or a file found under one, left out of a
// batch upload. Code is one of the API's error codes (not_found,
// permission_denied, path_outside_roots, ...) or duplicate.
type batchSkip struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

var errTooManyBatchFiles = newAPIError(http.StatusBadRequest, "too_many_files",
	fmt.Sprintf("a batch transfer can hold at most %d files", maxBatchFiles))

// batchResolver turns a batch upload's paths into its files.
type batchResolver struct {
	a         *agentServer
	recursive bool
	seen      map[string]bool // real paths of the files taken so far
	files     []batchFile
	skipped   []batchSkip
}

func (a *agentServer) resolveBatch(paths []string, recursive bool) ([]batchFile, []batchSkip, error) {
	br := &batchResolver{a: a, recursive: recursive, seen: map[string]bool{}}
	for _, p := range paths {
		if err := br.addPath(p); err != nil {
			return nil, nil, err
		}
	}
	return br.files, br.skipped, nil
}

func (br *batchResolver) skip(p, code, message string) {
	br.skipped = append(br.skipped, batchSkip{Path: p, Code: code, Message: message})
}

// skipErr records p as skipped for err, classified as writeOSError would.
func (br *batchResolver) skipErr(p string, err error) {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		br.skip(p, ae.Code, ae.Message)
	case os.IsNotExist(err):
		br.skip(p, "not_found", err.Error())
	case os.IsPermission(err):
		br.skip(p, "permission_denied", err.Error())
	default:
		br.skip(p, "io_error", err.Error())
	}
}

// addPath adds the file at requested path p, or (recursive) every file under
// the folder at p. Only errTooManyBatchFiles is returned; every other problem
// is recorded as a skip.
func (br *batchResolver) addPath(p string) error {
	if strings.TrimSpace(p) == "" {
		// resolveSafe would take this to mean the first root.
		br.skip(p, "invalid_path", "path is empty")
		return nil
	}
	abs, err := br.a.resolveSafe(p)
	if err != nil {
		br.skipErr(p, err)
		return nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		br.skipErr(abs, err)
		return nil
	}
	if !info.IsDir() {
		return br.addFile(abs, "", info)
	}
	if !br.recursive {
		br.skip(abs, "is_a_directory", "path is a directory; set recursive to upload its contents")
		return nil
	}
	return br.addTree(abs)
}

// addTree adds every file under folder abs, under a remote folder of the
// same name. Symlinks inside it are followed to files within the roots but
// not into folders, which could loop.
func (br *batchResolver) addTree(abs string) error {
	walkRoot := evalSafe(abs)
	top := filepath.Base(abs)
	return filepath.WalkDir(walkRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			br.skipErr(p, err)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := os.Stat(p) // follows a symlink
		if err != nil {
			br.skipErr(p, err)
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			if info.IsDir() {
				br.skip(p, "symlinked_directory", "symlinked folders are not followed")
				return nil
			}
			if _, ok := br.a.rootOf(p); !ok {
				br.skip(p, "path_outside_roots", "symlink points outside the allowed roots")
				return nil
			}
		}
		rel, _ := filepath.Rel(walkRoot, filepath.Dir(p))
		return br.addFile(p, path.Join(top, filepath.ToSlash(rel)), info)
	})
}

// addFile adds the file at abs (to go in remoteDir), unless it was already
// added under another path or can't be read.
func (br *batchResolver) addFile(abs, remoteDir string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		br.skip(abs, "not_a_regular_file", "only regular files can be uploaded")
		return nil
	}
	real := evalSafe(abs)
	if br.seen[real] {
		br.skip(abs, "duplicate", "the file is already in this batch")
		return nil
	}
	f, err := os.Open(abs)
	if err != nil {
		br.skipErr(abs, err)
		return nil
	}
	f.Close()
	if len(br.files) == maxBatchFiles {
		return errTooManyBatchFiles
	}
	br.seen[real] = true
	br.files = append(br.files, batchFile{
		LocalPath: abs, RemoteDir: remoteDir, Name: filepath.Base(abs), Size: info.Size(),
	})
	return nil
}

func (a *agentServer) handleUploadBatch(w http.ResponseWriter, r *http.Request) {
	var req uploadBatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if len(req.Paths) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "paths is required")
		return
	}
	if len(req.Paths) > maxBatchFiles {
		writeErr(w, errTooManyBatchFiles)
		return
	}
	if req.ParentID == "" && !req.DryRun {
		writeError(w, http.StatusBadRequest, "invalid_request", "parentId is required")
		return
	}

	files, skipped, err := a.resolveBatch(req.Paths, req.Recursive)
	if err != nil {
		writeErr(w, err)
		return
	}
	if req.DryRun {
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
			"error":   map[string]string{"code": "no_files", "message": "none of the paths resolved to a file that can be uploaded"},
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
				Direction: "upload", LocalPath: f.LocalPath, ParentID: req.ParentID,
				Name: f.Name, RemoteDir: f.RemoteDir, BytesTotal: f.Size,
			},
			cancel: cancel,
		}
	}
	batch, err := a.transfers.beginBatch(req.BatchID, transferBatch{Direction: "upload", ParentID: req.ParentID}, items)
	if err != nil {
		for _, it := range items {
			it.cancel()
		}
		writeErr(w, err)
		return
	}

	folders := &batchFolders{parentID: req.ParentID, ids: map[string]string{}}
	jobs := make([]queuedJob, len(files))
	transfers := make([]transferRecord, len(files))
	for i, id := range batch.TransferIDs {
		it := queuedUpload{id: id, file: files[i], ctx: ctxs[i], folders: folders}
		jobs[i] = queuedJob{id: id, ctx: it.ctx, run: func() (*storageNode, error) { return a.uploadBatchFile(it) }}
		transfers[i], _ = a.transfers.get(id)
	}
	a.queue.push(jobs...)
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"batch": batch, "transfers": transfers, "skipped": skipped,
	})
}

// --- The transfer queue ---

// queuedUpload is one batch upload file, as its queuedJob runs it.
type queuedUpload struct {
	id      string
	file    batchFile
	ctx     context.Context // cancelled by DELETE /transfers/{id} or brick shutting down
	folders *batchFolders
}

// batchFolders maps a batch's remote folder paths to the ids of the folders
// created (or found) for them. Only the queue's single worker touches it.
type batchFolders struct {
	parentID string
	ids      map[string]string
}

// folder returns the id of folder dir ("a/b") under the batch's parentId,
// creating any part of it that doesn't exist yet.
func (bf *batchFolders) folder(ctx context.Context, sc *storageClient, dir string) (string, error) {
	id := bf.parentID
	if dir == "" {
		return id, nil
	}
	prefix := ""
	for _, name := range strings.Split(dir, "/") {
		prefix = path.Join(prefix, name)
		if known, ok := bf.ids[prefix]; ok {
			id = known
			continue
		}
		node, err := sc.createFolder(ctx, id, name)
		if err != nil {
			return "", fmt.Errorf("create folder %q: %w", prefix, err)
		}
		bf.ids[prefix] = node.ID
		id = node.ID
	}
	return id, nil
}

// queuedJob is one batch file, upload or download, waiting its turn: run
// transfers it, under ctx (cancelled by DELETE /transfers/{id} or brick
// shutting down).
type queuedJob struct {
	id  string
	ctx context.Context
	run func() (*storageNode, error)
}

// transferQueue holds batch files waiting to be transferred, oldest first. A
// single worker (runTransferQueue) takes them one at a time, whichever way
// they go.
type transferQueue struct {
	mu    sync.Mutex
	items []queuedJob
	wake  chan struct{}
}

func newTransferQueue() *transferQueue {
	return &transferQueue{wake: make(chan struct{}, 1)}
}

func (q *transferQueue) push(items ...queuedJob) {
	q.mu.Lock()
	q.items = append(q.items, items...)
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *transferQueue) pop() (queuedJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return queuedJob{}, false
	}
	it := q.items[0]
	q.items[0] = queuedJob{}
	q.items = q.items[1:]
	return it, true
}

// runTransferQueue transfers queued batch files until ctx (the session) ends,
// then marks whatever is still queued as canceled.
func (a *agentServer) runTransferQueue(ctx context.Context) {
	for {
		it, ok := a.queue.pop()
		if !ok {
			select {
			case <-a.queue.wake:
				continue
			case <-ctx.Done():
			}
			for it, ok := a.queue.pop(); ok; it, ok = a.queue.pop() {
				a.transfers.finish(it.ctx, it.id, nil, it.ctx.Err())
			}
			return
		}
		a.runQueuedJob(it)
	}
}

func (a *agentServer) runQueuedJob(it queuedJob) {
	if !a.transfers.start(it.id) {
		return // cancelled while it waited
	}
	node, err := it.run()
	a.transfers.finish(it.ctx, it.id, node, err)
}

func (a *agentServer) uploadBatchFile(it queuedUpload) (*storageNode, error) {
	if err := it.ctx.Err(); err != nil {
		return nil, err
	}
	parentID, err := it.folders.folder(it.ctx, a.sc, it.file.RemoteDir)
	if err != nil {
		return nil, err
	}
	// The file may have changed, or gone, since the batch was queued.
	f, err := os.Open(it.file.LocalPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is no longer a regular file", it.file.LocalPath)
	}
	a.transfers.update(it.id, func(rec *transferRecord) {
		rec.ParentID = parentID
		rec.BytesTotal = info.Size()
	})
	return a.sc.uploadStream(it.ctx, parentID, it.file.Name, f, info.Size(), func(written int64) {
		a.transfers.progress(it.id, written)
	})
}
