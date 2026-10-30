package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// Each endpoint answers one method only; any other gets 405 and leaves the
// filesystem alone.
func TestAgentRejectsWrongMethod(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	file := writeTestFile(t, ta.root, "keep.txt", "keep")

	for _, tc := range []struct{ method, path string }{
		{"POST", "/version"},
		{"POST", "/fs/roots"},
		{"POST", "/fs/list?path=" + ta.root},
		{"POST", "/fs/stat?path=" + file},
		{"POST", "/fs/read?path=" + file},
		{"POST", "/fs/write?path=" + file},
		{"GET", "/fs/mkdir?path=" + filepath.Join(ta.root, "dir")},
		{"GET", "/fs/delete?path=" + file},
		{"POST", "/fs/delete?path=" + file},
		{"GET", "/fs/move"},
		{"GET", "/fs/copy"},
		{"GET", "/transfer/upload"},
		{"GET", "/transfer/download"},
		{"GET", "/transfer/upload-batch"},
		{"GET", "/transfer/download-batch"},
		{"GET", "/transfer/client-batch"},
		{"POST", "/transfers"},
		{"POST", "/transfers/x"},
		{"POST", "/config/exclude-dirs"},
	} {
		if status := ta.do(tc.method, tc.path, nil, nil); status != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: status %d, want 405", tc.method, tc.path, status)
		}
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("%s was removed by a request with the wrong method", file)
	}
}
