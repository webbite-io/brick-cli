package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// newLocalTreeCacheTestEngine builds a syncEngine wired to a fake Storage API
// that layers a /check-updates handler (always reporting "nothing changed",
// which is enough to let reconcileAllImpl's fetchRemote step take its
// incremental path on every pass after the first) over fakeStorageAPI's
// existing node/file routes from transfer_test.go.
func newLocalTreeCacheTestEngine(t *testing.T) (*syncEngine, *fakeStorageAPI) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "brick"), 0o755); err != nil {
		t.Fatal(err)
	}
	syncFolder := t.TempDir()

	fs := newFakeStorageAPI()
	var checkUpdatesCalls atomic.Int32
	top := http.NewServeMux()
	top.HandleFunc("/v1/accounts/acct-1/check-updates", func(w http.ResponseWriter, r *http.Request) {
		n := checkUpdatesCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"data": []storageNode{}, "count": 0,
			"serverTime": int64(1000 + n), "nextCursor": "",
		})
	})
	top.Handle("/", fs.mux("acct-1"))
	server := httptest.NewServer(top)
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
		firstSync:       true,
		conflictMode:    "device",
		state:           &SyncState{Folder: syncFolder, Entries: map[string]SyncEntry{}, Folders: map[string]bool{}, FolderIDs: map[string]string{}},
		recentlyWritten: map[string]time.Time{},
	}
	return eng, fs
}

// reconcileLocalChanges must only reconcile the rel paths it's told changed,
// not every local file that happens to exist — the whole point of patching
// localTreeFiles/localTreeDirs from the watcher's reported paths instead of a
// full filepath.WalkDir of the sync folder on every pass.
func TestReconcileLocalChangesOnlyProcessesReportedPaths(t *testing.T) {
	eng, _ := newLocalTreeCacheTestEngine(t)
	ctx := context.Background()

	if err := eng.reconcileAll(ctx); err != nil {
		t.Fatalf("bootstrap reconcileAll: %v", err)
	}
	if !eng.localTreeValid {
		t.Fatal("localTreeValid should be true after the bootstrap pass")
	}

	// Both files are written directly to disk, bypassing markLocalChange
	// entirely -- standing in for a file a real fsnotify watcher would have
	// reported, and one it hasn't (yet).
	if err := os.WriteFile(filepath.Join(eng.folder, "watched.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(eng.folder, "unwatched.txt"), []byte("bye"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := eng.reconcileLocalChanges(ctx, []string{"watched.txt"}); err != nil {
		t.Fatalf("reconcileLocalChanges: %v", err)
	}
	if _, ok := eng.state.Entries["watched.txt"]; !ok {
		t.Error("watched.txt: want an index entry (it was reported changed), got none")
	}
	if _, ok := eng.state.Entries["unwatched.txt"]; ok {
		t.Error("unwatched.txt: want no index entry yet (never reported changed), but it was reconciled anyway")
	}

	// Once it's reported, a later pass picks it up.
	if err := eng.reconcileLocalChanges(ctx, []string{"unwatched.txt"}); err != nil {
		t.Fatalf("reconcileLocalChanges (2): %v", err)
	}
	if _, ok := eng.state.Entries["unwatched.txt"]; !ok {
		t.Error("unwatched.txt: want an index entry after being reported changed, got none")
	}
}

// A directory moved or copied into the sync folder as a whole only ever gets
// one fsnotify Create event for the directory itself, never one per file
// already inside it. applyLocalChanges must expand that into a walk scoped to
// just the new subtree so every file inside it still gets uploaded, without
// falling back to a full walk of the whole sync folder.
func TestReconcileLocalChangesExpandsNewDirectoryContents(t *testing.T) {
	eng, _ := newLocalTreeCacheTestEngine(t)
	ctx := context.Background()

	if err := eng.reconcileAll(ctx); err != nil {
		t.Fatalf("bootstrap reconcileAll: %v", err)
	}

	sub := filepath.Join(eng.folder, "imported")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Only the directory itself is reported, exactly as fsnotify would.
	if err := eng.reconcileLocalChanges(ctx, []string{"imported"}); err != nil {
		t.Fatalf("reconcileLocalChanges: %v", err)
	}

	for _, rel := range []string{"imported/a.txt", "imported/b.txt"} {
		if _, ok := eng.state.Entries[rel]; !ok {
			t.Errorf("%s: want an index entry (discovered via the new-subtree walk), got none", rel)
		}
	}
	if !eng.localTreeDirs["imported"] {
		t.Error(`localTreeDirs["imported"] = false, want true`)
	}
}

// A directory removed locally is reported as a single event for the
// directory itself; applyLocalChanges must expand that into every file the
// engine previously knew lived under it (from the sync index), since there's
// no disk copy left to walk, so each one is still pushed as a remote
// deletion.
func TestReconcileLocalChangesExpandsRemovedDirectoryContents(t *testing.T) {
	eng, _ := newLocalTreeCacheTestEngine(t)
	ctx := context.Background()

	sub := filepath.Join(eng.folder, "gone")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := eng.reconcileAll(ctx); err != nil {
		t.Fatalf("bootstrap reconcileAll: %v", err)
	}
	if _, ok := eng.state.Entries["gone/a.txt"]; !ok {
		t.Fatal("gone/a.txt should have been uploaded during bootstrap")
	}

	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}

	if err := eng.reconcileLocalChanges(ctx, []string{"gone"}); err != nil {
		t.Fatalf("reconcileLocalChanges: %v", err)
	}

	for _, rel := range []string{"gone/a.txt", "gone/b.txt"} {
		if _, ok := eng.state.Entries[rel]; ok {
			t.Errorf("%s: want the index entry dropped after the remote delete, still present", rel)
		}
	}
	if eng.state.Folders["gone"] {
		t.Error(`state.Folders["gone"] still true, want it dropped once the folder itself was trashed`)
	}
}

// A transfer that fails leaves nothing for a later pass to notice: the file
// changes no further, so it appears in no later check-updates delta and in no
// later watcher event. The pass it failed in therefore has to leave the next one
// on the full key union, which is what retries it — otherwise it waits for the
// ~30 minute periodic backstop.
func TestFailedTransferIsRetriedByTheNextScopedPass(t *testing.T) {
	eng, fs := newLocalTreeCacheTestEngine(t)
	ctx := context.Background()

	fs.mu.Lock()
	fs.nodes["r1"] = &fakeNode{
		storageNode: storageNode{ID: "r1", ParentID: "root", Name: "a.txt", NodeType: "file", SizeBytes: 1},
		data:        []byte("A"),
	}
	fs.failNextDownload = 1
	fs.mu.Unlock()

	if err := eng.reconcileAll(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if _, err := os.Stat(filepath.Join(eng.folder, "a.txt")); err == nil {
		t.Fatal("the download should have failed")
	}

	// Nothing is reported as changed on either side, so scoping this pass would
	// have it check nothing at all.
	if err := eng.reconcileLocalChanges(ctx, nil); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(eng.folder, "a.txt"))
	if err != nil || string(data) != "A" {
		t.Errorf("a.txt = %q (err=%v), want it retried and downloaded", data, err)
	}
}
