package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// newPauseTestEngine builds a syncEngine wired to a fake Storage API server
// serving a single flat folder of totalFiles remote-only files under rootID
// "root" (file0.txt..fileN.txt, node IDs f0..fN, content "content-f<i>").
// The GET /files/:id handler flips eng.paused the instant the pauseAfter'th
// download completes, the same way pressing 'P' would mid-batch, but
// deterministically instead of racing goroutine scheduling. Pass a
// pauseAfter of 0 (or higher than totalFiles) to never pause.
func newPauseTestEngine(t *testing.T, totalFiles, pauseAfter int) *syncEngine {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "brick"), 0o755); err != nil {
		t.Fatal(err)
	}
	syncFolder := t.TempDir()

	var downloadCount atomic.Int32
	var eng *syncEngine

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/accounts/acct-1/nodes/root/children", func(w http.ResponseWriter, r *http.Request) {
		nodes := make([]storageNode, totalFiles)
		for i := 0; i < totalFiles; i++ {
			id := fmt.Sprintf("f%d", i)
			nodes[i] = storageNode{ID: id, ParentID: "root", Name: fmt.Sprintf("file%d.txt", i), NodeType: "file", Etag: "etag-" + id}
		}
		writeJSON(w, http.StatusOK, storageNodeList{Data: nodes, Count: int64(totalFiles)})
	})
	mux.HandleFunc("/v1/accounts/acct-1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[len("/v1/accounts/acct-1/files/"):]
		n := downloadCount.Add(1)
		if pauseAfter > 0 && int(n) == pauseAfter {
			eng.setPaused(true)
		}
		w.Header().Set("ETag", `"etag-`+id+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("content-" + id))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	eng = &syncEngine{
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
	return eng
}

func TestCheckInterrupted(t *testing.T) {
	eng := &syncEngine{}

	if err := eng.checkInterrupted(context.Background()); err != nil {
		t.Errorf("neither cancelled nor paused: err = %v, want nil", err)
	}

	eng.setPaused(true)
	if err := eng.checkInterrupted(context.Background()); !errors.Is(err, errPausedMidPass) {
		t.Errorf("paused: err = %v, want errPausedMidPass", err)
	}
	eng.setPaused(false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := eng.checkInterrupted(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v, want context.Canceled", err)
	}

	// ctx cancellation takes priority over a pause flag: shutting down should
	// never be masked as "just paused".
	eng.setPaused(true)
	if err := eng.checkInterrupted(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled+paused: err = %v, want context.Canceled (not errPausedMidPass)", err)
	}
}

// Pressing pause mid-pass (here: after the 3rd of 10 downloads) must stop
// reconcileAll between files, not run the batch to completion, and must not
// undo or re-touch what already downloaded successfully.
func TestReconcileAllPauseMidPassStopsBetweenFiles(t *testing.T) {
	const totalFiles = 10
	const pauseAfter = 3

	eng := newPauseTestEngine(t, totalFiles, pauseAfter)

	err := eng.reconcileAll(context.Background())
	if !errors.Is(err, errPausedMidPass) {
		t.Fatalf("reconcileAll error = %v, want errPausedMidPass", err)
	}
	if got := eng.downloaded.Load(); got != pauseAfter {
		t.Errorf("downloaded = %d, want exactly %d (pass should stop the instant paused flips true)", got, pauseAfter)
	}
	if got := len(eng.state.Entries); got != pauseAfter {
		t.Errorf("len(state.Entries) = %d, want %d", got, pauseAfter)
	}

	// The pause must not be recorded as a sync failure: setPaused already
	// logged it, and /v1/status's "paused" overlay is the correct signal, not
	// a lingering lastError.
	eng.setPaused(false)
	st := eng.statusSnapshot()
	if st.LastError != "" {
		t.Errorf("LastError = %q, want empty (a pause is not an error)", st.LastError)
	}
	if st.State == "idle" {
		t.Error(`State = "idle", want anything but idle: the pass never actually completed`)
	}

	// firstSync must survive an aborted pass: files not yet processed still
	// need the onboarding conflict-resolution mode applied on the pass that
	// eventually does reach them (see reconcileFile's e.firstSync branch).
	if !eng.firstSync {
		t.Error("firstSync = false after an aborted pass, want true (only a completed pass may clear it)")
	}
}

// Resuming (setPaused(false), then re-running reconcileAll — exactly what the
// debounce worker's notify()-triggered retry does) must pick up every file
// the aborted pass skipped, converge to a full sync, and only then clear
// firstSync.
func TestReconcileAllResumeAfterPauseCompletesRemainingFiles(t *testing.T) {
	const totalFiles = 10
	const pauseAfter = 3

	eng := newPauseTestEngine(t, totalFiles, pauseAfter)

	if err := eng.reconcileAll(context.Background()); !errors.Is(err, errPausedMidPass) {
		t.Fatalf("first pass error = %v, want errPausedMidPass", err)
	}
	if !eng.firstSync {
		t.Fatal("firstSync = false after the aborted pass, want true")
	}

	eng.setPaused(false)
	if err := eng.reconcileAll(context.Background()); err != nil {
		t.Fatalf("resumed pass error = %v, want nil", err)
	}

	if got := len(eng.state.Entries); got != totalFiles {
		t.Errorf("len(state.Entries) after resume = %d, want %d (all remaining files picked up)", got, totalFiles)
	}
	if got := eng.downloaded.Load(); got != totalFiles {
		t.Errorf("downloaded after resume = %d, want %d", got, totalFiles)
	}
	if eng.firstSync {
		t.Error("firstSync = true after a fully completed pass, want false")
	}

	// Every file actually landed on disk with the right content.
	for i := 0; i < totalFiles; i++ {
		abs := filepath.Join(eng.folder, fmt.Sprintf("file%d.txt", i))
		data, err := os.ReadFile(abs)
		if err != nil {
			t.Errorf("file%d.txt: %v", i, err)
			continue
		}
		want := fmt.Sprintf("content-f%d", i)
		if string(data) != want {
			t.Errorf("file%d.txt content = %q, want %q", i, data, want)
		}
	}

	st := eng.statusSnapshot()
	if st.State != "idle" {
		t.Errorf("State after full completion = %q, want idle", st.State)
	}
	if st.LastError != "" {
		t.Errorf("LastError after full completion = %q, want empty", st.LastError)
	}
}

// A pass that is never paused must behave exactly as before: run to
// completion in one call and clear firstSync immediately.
func TestReconcileAllNoPauseCompletesInOnePass(t *testing.T) {
	const totalFiles = 5
	eng := newPauseTestEngine(t, totalFiles, 0) // pauseAfter=0: never pauses

	if err := eng.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcileAll error = %v, want nil", err)
	}
	if got := len(eng.state.Entries); got != totalFiles {
		t.Errorf("len(state.Entries) = %d, want %d", got, totalFiles)
	}
	if eng.firstSync {
		t.Error("firstSync = true after a completed pass, want false")
	}
}

// firstSync surviving an aborted pass (proven above) only matters if a
// still-true firstSync actually drives conflict resolution correctly once
// the conflicting file is reached. This locks in that half of the chain
// directly: with firstSync true and conflictMode "brick", a file that exists
// on both sides with no prior sync history uploads the local copy rather
// than falling through to ordinary remote-wins.
func TestReconcileAllHonorsConflictModeWhileFirstSyncTrue(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "brick"), 0o755); err != nil {
		t.Fatal(err)
	}
	syncFolder := t.TempDir()
	if err := os.WriteFile(filepath.Join(syncFolder, "conflict.txt"), []byte("local content"), 0o644); err != nil {
		t.Fatal(err)
	}

	var uploaded atomic.Bool
	var downloaded atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/accounts/acct-1/nodes/root/children", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, storageNodeList{
			Data:  []storageNode{{ID: "f-conflict", ParentID: "root", Name: "conflict.txt", NodeType: "file", Etag: "etag-remote"}},
			Count: 1,
		})
	})
	mux.HandleFunc("/v1/accounts/acct-1/files/f-conflict", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut: // replaceFile: conflictMode "brick" uploads local over remote
			uploaded.Store(true)
			writeJSON(w, http.StatusOK, storageUploadResult{Node: storageNode{ID: "f-conflict", Etag: "etag-new"}})
		case http.MethodGet: // would mean the conflict resolved the wrong way (remote wins)
			downloaded.Store(true)
			w.Header().Set("ETag", `"etag-remote"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("remote content"))
		default:
			http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

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
		conflictMode:    "brick",
		state:           &SyncState{Folder: syncFolder, Entries: map[string]SyncEntry{}, Folders: map[string]bool{}, FolderIDs: map[string]string{}},
		recentlyWritten: map[string]time.Time{},
	}

	if err := eng.reconcileAll(context.Background()); err != nil {
		t.Fatalf("reconcileAll error = %v, want nil", err)
	}
	if !uploaded.Load() {
		t.Error("local copy was not uploaded: conflictMode \"brick\" was not honored")
	}
	if downloaded.Load() {
		t.Error("remote copy was downloaded: fell through to remote-wins instead of honoring conflictMode \"brick\"")
	}
}
