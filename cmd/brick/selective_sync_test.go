package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// A synced folder or file moved into an excluded folder on the server must
// not be mirrored with a local rename: that would carry a local copy into a
// folder sync ignores, where nothing ever updates or removes it again. The
// normal passes treat the old path as deleted on the server instead. Moves out
// of an excluded folder are left to the normal passes too (they download it).
func TestRemoteMovesAcrossExcludedFolderAreNotMirrored(t *testing.T) {
	folder := t.TempDir()
	for _, rel := range []string{"Photos/a.jpg", "b.jpg"} {
		abs := filepath.Join(folder, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e := &syncEngine{
		folder:      folder,
		excludeDirs: []string{"Camera Uploads"},
		state: &SyncState{
			Folder:    folder,
			Entries:   map[string]SyncEntry{"Photos/a.jpg": {RelPath: "Photos/a.jpg", NodeID: "a1"}, "b.jpg": {RelPath: "b.jpg", NodeID: "b1"}},
			Folders:   map[string]bool{"Photos": true, "Camera Uploads/Trip": true},
			FolderIDs: map[string]string{"Photos": "f1", "Camera Uploads/Trip": "f2"},
		},
	}
	localDirs := map[string]bool{"Photos": true}
	localFiles := map[string]int64{"Photos/a.jpg": 1, "b.jpg": 1}

	e.applyRemoteFolderMoves(map[string]storageNode{
		"Camera Uploads/Photos": {ID: "f1", NodeType: "folder"},
		"Trip":                  {ID: "f2", NodeType: "folder"},
	}, localDirs, localFiles)
	e.applyRemoteFileMoves(map[string]storageNode{
		"Camera Uploads/b.jpg": {ID: "b1", NodeType: "file"},
	}, localFiles)

	if _, err := os.Stat(filepath.Join(folder, "Camera Uploads")); !os.IsNotExist(err) {
		t.Errorf("excluded folder created locally: stat = %v", err)
	}
	for _, rel := range []string{"Photos/a.jpg", "b.jpg"} {
		if _, ok := e.state.Entries[rel]; !ok {
			t.Errorf("index entry %q rewritten, want it left for the file pass", rel)
		}
	}
	if n := e.moved.Load(); n != 0 {
		t.Errorf("moved = %d, want 0", n)
	}
}

// Pass 0 not mirroring such a move only works if the file pass then visits
// every file under both the old and new paths: a folder move's delta carries
// just the folder's own row, so applyRemoteDelta must report its descendants.
func TestApplyRemoteDeltaReportsDescendantsOfFolderMovedAcrossExclude(t *testing.T) {
	for _, tc := range []struct{ to, wantOld, wantNew string }{
		{"/Camera Uploads/docs", "docs/a.txt", "Camera Uploads/docs/a.txt"}, // into
		{"/archive", "", ""}, // neither side excluded: pass 0 mirrors it, nothing extra to report
	} {
		e := newDeltaTestEngine()
		e.excludeDirs = []string{"Camera Uploads"}
		affected := e.applyRemoteDelta([]storageNode{{ID: "f1", NodeType: "folder", Path: tc.to}})
		if tc.wantOld == "" {
			if len(affected) != 0 {
				t.Errorf("move to %s: affected = %v, want none", tc.to, affected)
			}
			continue
		}
		for _, want := range []string{tc.wantOld, tc.wantNew} {
			if !slices.Contains(affected, want) {
				t.Errorf("move to %s: affected = %v, want it to include %q", tc.to, affected, want)
			}
		}
	}

	// Out of an excluded folder: the newly synced files must be reported so
	// an incremental pass downloads them.
	e := newDeltaTestEngine()
	e.excludeDirs = []string{"docs"}
	affected := e.applyRemoteDelta([]storageNode{{ID: "f1", NodeType: "folder", Path: "/archive"}})
	for _, want := range []string{"archive/a.txt", "archive/b.txt", "docs/a.txt", "docs/b.txt"} {
		if !slices.Contains(affected, want) {
			t.Errorf("move out of exclude: affected = %v, want it to include %q", affected, want)
		}
	}
}

// An account with no folders yet gets no sync-scope question, but Camera
// Uploads is still left out, so it isn't synced by default once the first
// camera upload creates it. Exclusions already configured are kept.
func TestSyncScopeOnboardingExcludesCameraUploadsOnEmptyAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "brick"), 0o755); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/accounts/acct-1/nodes/root/children", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, storageNodeList{
			Data:  []storageNode{{ID: "f1", ParentID: "root", Name: "loose.txt", NodeType: "file"}},
			Count: 1,
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	cfg := &Config{AccessToken: "test-token", ActiveAccountID: "acct-1"}
	cfg.ensureActiveAccount().ExcludeDirs = []string{"Old"}
	sc := &storageClient{baseURL: server.URL, apiURL: server.URL, accountID: "acct-1", cfg: cfg}

	for i := 0; i < 2; i++ { // and running it again doesn't add a duplicate
		if err := runSyncScopeOnboarding(sc, "root", cfg, nil); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"Old", cameraUploadsFolder}
	if got := cfg.ensureActiveAccount().ExcludeDirs; !slices.Equal(got, want) {
		t.Errorf("ExcludeDirs = %v, want %v", got, want)
	}
	saved, err := os.ReadFile(filepath.Join(home, ".config", "brick", "config.yaml"))
	if err != nil {
		t.Fatalf("config not saved: %v", err)
	}
	if !strings.Contains(string(saved), cameraUploadsFolder) {
		t.Errorf("saved config doesn't exclude Camera Uploads:\n%s", saved)
	}
}

