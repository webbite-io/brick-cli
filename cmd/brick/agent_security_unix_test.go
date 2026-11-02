//go:build unix

package main

import (
	"net/http"
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
