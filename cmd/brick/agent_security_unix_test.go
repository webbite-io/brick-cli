//go:build unix

package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAgentCopySkipsFIFO(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	writeTestFile(t, ta.root, "src/a.txt", "a")
	fifo := filepath.Join(ta.root, "src", "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}

	var out struct {
		Skipped []batchSkip `json:"skipped"`
	}
	if status := ta.do("POST", "/fs/copy", fromToReq{From: filepath.Join(ta.root, "src"), To: filepath.Join(ta.root, "dst")}, &out); status != http.StatusOK {
		t.Fatalf("copy folder: status %d, want 200", status)
	}
	if len(out.Skipped) != 1 || out.Skipped[0].Code != "not_a_regular_file" {
		t.Errorf("skipped = %+v, want the FIFO as not_a_regular_file", out.Skipped)
	}

	var errResp struct {
		Error apiError `json:"error"`
	}
	if status := ta.do("POST", "/fs/copy", fromToReq{From: fifo, To: filepath.Join(ta.root, "pipe-copy")}, &errResp); status != http.StatusBadRequest || errResp.Error.Code != "not_a_regular_file" {
		t.Errorf("copy FIFO: status %d code %q, want 400 not_a_regular_file", status, errResp.Error.Code)
	}
}

func TestAgentRefusesToReadOrUploadFIFO(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	fifo := filepath.Join(ta.root, "pipe")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	var errResp struct {
		Error apiError `json:"error"`
	}
	if status := ta.do("GET", "/fs/read?path="+url.QueryEscape(fifo), nil, &errResp); status != http.StatusBadRequest || errResp.Error.Code != "not_a_regular_file" {
		t.Errorf("read FIFO: status %d code %q, want 400 not_a_regular_file", status, errResp.Error.Code)
	}
	errResp.Error = apiError{}
	if status := ta.do("POST", "/transfer/upload", uploadReq{LocalPath: fifo, ParentID: "root"}, &errResp); status != http.StatusBadRequest || errResp.Error.Code != "not_a_regular_file" {
		t.Errorf("upload FIFO: status %d code %q, want 400 not_a_regular_file", status, errResp.Error.Code)
	}
}

// A file that turns into a FIFO while its batch waits fails, rather than
// blocking the queue for every batch after it.
func TestAgentUploadBatchFileTurnedFIFOFails(t *testing.T) {
	oneTransferAtATime(t)
	fake := newFakeStorageAPI()
	release := make(chan struct{})
	ta := newTestAgent(t, gatedUploadServer(t, fake, release), fake, true)
	a := writeTestFile(t, ta.root, "a.txt", "a")
	b := writeTestFile(t, ta.root, "b.txt", "b")
	c := writeTestFile(t, ta.root, "c.txt", "c")

	var out batchResp
	if status := ta.do("POST", "/transfer/upload-batch", map[string]interface{}{"paths": []string{a, b, c}, "parentId": "root"}, &out); status != http.StatusAccepted {
		t.Fatalf("upload-batch: status %d, want 202", status)
	}
	ids := out.Batch.TransferIDs
	ta.waitForTransfer(ids[0], func(rec transferRecord) bool { return rec.State == transferRunning })
	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(b, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	close(release)
	ta.waitForTransfer(ids[1], func(rec transferRecord) bool { return rec.State == transferFailed })
	ta.waitForTransfer(ids[2], func(rec transferRecord) bool { return rec.State == transferCompleted })
}
