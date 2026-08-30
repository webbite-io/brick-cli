package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// --- fake Storage API ---

// fakeNode is a fakeStorageAPI-internal node: the JSON-facing storageNode
// plus the raw bytes for a file.
type fakeNode struct {
	storageNode
	data []byte
}

// fakeStorageAPI is a minimal in-memory stand-in for the real Storage API,
// covering just enough of the surface transfer.go's new storageClient
// methods talk to: resolve, nodes (get/create/list-children), and files
// (create/replace/download). Good enough to exercise the CLI's transfer
// logic end-to-end without a live server.
type fakeStorageAPI struct {
	mu     sync.Mutex
	nodes  map[string]*fakeNode
	nextID int
}

func newFakeStorageAPI() *fakeStorageAPI {
	fs := &fakeStorageAPI{nodes: map[string]*fakeNode{}}
	fs.nodes["root"] = &fakeNode{storageNode: storageNode{ID: "root", NodeType: "root"}}
	return fs
}

func (fs *fakeStorageAPI) newID() string {
	fs.nextID++
	return fmt.Sprintf("n%d", fs.nextID)
}

// children must be called with fs.mu held.
func (fs *fakeStorageAPI) children(parentID string) []storageNode {
	var out []storageNode
	for _, n := range fs.nodes {
		if n.ParentID == parentID {
			out = append(out, n.storageNode)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// resolvePath must be called with fs.mu held.
func (fs *fakeStorageAPI) resolvePath(path string) (*storageNode, bool) {
	cur := fs.nodes["root"]
	for _, seg := range strings.FieldsFunc(path, func(r rune) bool { return r == '/' }) {
		found := (*fakeNode)(nil)
		for _, n := range fs.nodes {
			if n.ParentID == cur.ID && n.Name == seg {
				found = n
				break
			}
		}
		if found == nil {
			return nil, false
		}
		cur = found
	}
	node := cur.storageNode
	return &node, true
}

func (fs *fakeStorageAPI) mux(accountID string) http.Handler {
	base := "/v1/accounts/" + accountID
	mux := http.NewServeMux()

	mux.HandleFunc(base+"/resolve", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		node, ok := fs.resolvePath(r.URL.Query().Get("path"))
		fs.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusOK, node)
	})

	mux.HandleFunc(base+"/nodes", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ParentID string `json:"parentId"`
			Name     string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		fs.mu.Lock()
		defer fs.mu.Unlock()
		for _, n := range fs.nodes {
			if n.ParentID == body.ParentID && n.Name == body.Name && n.NodeType == "folder" {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "conflict"})
				return
			}
		}
		id := fs.newID()
		node := storageNode{ID: id, ParentID: body.ParentID, Name: body.Name, NodeType: "folder"}
		fs.nodes[id] = &fakeNode{storageNode: node}
		writeJSON(w, http.StatusCreated, node)
	})

	mux.HandleFunc(base+"/nodes/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, base+"/nodes/")
		if strings.HasSuffix(rest, "/children") {
			id := strings.TrimSuffix(rest, "/children")
			fs.mu.Lock()
			kids := fs.children(id)
			fs.mu.Unlock()
			writeJSON(w, http.StatusOK, storageNodeList{Data: kids, Count: int64(len(kids))})
			return
		}
		fs.mu.Lock()
		n, ok := fs.nodes[rest]
		fs.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusOK, n.storageNode)
	})

	mux.HandleFunc(base+"/files", func(w http.ResponseWriter, r *http.Request) {
		parentID := r.Header.Get("X-Parent-ID")
		name := r.Header.Get("X-Filename")
		data, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		fs.mu.Lock()
		id := fs.newID()
		node := storageNode{ID: id, ParentID: parentID, Name: name, NodeType: "file", SizeBytes: int64(len(data))}
		fs.nodes[id] = &fakeNode{storageNode: node, data: data}
		fs.mu.Unlock()
		writeJSON(w, http.StatusCreated, storageUploadResult{Node: node})
	})

	mux.HandleFunc(base+"/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, base+"/files/")
		fs.mu.Lock()
		defer fs.mu.Unlock()
		n, ok := fs.nodes[id]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(n.data)
		case http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			n.data = data
			n.SizeBytes = int64(len(data))
			writeJSON(w, http.StatusOK, storageUploadResult{Node: n.storageNode})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	return mux
}

func newTestTransferClient(t *testing.T) (*storageClient, *fakeStorageAPI) {
	t.Helper()
	fs := newFakeStorageAPI()
	server := httptest.NewServer(fs.mux("acct-1"))
	t.Cleanup(server.Close)
	sc := &storageClient{
		baseURL:   server.URL,
		apiURL:    server.URL,
		accountID: "acct-1",
		cfg:       &Config{AccessToken: "test-token"},
	}
	return sc, fs
}

