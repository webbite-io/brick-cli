package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
)

// --- Client batch: files from another attached client to local disk ---
//
// POST /transfer/client-batch is download-batch with another client of the
// same user as the source rather than Drive. It takes paths on that client,
// sourceClientId (files, or with recursive folders too) and a local
// destination folder, localDir, inside a writable root. Nothing goes through
// Drive: the bytes stream from the source client through the Storage API to
// this one.
//
// The source client resolves the paths itself, with the dry run of its own
// upload-batch, so its roots, symlink rules and file cap apply as they would
// to an upload from it. Its skips are passed on. Here each file is then
// checked as a download's would be: a name that can't be used on this device
// is skipped with invalid_name, and a file already in its place with exists.
// The rest are queued under one batch (direction "copy") and fetched one at a
// time from the source's /fs/read, as the files of download-batch are.
//
// Progress and cancelling work as for the other batches: GET
// /transfers/{batchId}, GET /transfers?batchId=, DELETE /transfers/{batchId}
// or /transfers/{id}.

type clientBatchReq struct {
	SourceClientID string   `json:"sourceClientId"`
	Paths          []string `json:"paths"`
	LocalDir       string   `json:"localDir"`
	Recursive      bool     `json:"recursive"`
	DryRun         bool     `json:"dryRun"`
	BatchID        string   `json:"batchId"`
}

func (a *agentServer) handleClientBatch(w http.ResponseWriter, r *http.Request) {
	var req clientBatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body")
		return
	}
	if strings.TrimSpace(req.SourceClientID) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "sourceClientId is required")
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
	localDir, ok := a.resolveLocalDir(w, req.LocalDir, req.DryRun)
	if !ok {
		return
	}
	files, skipped, err := a.resolveClientBatch(r.Context(), req.SourceClientID, req.Paths, localDir, req.Recursive)
	if err != nil {
		writeErr(w, err)
		return
	}
	dest := transferBatch{Direction: "copy", LocalDir: localDir, SourceClientID: req.SourceClientID}
	a.answerDownloadBatch(w, req.DryRun, req.BatchID, dest, files, skipped,
		func(ctx context.Context, f downloadBatchFile, dest string, onProgress func(written, total int64)) error {
			return a.sc.clientReadToFile(ctx, req.SourceClientID, f.SourcePath, dest, f.Size, onProgress)
		})
}

// resolveClientBatch turns paths on client sourceID into the files to copy
// from it into localDir.
func (a *agentServer) resolveClientBatch(ctx context.Context, sourceID string, paths []string, localDir string, recursive bool) ([]downloadBatchFile, []batchSkip, error) {
	srcFiles, srcSkipped, err := a.sc.clientUploadDryRun(ctx, sourceID, paths, recursive)
	if err != nil {
		return nil, nil, err
	}
	dr := &downloadResolver{
		ctx: ctx, localDir: localDir, recursive: recursive,
		seen: map[string]bool{}, dests: map[string]bool{}, skipped: srcSkipped,
	}
	for _, f := range srcFiles {
		if !validLocalDir(f.RemoteDir) {
			dr.skip(path.Join(f.RemoteDir, f.Name), "invalid_name", "a folder's name can't be used on this device")
			continue
		}
		if err := dr.addFile(f.LocalPath, downloadBatchFile{SourcePath: f.LocalPath, Name: f.Name, Size: f.Size}, f.RemoteDir); err != nil {
			return nil, nil, err
		}
	}
	return dr.files, dr.skipped, nil
}

// validLocalDir reports whether every folder of dir ("a/b", or "") can be used
// as a path segment on this device.
func validLocalDir(dir string) bool {
	if dir == "" {
		return true
	}
	for _, name := range strings.Split(dir, "/") {
		if !validLocalName(name) {
			return false
		}
	}
	return true
}

// clientPath is the Storage API path of endpoint op on attached client id.
func clientPath(id, op string) string {
	return "/clients/" + url.PathEscape(id) + op
}

// clientUploadDryRun asks attached client id, through the Storage API, what
// an upload-batch of paths from it would come to.
func (sc *storageClient) clientUploadDryRun(ctx context.Context, id string, paths []string, recursive bool) ([]batchFile, []batchSkip, error) {
	body, err := json.Marshal(map[string]interface{}{"paths": paths, "recursive": recursive, "dryRun": true})
	if err != nil {
		return nil, nil, err
	}
	resp, err := sc.request(ctx, "POST", clientPath(id, "/transfer/upload-batch"), body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, sourceError(resp)
	}
	var out struct {
		Files   []batchFile `json:"files"`
		Skipped []batchSkip `json:"skipped"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, nil, err
	}
	return out.Files, out.Skipped, nil
}

// clientReadToFile streams file p on attached client id, through the Storage
// API, to destPath.
func (sc *storageClient) clientReadToFile(ctx context.Context, id, p, destPath string, total int64, onProgress func(written, total int64)) error {
	resp, err := sc.request(ctx, "GET", clientPath(id, "/fs/read?"+url.Values{"path": {p}}.Encode()), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return sourceError(resp)
	}
	return writeBodyToFile(resp.Body, destPath, total, onProgress)
}

// sourceError turns the source client's (or the Storage API's) failed answer
// into an API error saying so. A client from before upload-batch answers it
// with a plain-text 404.
func sourceError(resp *http.Response) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var env struct {
		Error apiError `json:"error"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Error.Code == "" {
		if resp.StatusCode == http.StatusNotFound {
			return newAPIError(http.StatusBadGateway, "source_outdated", "the source device runs a version of Brick without this feature")
		}
		msg := strings.TrimSpace(string(bytes.ToValidUTF8(raw, nil)))
		if msg == "" {
			msg = resp.Status
		}
		return newAPIError(http.StatusBadGateway, "source_error", "source device: "+msg)
	}
	// Always a 502: the caller's own request was fine, and passing on a 401 or
	// 403 would read as its own credentials failing.
	return newAPIError(http.StatusBadGateway, env.Error.Code, "source device: "+env.Error.Message)
}
