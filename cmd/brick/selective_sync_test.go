package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReconcileAllDoesNotCreateExcludedFolder is a regression test: a remote
// folder listed in excludeDirs must never be created locally, even though it
// still shows up in the server's tree. Before this fix, reconcileAll's
// "create missing local directories for remote folders" pass ran for every
// remote folder unconditionally, recreating an excluded folder (empty, since
// the files under it are correctly skipped) on every sync pass.
func TestReconcileAllDoesNotCreateExcludedFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "brick"), 0o755); err != nil {
		t.Fatal(err)
	}
	syncFolder := t.TempDir()

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/accounts/acct-1/nodes/root/children", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, storageNodeList{
			Data: []storageNode{
				{ID: "cam", ParentID: "root", Name: "Camera Uploads", NodeType: "folder"},
			},
			Count: 1,
		})
	})
	mux.HandleFunc("/v1/accounts/acct-1/nodes/cam/children", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, storageNodeList{
			Data: []storageNode{
				{ID: "photo1", ParentID: "cam", Name: "photo1.jpg", NodeType: "file", Etag: `"etag-photo1"`},
			},
			Count: 1,
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	eng := &syncEngine{
		sc: &storageClient{
			baseURL:   server.URL,
			apiURL:    server.URL,
			accountID: "acct-1",
			cfg:       &Config{AccessToken: "test-token"},
		},
		folder:          syncFolder,
		accountID:       "acct-1",
		rootID:          "root",
		excludeDirs:     []string{"Camera Uploads"},
		firstSync:       true,
		conflictMode:    "device",
		state:           &SyncState{Folder: syncFolder, Entries: map[string]SyncEntry{}, Folders: map[string]bool{}, FolderIDs: map[string]string{}},
		recentlyWritten: map[string]time.Time{},
	}

	for i := 0; i < 2; i++ {
		if err := eng.reconcileAll(context.Background()); err != nil {
			t.Fatalf("reconcileAll (pass %d): %v", i, err)
		}
	}

	if _, err := os.Stat(filepath.Join(syncFolder, "Camera Uploads")); err == nil {
		t.Error("excluded folder was created locally, want it left absent")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat excluded folder: %v", err)
	}

	if !eng.state.Folders["Camera Uploads"] {
		t.Error("excluded folder should still be recorded in the index (so a later un-exclude/re-exclude round trip works), but it wasn't")
	}
	if eng.state.FolderIDs["Camera Uploads"] != "cam" {
		t.Errorf("FolderIDs[Camera Uploads] = %q, want \"cam\"", eng.state.FolderIDs["Camera Uploads"])
	}

	if _, ok := eng.state.Entries["Camera Uploads/photo1.jpg"]; !ok {
		t.Error("excluded file should still get an index entry (see reconcileExcludedFile), but none was recorded")
	}
	if _, err := os.Stat(filepath.Join(syncFolder, "Camera Uploads", "photo1.jpg")); !os.IsNotExist(err) {
		t.Errorf("excluded file must never be downloaded: stat = %v", err)
	}
}
