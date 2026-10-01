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

// newRemoteTreeCacheTestEngine builds a syncEngine wired to a fake Storage
// API tracking how many times GET .../children and /check-updates are hit,
// with checkUpdatesResp controlling each /check-updates response in turn
// (the last entry repeats once exhausted).
func newRemoteTreeCacheTestEngine(t *testing.T, checkUpdatesResp func(call int) (data []storageNode, serverTime int64)) (*syncEngine, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "brick"), 0o755); err != nil {
		t.Fatal(err)
	}
	syncFolder := t.TempDir()

	var childrenCalls, checkUpdatesCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/accounts/acct-1/nodes/root/children", func(w http.ResponseWriter, r *http.Request) {
		childrenCalls.Add(1)
		writeJSON(w, http.StatusOK, storageNodeList{Data: nil, Count: 0})
	})
	mux.HandleFunc("/v1/accounts/acct-1/check-updates", func(w http.ResponseWriter, r *http.Request) {
		n := int(checkUpdatesCalls.Add(1))
		data, serverTime := checkUpdatesResp(n)
		writeUpdatesPage(t, w, data, serverTime, "")
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
		state:           &SyncState{Folder: syncFolder, Entries: map[string]SyncEntry{}, Folders: map[string]bool{}, FolderIDs: map[string]string{}},
		recentlyWritten: map[string]time.Time{},
	}
	return eng, &childrenCalls, &checkUpdatesCalls
}

// A reconcileAll pass with nothing to do (the common case for one triggered
// purely by a local filesystem change) must not re-walk the remote tree once
// a cheap /check-updates probe confirms nothing changed since the last walk
// — this is the fix for the reported "every local file add triggers a full
// GET /children on every folder" behavior.
func TestReconcileAllReusesRemoteTreeCacheWhenNothingChanged(t *testing.T) {
	eng, childrenCalls, checkUpdatesCalls := newRemoteTreeCacheTestEngine(t, func(call int) ([]storageNode, int64) {
		return nil, int64(1000 + call) // always reports "nothing changed"
	})

	if err := eng.reconcileAll(context.Background()); err != nil {
		t.Fatalf("first reconcileAll error = %v", err)
	}
	if got := childrenCalls.Load(); got != 1 {
		t.Fatalf("after first reconcileAll, children calls = %d, want 1", got)
	}

	if err := eng.reconcileAll(context.Background()); err != nil {
		t.Fatalf("second reconcileAll error = %v", err)
	}
	if got := childrenCalls.Load(); got != 1 {
		t.Errorf("after second reconcileAll with nothing changed, children calls = %d, want still 1 (the cache should have been reused)", got)
	}
	if got := checkUpdatesCalls.Load(); got < 2 {
		t.Errorf("check-updates calls = %d, want at least 2 (one seeding the cache, one confirming it's still fresh)", got)
	}
}

// The moment /check-updates reports an actual change that fits within a
// single checkUpdatesDelta page, the next reconcileAll pass must patch the
// cached tree directly (applyRemoteDelta) rather than falling back to a real
// GET .../children walk of every folder — that's the whole point of
// checkUpdatesDelta existing.
func TestReconcileAllPatchesRemoteTreeFromDeltaInsteadOfWalking(t *testing.T) {
	eng, childrenCalls, _ := newRemoteTreeCacheTestEngine(t, func(call int) ([]storageNode, int64) {
		if call == 1 {
			return nil, 1000 // seeds the cache during the first pass
		}
		// A new file lands remotely, reported with its full path the way a
		// real /check-updates response would.
		return []storageNode{{ID: "n1", NodeType: "file", Path: "/newfile.txt", Etag: "e1"}}, 2000
	})

	if err := eng.reconcileAll(context.Background()); err != nil {
		t.Fatalf("first reconcileAll error = %v", err)
	}
	if got := childrenCalls.Load(); got != 1 {
		t.Fatalf("after first reconcileAll, children calls = %d, want 1", got)
	}

	if err := eng.reconcileAll(context.Background()); err != nil {
		t.Fatalf("second reconcileAll error = %v", err)
	}
	if got := childrenCalls.Load(); got != 1 {
		t.Errorf("after second reconcileAll with a small reported change, children calls = %d, want still 1 (patched from the delta, not a fresh walk)", got)
	}
	node, ok := eng.remoteTreeFiles["newfile.txt"]
	if !ok || node.ID != "n1" {
		t.Errorf("remoteTreeFiles[%q] = %+v, %v, want the delta's node to have been patched in", "newfile.txt", node, ok)
	}
}

// A change too big to fit checkUpdatesDelta's page budget must fall back to a
// real remote tree walk rather than risk patching a partial delta.
func TestReconcileAllFallsBackToWalkWhenDeltaTooLarge(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "brick"), 0o755); err != nil {
		t.Fatal(err)
	}
	syncFolder := t.TempDir()

	var childrenCalls, checkUpdatesCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/accounts/acct-1/nodes/root/children", func(w http.ResponseWriter, r *http.Request) {
		childrenCalls.Add(1)
		writeJSON(w, http.StatusOK, storageNodeList{Data: nil, Count: 0})
	})
	mux.HandleFunc("/v1/accounts/acct-1/check-updates", func(w http.ResponseWriter, r *http.Request) {
		n := checkUpdatesCalls.Add(1)
		if n == 1 {
			// Seeds the cache during the first pass.
			writeUpdatesPage(t, w, nil, 1000, "")
			return
		}
		// Every page beyond checkUpdatesDeltaMaxPages carries a cursor, so
		// checkUpdatesDelta gives up on collecting the whole delta.
		writeUpdatesPage(t, w, []storageNode{{ID: "n1", NodeType: "file", Path: "/f.txt"}}, 2000, "next")
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
		state:           &SyncState{Folder: syncFolder, Entries: map[string]SyncEntry{}, Folders: map[string]bool{}, FolderIDs: map[string]string{}},
		recentlyWritten: map[string]time.Time{},
	}

	if err := eng.reconcileAll(context.Background()); err != nil {
		t.Fatalf("first reconcileAll error = %v", err)
	}
	if got := childrenCalls.Load(); got != 1 {
		t.Fatalf("after first reconcileAll, children calls = %d, want 1", got)
	}

	if err := eng.reconcileAll(context.Background()); err != nil {
		t.Fatalf("second reconcileAll error = %v", err)
	}
	if got := childrenCalls.Load(); got != 2 {
		t.Errorf("after second reconcileAll with an unbounded delta, children calls = %d, want 2 (a fresh walk, too big a backlog to patch)", got)
	}
}

// forceReconcile is the periodic backstop specifically for hard deletes that
// /check-updates can never report — it must always perform a real remote
// tree walk, never satisfied by remoteTreeCache, even when a check-updates
// probe would otherwise say the cache is still fresh.
func TestForceReconcileAlwaysWalksRemoteTreeEvenWhenCacheIsFresh(t *testing.T) {
	eng, childrenCalls, _ := newRemoteTreeCacheTestEngine(t, func(call int) ([]storageNode, int64) {
		return nil, int64(1000 + call) // always reports "nothing changed"
	})

	if err := eng.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcileAll error = %v", err)
	}
	if got := childrenCalls.Load(); got != 1 {
		t.Fatalf("after reconcileAll, children calls = %d, want 1", got)
	}

	if err := eng.forceReconcile(context.Background()); err != nil {
		t.Fatalf("forceReconcile error = %v", err)
	}
	if got := childrenCalls.Load(); got != 2 {
		t.Errorf("after forceReconcile, children calls = %d, want 2 (it must bypass the cache and walk for real)", got)
	}
}