// --- tests ---

func TestGetNodeAndResolvePath(t *testing.T) {
	sc, fs := newTestTransferClient(t)
	ctx := context.Background()

	fs.mu.Lock()
	fs.nodes["f1"] = &fakeNode{storageNode: storageNode{ID: "f1", ParentID: "root", Name: "report.pdf", NodeType: "file", SizeBytes: 42}}
	fs.mu.Unlock()

	node, err := sc.getNode(ctx, "f1")
	if err != nil {
		t.Fatalf("getNode: %v", err)
	}
	if node.Name != "report.pdf" || node.SizeBytes != 42 {
		t.Errorf("getNode returned %+v", node)
	}

	if _, err := sc.getNode(ctx, "missing"); err == nil {
		t.Error("getNode(missing): want error, got nil")
	}

	node, err = sc.resolvePath(ctx, "/report.pdf")
	if err != nil {
		t.Fatalf("resolvePath: %v", err)
	}
	if node.ID != "f1" {
		t.Errorf("resolvePath id = %q, want f1", node.ID)
	}

	if _, err := sc.resolvePath(ctx, "/nope"); err == nil {
		t.Error("resolvePath(/nope): want error, got nil")
	}
}

func TestRemoteFolderCacheEnsure(t *testing.T) {
	sc, fs := newTestTransferClient(t)
	ctx := context.Background()
	cache := newRemoteFolderCache(ctx, sc, "root")

	id, err := cache.ensure("a/b/c")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	fs.mu.Lock()
	n := len(fs.nodes)
	fs.mu.Unlock()
	if n != 4 { // root + a + b + c
		t.Errorf("created %d nodes, want 4 (root, a, b, c)", n)
	}

	// Re-ensuring the same path must not create duplicates, from the local
	// cache alone.
	id2, err := cache.ensure("a/b/c")
	if err != nil {
		t.Fatalf("ensure (again): %v", err)
	}
	if id != id2 {
		t.Errorf("ensure not idempotent: %q != %q", id, id2)
	}

	// A *fresh* cache against the same server must reuse the folders
	// createFolder already created (via its 409-then-reuse behavior), not
	// duplicate them.
	cache2 := newRemoteFolderCache(ctx, sc, "root")
	id3, err := cache2.ensure("a/b/c")
	if err != nil {
		t.Fatalf("ensure (fresh cache): %v", err)
	}
	if id3 != id {
		t.Errorf("fresh cache created a new folder instead of reusing: %q != %q", id3, id)
	}
	fs.mu.Lock()
	n = len(fs.nodes)
	fs.mu.Unlock()
	if n != 4 {
		t.Errorf("fresh cache created extra nodes: now %d, want 4", n)
	}
}