// brick sync -s lists only the account's current top-level folders, so only
// their entries may change: an exclusion for anything else (Camera Uploads
// before it exists, a nested path, a folder missing on the server) must
// survive, where it used to be dropped because huh rebuilds the selection
// from the listed options alone.
func TestSelectiveSyncExcludesKeepsUnlistedEntries(t *testing.T) {
	for _, tc := range []struct {
		name                string
		prev, listed, incl  []string
		wantExcl, wantNewly []string
	}{
		{
			name:     "untick one; unlisted entries kept",
			prev:     []string{cameraUploadsFolder, "Photos/2019"},
			listed:   []string{"Music", "Work"},
			incl:     []string{"Work"},
			wantExcl: []string{cameraUploadsFolder, "Photos/2019", "Music"}, wantNewly: []string{"Music"},
		},
		{
			name:     "re-tick a previously excluded folder",
			prev:     []string{"Music", cameraUploadsFolder},
			listed:   []string{"Music", "Work"},
			incl:     []string{"Music", "Work"},
			wantExcl: []string{cameraUploadsFolder},
		},
		{
			name:     "keep an existing exclusion: not newly excluded",
			prev:     []string{"Music"},
			listed:   []string{"Music", "Work"},
			incl:     []string{"Work"},
			wantExcl: []string{"Music"},
		},
		{
			name:     "everything ticked, nothing excluded",
			listed:   []string{"Music", "Work"},
			incl:     []string{"Music", "Work"},
			wantExcl: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			excl, newly := selectiveSyncExcludes(tc.prev, tc.listed, tc.incl)
			if !slices.Equal(excl, tc.wantExcl) || !slices.Equal(newly, tc.wantNewly) {
				t.Errorf("got exclude=%v newly=%v, want exclude=%v newly=%v", excl, newly, tc.wantExcl, tc.wantNewly)
			}
		})
	}
}

func TestNormalizeExcludeDirEntries(t *testing.T) {
	got := normalizeExcludeDirEntries([]string{"/Music/", "", "Music", "Photos/2019/", "/"})
	if want := []string{"Music", "Photos/2019"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
