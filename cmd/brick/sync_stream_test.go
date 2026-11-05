package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// streamTestServer is a fake Storage API for the sync engine's transfers: one
// remote file, f1 ("a.txt"), under "root", plus POST /files and PUT /files/{id}
// that keep what they receive. fail503 makes that many file downloads answer
// 503, which a retrying transfer would retry.
type streamTestServer struct {
	mu        sync.Mutex
	content   string
	downloads int
	fail503   int
	received  map[string]string // node id → last uploaded content
}

func newStreamTestEngine(t *testing.T, srv *streamTestServer) *syncEngine {
	t.Helper()
	srv.received = map[string]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/accounts/acct-1/nodes/root/children", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, storageNodeList{Data: []storageNode{{ID: "f1", ParentID: "root", Name: "a.txt", NodeType: "file", SizeBytes: int64(len(srv.content)), Etag: "v1"}}, Count: 1})
	})
	mux.HandleFunc("/v1/accounts/acct-1/files", func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		srv.mu.Lock()
		srv.received["new"] = string(data)
		srv.mu.Unlock()
		writeJSON(w, http.StatusCreated, storageUploadResult{Node: storageNode{ID: "new", ParentID: r.Header.Get("X-Parent-ID"), Name: r.Header.Get("X-Filename"), NodeType: "file", Etag: "n1"}})
	})
	mux.HandleFunc("/v1/accounts/acct-1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/v1/accounts/acct-1/files/")
		srv.mu.Lock()
		defer srv.mu.Unlock()
		if r.Method == http.MethodPut {
			data, _ := io.ReadAll(r.Body)
			srv.received[id] = string(data)
			writeJSON(w, http.StatusOK, storageUploadResult{Node: storageNode{ID: id, NodeType: "file", Etag: "v2"}})
			return
		}
		srv.downloads++
		if srv.fail503 > 0 {
			srv.fail503--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		http.ServeContent(w, r, "", time.Time{}, strings.NewReader(srv.content))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	folder := t.TempDir()
	return &syncEngine{
		sc:              &storageClient{baseURL: server.URL, apiURL: server.URL, accountID: "acct-1", cfg: &Config{AccessToken: "test-token"}},
		folder:          folder,
		accountID:       "acct-1",
		rootID:          "root",
		conflictMode:    "device",
		state:           &SyncState{Folder: folder, Entries: map[string]SyncEntry{}, Folders: map[string]bool{}, FolderIDs: map[string]string{}},
		recentlyWritten: map[string]time.Time{},
	}
}

// tempFilesIn lists the in-progress download files left in folder.
func tempFilesIn(folder string) []string {
	var left []string
	_ = filepath.WalkDir(folder, func(path string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(d.Name(), tmpSuffix) {
			left = append(left, path)
		}
		return nil
	})
	return left
}

func syncTestNode(srv *streamTestServer) storageNode {
	return storageNode{ID: "f1", ParentID: "root", Name: "a.txt", NodeType: "file", SizeBytes: int64(len(srv.content)), Etag: "v1"}
}

// A download streams to disk and records the bytes it put there, the way the
// in-memory download it replaced did.
func TestSyncDownloadRecordsWhatItWrote(t *testing.T) {
	srv := &streamTestServer{content: "streamed content"}
	eng := newStreamTestEngine(t, srv)
	localFiles, localDirs := map[string]int64{}, map[string]bool{}
	if err := eng.downloadFile(context.Background(), "a.txt", syncTestNode(srv), localFiles, localDirs); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}

	assertFileContent(t, filepath.Join(eng.folder, "a.txt"), []byte("streamed content"))
	entry := eng.state.Entries["a.txt"]
	if entry.LocalHash != sha256Hex("streamed content") || entry.LocalSize != int64(len("streamed content")) || entry.RemoteEtag != "v1" {
		t.Errorf("entry = %+v, want the downloaded content's hash and size, and ETag v1", entry)
	}
	if localFiles["a.txt"] != int64(len("streamed content")) {
		t.Errorf("localFiles[a.txt] = %d, want the downloaded size", localFiles["a.txt"])
	}
}