func TestDownloadToFileAndUploadStreamRoundTrip(t *testing.T) {
	sc, _ := newTestTransferClient(t)
	ctx := context.Background()
	content := []byte(strings.Repeat("hello brick ", 1000))

	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var progressCalls []int64
	node, err := sc.uploadStream(ctx, "root", "src.bin", f, int64(len(content)), func(written int64) {
		progressCalls = append(progressCalls, written)
	})
	if err != nil {
		t.Fatalf("uploadStream: %v", err)
	}
	if node.SizeBytes != int64(len(content)) {
		t.Errorf("uploaded size = %d, want %d", node.SizeBytes, len(content))
	}
	if len(progressCalls) == 0 || progressCalls[len(progressCalls)-1] != int64(len(content)) {
		t.Errorf("progress calls = %v, want to end at %d", progressCalls, len(content))
	}

	dest := filepath.Join(dir, "dst.bin")
	var dlProgress []int64
	if err := sc.downloadToFile(ctx, node.ID, dest, node.SizeBytes, func(written, total int64) {
		dlProgress = append(dlProgress, written)
		if total != int64(len(content)) {
			t.Errorf("downloadToFile total = %d, want %d", total, len(content))
		}
	}); err != nil {
		t.Fatalf("downloadToFile: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Error("downloaded content does not match uploaded content")
	}
	if _, err := os.Stat(dest + tmpSuffix); !os.IsNotExist(err) {
		t.Errorf("temp file %s should not exist after a successful download", dest+tmpSuffix)
	}
	if len(dlProgress) == 0 || dlProgress[len(dlProgress)-1] != int64(len(content)) {
		t.Errorf("download progress calls = %v, want to end at %d", dlProgress, len(content))
	}

	// replaceStream overwrites the same node rather than creating a new one.
	newContent := []byte("replaced content")
	rf, err := os.CreateTemp(dir, "replace-*")
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()
	if _, err := rf.Write(newContent); err != nil {
		t.Fatal(err)
	}
	if _, err := rf.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	replaced, err := sc.replaceStream(ctx, node.ID, rf, int64(len(newContent)), nil)
	if err != nil {
		t.Fatalf("replaceStream: %v", err)
	}
	if replaced.ID != node.ID {
		t.Errorf("replaceStream returned a different node id: %q != %q", replaced.ID, node.ID)
	}
	if replaced.SizeBytes != int64(len(newContent)) {
		t.Errorf("replaced size = %d, want %d", replaced.SizeBytes, len(newContent))
	}
}

func TestWalkLocalTree(t *testing.T) {
	dir := t.TempDir()
	mustWrite := func(rel string, content string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("a.txt", "1234")
	mustWrite("sub/b.txt", "12345678")
	mustWrite("sub/deeper/c.txt", "12")

	files, folderCount, totalSize, err := walkLocalTree(dir)
	if err != nil {
		t.Fatal(err)
	}
	if folderCount != 2 {
		t.Errorf("folderCount = %d, want 2", folderCount)
	}
	if totalSize != 4+8+2 {
		t.Errorf("totalSize = %d, want 14", totalSize)
	}
	rels := make([]string, len(files))
	for i, f := range files {
		rels[i] = f.relPath
	}
	sort.Strings(rels)
	want := []string{"a.txt", "sub/b.txt", "sub/deeper/c.txt"}
	if len(rels) != len(want) {
		t.Fatalf("relPaths = %v, want %v", rels, want)
	}
	for i := range want {
		if rels[i] != want[i] {
			t.Errorf("relPaths = %v, want %v", rels, want)
			break
		}
	}
}

func TestRunUploadRunDownloadRoundTrip(t *testing.T) {
	sc, _ := newTestTransferClient(t)
	ctx := context.Background()

	srcDir := t.TempDir()
	files := map[string]string{
		"a.txt":          "top level file",
		"sub/b.txt":      "nested file",
		"sub/deep/c.txt": "deeply nested file",
	}
	for rel, content := range files {
		full := filepath.Join(srcDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := runUpload(ctx, sc, srcDir, "/backup", transferOptions{recursive: true, silent: true}); err != nil {
		t.Fatalf("runUpload: %v", err)
	}

	// Uploading a folder without -r must be a no-op-with-error, not a
	// partial upload.
	if err := runUpload(ctx, sc, srcDir, "/backup2", transferOptions{}); err == nil {
		t.Error("runUpload without -r on a folder: want error, got nil")
	}

	dstDir := t.TempDir()
	if err := runDownload(ctx, sc, "/backup", dstDir, transferOptions{recursive: true, silent: true}); err != nil {
		t.Fatalf("runDownload: %v", err)
	}

	for rel, want := range files {
		got, err := os.ReadFile(filepath.Join(dstDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("reading downloaded %s: %v", rel, err)
		}
		if string(got) != want {
			t.Errorf("%s content = %q, want %q", rel, got, want)
		}
	}
}

func TestRunUploadOverwrite(t *testing.T) {
	sc, fs := newTestTransferClient(t)
	ctx := context.Background()

	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runUpload(ctx, sc, path, "", transferOptions{silent: true}); err != nil {
		t.Fatalf("first upload: %v", err)
	}

	if err := os.WriteFile(path, []byte("v2-longer-content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runUpload(ctx, sc, path, "", transferOptions{silent: true, overwrite: true}); err != nil {
		t.Fatalf("second upload (--overwrite): %v", err)
	}

	fs.mu.Lock()
	var matches []*fakeNode
	for _, n := range fs.nodes {
		if n.Name == "note.txt" {
			matches = append(matches, n)
		}
	}
	fs.mu.Unlock()
	if len(matches) != 1 {
		t.Fatalf("found %d nodes named note.txt, want 1 (overwrite should replace, not duplicate)", len(matches))
	}
	if string(matches[0].data) != "v2-longer-content" {
		t.Errorf("content = %q, want %q", matches[0].data, "v2-longer-content")
	}

	// Without --overwrite, a same-named upload must not touch the existing
	// node (our fake server doesn't implement the real API's auto-suffix
	// rename, so this just verifies the client didn't call replace).
	if err := os.WriteFile(path, []byte("v3-should-not-apply"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runUpload(ctx, sc, path, "", transferOptions{silent: true}); err != nil {
		t.Fatalf("third upload (no --overwrite): %v", err)
	}
	fs.mu.Lock()
	if string(matches[0].data) != "v2-longer-content" {
		t.Errorf("existing node's content changed without --overwrite: %q", matches[0].data)
	}
	fs.mu.Unlock()
}
