package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// writeTestFile creates the file at root/rel (and its folders) holding data.
func writeTestFile(t *testing.T, root, rel, data string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

type dryRunResp struct {
	DryRun     bool        `json:"dryRun"`
	FileCount  int         `json:"fileCount"`
	TotalBytes int64       `json:"totalBytes"`
	Files      []batchFile `json:"files"`
	Skipped    []batchSkip `json:"skipped"`
}

type batchResp struct {
	Batch     transferBatch    `json:"batch"`
	Transfers []transferRecord `json:"transfers"`
	Skipped   []batchSkip      `json:"skipped"`
}

func skipCodes(skipped []batchSkip) map[string]string {
	out := map[string]string{}
	for _, s := range skipped {
		out[filepath.Base(s.Path)] = s.Code
	}
	return out
}

func (ta *testAgent) batch(id string) transferBatch {
	ta.t.Helper()
	var out struct {
		Batch transferBatch `json:"batch"`
	}
	if status := ta.do("GET", "/transfers/"+id, nil, &out); status != http.StatusOK {
		ta.t.Fatalf("GET /transfers/%s: status %d", id, status)
	}
	return out.Batch
}

func (ta *testAgent) waitForBatch(id string, cond func(transferBatch) bool) transferBatch {
	ta.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b := ta.batch(id)
		if cond(b) {
			return b
		}
		if time.Now().After(deadline) {
			ta.t.Fatalf("batch %s never reached the expected state; last: %+v", id, b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgentUploadBatchDryRun(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)

	a := writeTestFile(t, ta.root, "a.txt", "aaaa")
	writeTestFile(t, ta.root, "docs/b.txt", "bb")
	writeTestFile(t, ta.root, "docs/sub/c.txt", "c")
	outside := writeTestFile(t, t.TempDir(), "outside.txt", "x")

	var out dryRunResp
	status := ta.do("POST", "/transfer/upload-batch", map[string]interface{}{
		"paths": []string{
			a,
			filepath.Join(ta.root, "docs"),
			a,                                       // duplicate
			filepath.Join(ta.root, "docs", "b.txt"), // already in via docs
			filepath.Join(ta.root, "missing.txt"),
			outside,
		},
		"recursive": true,
		"dryRun":    true,
	}, &out)
	if status != http.StatusOK {
		t.Fatalf("dry run: status %d, want 200", status)
	}
	if !out.DryRun || out.FileCount != 3 || out.TotalBytes != 7 {
		t.Errorf("dry run = %d files / %d bytes, want 3 files / 7 bytes", out.FileCount, out.TotalBytes)
	}
	var got []string
	for _, f := range out.Files {
		got = append(got, f.RemoteDir+"|"+f.Name)
	}
	sort.Strings(got)
	if want := "docs/sub|c.txt docs|b.txt |a.txt"; strings.Join(got, " ") != want {
		t.Errorf("files = %v, want %s", got, want)
	}
	codes := skipCodes(out.Skipped)
	want := map[string]string{
		"b.txt": "duplicate", "missing.txt": "not_found", "outside.txt": "path_outside_roots",
	}
	for name, code := range want {
		if codes[name] != code {
			t.Errorf("skip for %s = %q, want %q (all: %+v)", name, codes[name], code, out.Skipped)
		}
	}
	// a.txt was skipped once, as the second copy.
	if n := len(out.Skipped); n != 4 {
		t.Errorf("skipped %d paths, want 4: %+v", n, out.Skipped)
	}

	// Without recursive a folder is skipped rather than walked.
	ta.do("POST", "/transfer/upload-batch", map[string]interface{}{
		"paths": []string{filepath.Join(ta.root, "docs")}, "dryRun": true,
	}, &out)
	if out.FileCount != 0 || len(out.Skipped) != 1 || out.Skipped[0].Code != "is_a_directory" {
		t.Errorf("non-recursive folder: %+v, want it skipped as is_a_directory", out)
	}

	fake.mu.Lock()
	nodes := len(fake.nodes)
	fake.mu.Unlock()
	if nodes != 1 {
		t.Errorf("a dry run created %d nodes in storage, want none", nodes-1)
	}
}

func TestAgentUploadBatchSkipsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	writeTestFile(t, ta.root, "dir/ok.txt", "ok")
	locked := writeTestFile(t, ta.root, "dir/locked.txt", "no")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	lockedDir := filepath.Join(ta.root, "dir", "lockeddir")
	writeTestFile(t, ta.root, "dir/lockeddir/x.txt", "x")
	if err := os.Chmod(lockedDir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(lockedDir, 0o755) })

	var out dryRunResp
	ta.do("POST", "/transfer/upload-batch", map[string]interface{}{
		"paths": []string{filepath.Join(ta.root, "dir")}, "recursive": true, "dryRun": true,
	}, &out)
	if out.FileCount != 1 || out.Files[0].Name != "ok.txt" {
		t.Errorf("files = %+v, want only ok.txt", out.Files)
	}
	codes := skipCodes(out.Skipped)
	if codes["locked.txt"] != "permission_denied" || codes["lockeddir"] != "permission_denied" {
		t.Errorf("skipped = %+v, want locked.txt and lockeddir as permission_denied", out.Skipped)
	}
}

func TestAgentUploadBatchUploadsTree(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	single := writeTestFile(t, ta.root, "single.txt", "1")
	writeTestFile(t, ta.root, "Photos/a.jpg", "aa")
	writeTestFile(t, ta.root, "Photos/2024/b.jpg", "bbb")

	var out batchResp
	status := ta.do("POST", "/transfer/upload-batch", map[string]interface{}{
		"paths":     []string{single, filepath.Join(ta.root, "Photos"), filepath.Join(ta.root, "nope")},
		"parentId":  "root",
		"recursive": true,
	}, &out)
	if status != http.StatusAccepted {
		t.Fatalf("POST /transfer/upload-batch: status %d, want 202", status)
	}
	if out.Batch.FileCount != 3 || out.Batch.TotalBytes != 6 || len(out.Transfers) != 3 || len(out.Skipped) != 1 {
		t.Fatalf("batch = %+v (%d transfers, %d skipped), want 3 files / 6 bytes and one skip", out.Batch, len(out.Transfers), len(out.Skipped))
	}

	b := ta.waitForBatch(out.Batch.ID, func(b transferBatch) bool { return b.FinishedAt != nil })
	if b.State != transferCompleted || b.Completed != 3 || b.BytesDone != 6 {
		t.Errorf("finished batch = %+v, want 3 completed files and 6 bytes done", b)
	}

	fake.mu.Lock()
	photos, _ := fake.resolvePath("/Photos")
	year, _ := fake.resolvePath("/Photos/2024")
	bNode, _ := fake.resolvePath("/Photos/2024/b.jpg")
	singleNode, _ := fake.resolvePath("/single.txt")
	fake.mu.Unlock()
	if photos == nil || photos.NodeType != "folder" || year == nil || bNode == nil || singleNode == nil {
		t.Fatalf("storage is missing the uploaded tree: Photos %v, 2024 %v, b.jpg %v, single.txt %v", photos, year, bNode, singleNode)
	}

	var list struct {
		Batch   transferBatch    `json:"batch"`
		Active  []transferRecord `json:"active"`
		History []transferRecord `json:"history"`
	}
	// An unrelated single upload must not show up in the batch's list.
	ta.do("POST", "/transfer/upload", map[string]string{"localPath": single, "parentId": "root"}, nil)
	ta.do("GET", "/transfers?batchId="+out.Batch.ID, nil, &list)
	if list.Batch.ID != out.Batch.ID || len(list.Active) != 0 || len(list.History) != 3 {
		t.Errorf("GET /transfers?batchId: batch %q, %d active, %d history; want the batch with 3 finished", list.Batch.ID, len(list.Active), len(list.History))
	}
	for _, rec := range list.History {
		if rec.BatchID != out.Batch.ID || rec.State != transferCompleted {
			t.Errorf("batch transfer = %+v, want completed and tagged with the batch", rec)
		}
		if rec.Name == "b.jpg" && (rec.RemoteDir != "Photos/2024" || rec.ParentID != year.ID) {
			t.Errorf("b.jpg went to %q (%s), want Photos/2024 (%s)", rec.RemoteDir, rec.ParentID, year.ID)
		}
	}
}

// gatedUploadServer serves fake's Storage API but holds every file upload
// until a value arrives on release, so the queue can be seen mid-batch.
func gatedUploadServer(t *testing.T, fake *fakeStorageAPI, release <-chan struct{}) *storageClient {
	t.Helper()
	inner := fake.mux("acct-1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/accounts/acct-1/files" {
			// Read first: the server only notices the client going away (and
			// cancels r.Context()) once the body has been consumed.
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return &storageClient{baseURL: server.URL, apiURL: server.URL, accountID: "acct-1", cfg: &Config{AccessToken: "test-token"}}
}

func TestAgentUploadBatchRunsOneAtATimeAndCancels(t *testing.T) {
	fake := newFakeStorageAPI()
	release := make(chan struct{})
	ta := newTestAgent(t, gatedUploadServer(t, fake, release), fake, true)
	var paths []string
	for i := 0; i < 4; i++ {
		paths = append(paths, writeTestFile(t, ta.root, fmt.Sprintf("f%d.txt", i), "data"))
	}

	var out batchResp
	if status := ta.do("POST", "/transfer/upload-batch", map[string]interface{}{"paths": paths, "parentId": "root"}, &out); status != http.StatusAccepted {
		t.Fatalf("POST /transfer/upload-batch: status %d, want 202", status)
	}
	ids := out.Batch.TransferIDs

	ta.waitForTransfer(ids[0], func(rec transferRecord) bool { return rec.State == transferRunning })
	for _, id := range ids[1:] {
		if rec := ta.transfer(id); rec.State != transferQueued {
			t.Errorf("transfer %s is %s while the first file uploads, want queued", rec.Name, rec.State)
		}
	}

	release <- struct{}{}
	ta.waitForTransfer(ids[0], func(rec transferRecord) bool { return rec.State == transferCompleted })
	ta.waitForTransfer(ids[1], func(rec transferRecord) bool { return rec.State == transferRunning })

	// Cancelling one queued file leaves the rest of the batch going.
	var one struct {
		Transfer transferRecord `json:"transfer"`
	}
	if status := ta.do("DELETE", "/transfers/"+ids[3], nil, &one); status != http.StatusOK || one.Transfer.State != transferCanceled {
		t.Errorf("cancel queued file: status %d state %q, want 200 canceled", status, one.Transfer.State)
	}

	var cancelled struct {
		Batch transferBatch `json:"batch"`
	}
	if status := ta.do("DELETE", "/transfers/"+out.Batch.ID, nil, &cancelled); status != http.StatusOK {
		t.Fatalf("DELETE batch: status %d, want 200", status)
	}
	b := cancelled.Batch
	if b.State != transferCanceled || b.Completed != 1 || b.Canceled != 3 || b.FinishedAt == nil {
		t.Errorf("cancelled batch = %+v, want canceled with 1 completed and 3 canceled", b)
	}
	if status := ta.do("DELETE", "/transfers/"+out.Batch.ID, nil, nil); status != http.StatusConflict {
		t.Errorf("cancelling a finished batch: status %d, want 409", status)
	}
}

func TestAgentUploadBatchQueuedCanceledOnShutdown(t *testing.T) {
	fake := newFakeStorageAPI()
	ta := newTestAgent(t, gatedUploadServer(t, fake, make(chan struct{})), fake, true)
	paths := []string{writeTestFile(t, ta.root, "a", "a"), writeTestFile(t, ta.root, "b", "b")}

	var out batchResp
	ta.do("POST", "/transfer/upload-batch", map[string]interface{}{"paths": paths, "parentId": "root"}, &out)
	ta.waitForTransfer(out.Batch.TransferIDs[0], func(rec transferRecord) bool { return rec.State == transferRunning })
	ta.cancel()
	ta.waitForBatch(out.Batch.ID, func(b transferBatch) bool {
		return b.State == transferCanceled && b.Canceled == 2
	})
}

func TestAgentUploadBatchLimits(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	for i := 0; i <= maxBatchFiles; i++ {
		writeTestFile(t, ta.root, fmt.Sprintf("many/%04d", i), "")
	}

	var errResp struct {
		Error apiError `json:"error"`
	}
	status := ta.do("POST", "/transfer/upload-batch", map[string]interface{}{
		"paths": []string{filepath.Join(ta.root, "many")}, "recursive": true, "dryRun": true,
	}, &errResp)
	if status != http.StatusBadRequest || errResp.Error.Code != "too_many_files" {
		t.Errorf("%d files: status %d code %q, want 400 too_many_files", maxBatchFiles+1, status, errResp.Error.Code)
	}

	os.Remove(filepath.Join(ta.root, "many", "0000"))
	var out dryRunResp
	ta.do("POST", "/transfer/upload-batch", map[string]interface{}{
		"paths": []string{filepath.Join(ta.root, "many")}, "recursive": true, "dryRun": true,
	}, &out)
	if out.FileCount != maxBatchFiles {
		t.Errorf("exactly %d files: got %d, want them accepted", maxBatchFiles, out.FileCount)
	}

	var noFiles struct {
		Error   apiError    `json:"error"`
		Skipped []batchSkip `json:"skipped"`
	}
	status = ta.do("POST", "/transfer/upload-batch", map[string]interface{}{
		"paths": []string{filepath.Join(ta.root, "gone")}, "parentId": "root",
	}, &noFiles)
	if status != http.StatusBadRequest || noFiles.Error.Code != "no_files" || len(noFiles.Skipped) != 1 {
		t.Errorf("nothing to upload: status %d code %q skipped %d, want 400 no_files with the skip", status, noFiles.Error.Code, len(noFiles.Skipped))
	}
	if status := ta.do("POST", "/transfer/upload-batch", map[string]interface{}{"paths": []string{"x"}}, nil); status != http.StatusBadRequest {
		t.Errorf("missing parentId: status %d, want 400", status)
	}
	if status := ta.do("POST", "/transfer/upload-batch", map[string]interface{}{
		"paths": []string{writeTestFile(t, ta.root, "x", "x")}, "parentId": "root", "batchId": "nope",
	}, nil); status != http.StatusBadRequest {
		t.Errorf("non-UUID batchId: status %d, want 400", status)
	}
}