// A download that fails, even with an error worth retrying, is not retried
// within the pass — the next pass retries it — and leaves nothing
// half-written behind.
func TestSyncDownloadFailureIsLeftForTheNextPass(t *testing.T) {
	fastRetries(t)
	srv := &streamTestServer{content: "A", fail503: 1}
	eng := newStreamTestEngine(t, srv)
	if err := eng.downloadFile(context.Background(), "a.txt", syncTestNode(srv), map[string]int64{}, map[string]bool{}); err == nil {
		t.Fatal("downloadFile succeeded, want the 503 to fail it")
	}
	if srv.downloads != 1 {
		t.Errorf("%d download requests, want 1", srv.downloads)
	}
	if _, err := os.Stat(filepath.Join(eng.folder, "a.txt")); err == nil {
		t.Error("a.txt exists after its download failed")
	}
	if left := tempFilesIn(eng.folder); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}

// An upload or replace records the hash of the bytes it sent.
func TestSyncUploadRecordsWhatItSent(t *testing.T) {
	srv := &streamTestServer{}
	eng := newStreamTestEngine(t, srv)
	if err := os.WriteFile(filepath.Join(eng.folder, "b.txt"), []byte("local content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := eng.uploadNewFile(context.Background(), "b.txt", map[string]storageNode{}, map[string]string{}); err != nil {
		t.Fatalf("uploadNewFile: %v", err)
	}
	if got := srv.received["new"]; got != "local content" {
		t.Errorf("server received %q, want the file's content", got)
	}
	if entry := eng.state.Entries["b.txt"]; entry.LocalHash != sha256Hex("local content") || entry.LocalSize != int64(len("local content")) {
		t.Errorf("entry = %+v, want the uploaded content's hash and size", entry)
	}

	if err := os.WriteFile(filepath.Join(eng.folder, "b.txt"), []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := eng.replaceFile(context.Background(), "b.txt", "new"); err != nil {
		t.Fatalf("replaceFile: %v", err)
	}
	if got := srv.received["new"]; got != "edited" {
		t.Errorf("server received %q on replace, want the edited content", got)
	}
	if entry := eng.state.Entries["b.txt"]; entry.LocalHash != sha256Hex("edited") || entry.RemoteEtag != "v2" {
		t.Errorf("entry after replace = %+v, want the edited content's hash and ETag v2", entry)
	}
}

// uploadBody sends only the size it was opened at, even if the file grows,
// and starts its hash over when a resent request reads it again.
func TestUploadBodyHashesWhatIsSent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, size, err := openUpload(path)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	if size != 5 {
		t.Fatalf("size %d, want 5", size)
	}
	if err := os.WriteFile(path, []byte("hello, world"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := io.CopyN(io.Discard, body, 3); err != nil { // a first attempt cut off part way
		t.Fatal(err)
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	sent, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(sent) != "hello" || body.n != 5 || body.sum() != sha256Hex("hello") {
		t.Errorf("sent %q (n=%d, sum %s), want the 5 bytes the file had when opened", sent, body.n, body.sum())
	}
	if _, err := body.Seek(2, io.SeekStart); err == nil {
		t.Error("seeking to the middle succeeded, want an error")
	}
}

func TestDownloadForSyncReportsWhatItWrote(t *testing.T) {
	fastRetries(t)
	_, sc := newFlakyFileServer(t, retryTestContent, "drop")
	dest := filepath.Join(t.TempDir(), "out.bin")
	got, err := sc.downloadForSync(context.Background(), "f1", dest, int64(len(retryTestContent)))
	if err != nil {
		t.Fatalf("downloadForSync: %v", err)
	}
	assertFileContent(t, dest, retryTestContent)
	if got.size != int64(len(retryTestContent)) || got.sha256 != sha256Hex(string(retryTestContent)) || got.etag != "v1" {
		t.Errorf("downloadForSync = %+v, want the whole resumed file's size and hash, and ETag v1", got)
	}
}
