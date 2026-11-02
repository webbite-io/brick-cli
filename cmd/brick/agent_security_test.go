package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// symlinkOrSkip makes link point at target, skipping the test where symlinks
// can't be made (e.g. Windows without the privilege).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

func TestEvalSafeResolvesThroughMissingLevels(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside, filepath.Join(root, "real")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	symlinkOrSkip(t, outside, filepath.Join(root, "link"))
	symlinkOrSkip(t, filepath.Join(base, "nowhere", "x"), filepath.Join(root, "dangling"))
	if err := os.WriteFile(filepath.Join(root, "file.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ in, want string }{
		{filepath.Join(root, "real"), filepath.Join(root, "real")},
		{filepath.Join(root, "real", "a", "b", "c"), filepath.Join(root, "real", "a", "b", "c")},
		{filepath.Join(root, "link"), outside},
		{filepath.Join(root, "link", "f"), filepath.Join(outside, "f")},
		{filepath.Join(root, "link", "new", "f"), filepath.Join(outside, "new", "f")},
		{filepath.Join(root, "link", "a", "b", "c", "d"), filepath.Join(outside, "a", "b", "c", "d")},
		// A dangling symlink as the leaf is named by its folder, so it can
		// be deleted or replaced; further up, nothing it leads to is known.
		{filepath.Join(root, "dangling"), filepath.Join(root, "dangling")},
		{filepath.Join(root, "dangling", "new", "f"), ""},
		// A file in the middle of the path: the request fails later, as a
		// normal I/O error, not as a path outside the roots.
		{filepath.Join(root, "file.txt", "x", "y"), filepath.Join(root, "file.txt", "x", "y")},
	} {
		if got := evalSafe(tc.in); got != tc.want {
			t.Errorf("evalSafe(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestAgentRefusesWritesThroughSymlinkOutOfRoots(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	symlinkOrSkip(t, outside, filepath.Join(root, "link"))
	ta := newTestAgentWithRoots(t, sc, fake, true, []agentRoot{{path: root}})
	addFakeFile(fake, "f1", "remote.txt", []byte("remote"))
	src := writeTestFile(t, root, "src.txt", "src")

	deep := filepath.Join(root, "link", "new", "deeper")
	q := func(p string) string { return url.QueryEscape(p) }
	for _, tc := range []struct {
		name, method, path string
		body               interface{}
	}{
		{"write", "PUT", "/fs/write?path=" + q(filepath.Join(deep, "f.txt")), nil},
		{"mkdir", "POST", "/fs/mkdir?path=" + q(deep), nil},
		{"move", "POST", "/fs/move", fromToReq{From: src, To: filepath.Join(deep, "moved.txt")}},
		{"copy", "POST", "/fs/copy", fromToReq{From: src, To: filepath.Join(deep, "copied.txt")}},
		{"download", "POST", "/transfer/download", downloadReq{NodeID: "f1", LocalPath: filepath.Join(deep, "remote.txt")}},
		{"download-batch", "POST", "/transfer/download-batch", downloadBatchReq{NodeIDs: []string{"f1"}, LocalDir: deep}},
	} {
		var errResp struct {
			Error apiError `json:"error"`
		}
		if status := ta.do(tc.method, tc.path, tc.body, &errResp); status != http.StatusForbidden || errResp.Error.Code != "path_outside_roots" {
			t.Errorf("%s: status %d code %q, want 403 path_outside_roots", tc.name, status, errResp.Error.Code)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("something was written outside the roots: %v", entries)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("src.txt was moved away: %v", err)
	}
}

func TestAgentRefusesWritesThroughSymlinkIntoReadOnlyRoot(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	base := t.TempDir()
	rw := filepath.Join(base, "rw")
	ro := filepath.Join(base, "ro")
	for _, d := range []string{rw, ro} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	symlinkOrSkip(t, ro, filepath.Join(rw, "link"))
	ta := newTestAgentWithRoots(t, sc, fake, true, []agentRoot{{path: rw}, {path: ro, readOnly: true}})

	var errResp struct {
		Error apiError `json:"error"`
	}
	p := filepath.Join(rw, "link", "new", "f.txt")
	if status := ta.do("PUT", "/fs/write?path="+url.QueryEscape(p), nil, &errResp); status != http.StatusForbidden || errResp.Error.Code != "read_only_root" {
		t.Errorf("write: status %d code %q, want 403 read_only_root", status, errResp.Error.Code)
	}
	if entries, _ := os.ReadDir(ro); len(entries) != 0 {
		t.Errorf("something was written into the read-only root: %v", entries)
	}
}

func TestAgentDeletesDanglingSymlink(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	link := filepath.Join(ta.root, "dangling")
	symlinkOrSkip(t, filepath.Join(ta.root, "gone"), link)
	if status := ta.do("DELETE", "/fs/delete?path="+url.QueryEscape(link), nil, nil); status != http.StatusOK {
		t.Fatalf("delete dangling symlink: status %d, want 200", status)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("dangling symlink still there: %v", err)
	}
}

func TestAgentCopySkipsSymlinksOutOfRoots(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	base := t.TempDir()
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := writeTestFile(t, outside, "secret.txt", "secret")
	inside := writeTestFile(t, root, "inside.txt", "inside")
	writeTestFile(t, root, "src/plain.txt", "plain")
	src := filepath.Join(root, "src")
	symlinkOrSkip(t, secret, filepath.Join(src, "to-secret"))
	symlinkOrSkip(t, inside, filepath.Join(src, "to-inside"))
	symlinkOrSkip(t, outside, filepath.Join(src, "to-dir"))
	symlinkOrSkip(t, filepath.Join(base, "gone"), filepath.Join(src, "dangling"))
	ta := newTestAgentWithRoots(t, sc, fake, true, []agentRoot{{path: root}})

	dst := filepath.Join(root, "dst")
	var out struct {
		Skipped []batchSkip `json:"skipped"`
	}
	if status := ta.do("POST", "/fs/copy", fromToReq{From: src, To: dst}, &out); status != http.StatusOK {
		t.Fatalf("copy: status %d, want 200", status)
	}
	codes := map[string]string{}
	for _, s := range out.Skipped {
		codes[filepath.Base(s.Path)] = s.Code
	}
	want := map[string]string{"to-secret": "path_outside_roots", "to-dir": "symlinked_directory", "dangling": "not_found"}
	for name, code := range want {
		if codes[name] != code {
			t.Errorf("skipped[%s] = %q, want %q (all: %v)", name, codes[name], code, codes)
		}
	}
	if len(codes) != len(want) {
		t.Errorf("skipped = %v, want exactly %v", codes, want)
	}
	if _, err := os.Lstat(filepath.Join(dst, "to-secret")); !os.IsNotExist(err) {
		t.Errorf("symlink out of the roots was copied: %v", err)
	}
	for name, data := range map[string]string{"plain.txt": "plain", "to-inside": "inside"} {
		p := filepath.Join(dst, name)
		got, err := os.ReadFile(p)
		if err != nil || string(got) != data {
			t.Errorf("%s = %q, %v; want %q", name, got, err, data)
		}
		if info, err := os.Lstat(p); err == nil && !info.Mode().IsRegular() {
			t.Errorf("%s copied as %v, want a regular file", name, info.Mode())
		}
	}
}

func TestAgentCopyFollowsSymlinkedStartingFolder(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	writeTestFile(t, ta.root, "real/a.txt", "a")
	link := filepath.Join(ta.root, "link")
	symlinkOrSkip(t, filepath.Join(ta.root, "real"), link)
	if status := ta.do("POST", "/fs/copy", fromToReq{From: link, To: filepath.Join(ta.root, "copy")}, nil); status != http.StatusOK {
		t.Fatalf("copy: status %d, want 200", status)
	}
	if got, err := os.ReadFile(filepath.Join(ta.root, "copy", "a.txt")); err != nil || string(got) != "a" {
		t.Errorf("copy/a.txt = %q, %v; want \"a\"", got, err)
	}
}

func TestAgentCopyOntoSameFileUnderOtherCase(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	a := writeTestFile(t, ta.root, "a.txt", "data")
	upper := filepath.Join(ta.root, "A.txt")
	if _, err := os.Stat(upper); err != nil {
		t.Skip("disk is case-sensitive")
	}
	var errResp struct {
		Error apiError `json:"error"`
	}
	if status := ta.do("POST", "/fs/copy", fromToReq{From: a, To: upper}, &errResp); status != http.StatusConflict || errResp.Error.Code != "exists" {
		t.Errorf("copy a.txt to A.txt: status %d code %q, want 409 exists", status, errResp.Error.Code)
	}
	if got, _ := os.ReadFile(a); string(got) != "data" {
		t.Errorf("a.txt = %q, want it untouched", got)
	}
}

func TestAgentRefusesToDeleteOrMoveFolderHoldingRoot(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	rw := t.TempDir()
	ro := filepath.Join(rw, "a", "docs") // read-only, nested in the writable root
	writeTestFile(t, ro, "keep.txt", "keep")
	writeTestFile(t, rw, "a/other.txt", "other")
	writeTestFile(t, rw, "ab/x.txt", "x") // shares a's name as a prefix
	ta := newTestAgentWithRoots(t, sc, fake, true, []agentRoot{{path: rw}, {path: ro, readOnly: true}})
	a := filepath.Join(rw, "a")

	var errResp struct {
		Error apiError `json:"error"`
	}
	for _, tc := range []struct {
		name, method, path string
		body               interface{}
	}{
		{"delete", "DELETE", "/fs/delete?path=" + url.QueryEscape(a), nil},
		{"move", "POST", "/fs/move", fromToReq{From: a, To: filepath.Join(rw, "moved")}},
	} {
		errResp.Error = apiError{}
		if status := ta.do(tc.method, tc.path, tc.body, &errResp); status != http.StatusForbidden || errResp.Error.Code != "root_protected" {
			t.Errorf("%s: status %d code %q, want 403 root_protected", tc.name, status, errResp.Error.Code)
		}
	}
	var batch struct {
		Deleted []string    `json:"deleted"`
		Skipped []batchSkip `json:"skipped"`
	}
	other := filepath.Join(a, "other.txt")
	ab := filepath.Join(rw, "ab")
	if status := ta.do("POST", "/fs/delete-batch", map[string]interface{}{"paths": []string{a, other, ab}}, &batch); status != http.StatusOK {
		t.Fatalf("delete-batch: status %d", status)
	}
	if len(batch.Skipped) != 1 || batch.Skipped[0].Path != a || batch.Skipped[0].Code != "root_protected" {
		t.Errorf("skipped = %+v, want only %s as root_protected", batch.Skipped, a)
	}
	if len(batch.Deleted) != 2 {
		t.Errorf("deleted = %v, want a/other.txt and ab", batch.Deleted)
	}
	if got, _ := os.ReadFile(filepath.Join(ro, "keep.txt")); string(got) != "keep" {
		t.Errorf("file in the read-only root = %q, want it kept", got)
	}
}

func TestCreateExclusiveDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	target := writeTestFile(t, dir, "target.txt", "precious")
	p := filepath.Join(dir, "file.brick-tmp")
	symlinkOrSkip(t, target, p)

	f, err := createExclusive(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("new"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, _ := os.ReadFile(target); string(got) != "precious" {
		t.Errorf("symlink target = %q, want it untouched", got)
	}
	if info, err := os.Lstat(p); err != nil || !info.Mode().IsRegular() {
		t.Errorf("%s = %v, %v; want a new regular file", p, info, err)
	}

	// A leftover regular file is replaced too.
	f, err = createExclusive(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if got, _ := os.ReadFile(p); len(got) != 0 {
		t.Errorf("leftover = %q, want it emptied", got)
	}
}

// A file that appears where a batch download is going, while it downloads,
// is kept, and the download fails.
func TestAgentDownloadBatchKeepsFileThatAppearedMeanwhile(t *testing.T) {
	fake := newFakeStorageAPI()
	release := make(chan struct{})
	ta := newTestAgent(t, gatedDownloadServer(t, fake, release), fake, true)
	addFakeFile(fake, "f1", "report.txt", []byte("0123456789"))

	var out batchResp
	if status := ta.do("POST", "/transfer/download-batch", downloadBatchReq{NodeIDs: []string{"f1"}, LocalDir: ta.root}, &out); status != http.StatusAccepted {
		t.Fatalf("download-batch: status %d, want 202", status)
	}
	id := out.Batch.TransferIDs[0]
	ta.waitForTransfer(id, func(rec transferRecord) bool { return rec.BytesDone > 0 })
	dest := writeTestFile(t, ta.root, "report.txt", "mine")
	close(release)

	rec := ta.waitForTransfer(id, func(rec transferRecord) bool { return rec.State != transferRunning })
	if rec.State != transferFailed {
		t.Errorf("transfer = %s, want failed", rec.State)
	}
	if got, _ := os.ReadFile(dest); string(got) != "mine" {
		t.Errorf("report.txt = %q, want the file that appeared kept", got)
	}
	if entries, _ := os.ReadDir(ta.root); len(entries) != 1 {
		t.Errorf("root holds %v, want only report.txt (no temporary file left)", entries)
	}
}

func TestPlaceNoClobber(t *testing.T) {
	dir := t.TempDir()
	tmp := writeTestFile(t, dir, "a.brick-tmp", "new")
	dest := filepath.Join(dir, "a")
	if err := placeNoClobber(tmp, dest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "new" {
		t.Errorf("dest = %q, want new", got)
	}
	if _, err := os.Lstat(tmp); !os.IsNotExist(err) {
		t.Errorf("tmp still there: %v", err)
	}

	tmp = writeTestFile(t, dir, "a.brick-tmp", "newer")
	if err := placeNoClobber(tmp, dest); !errors.Is(err, errDestExists) {
		t.Errorf("onto an existing file: err %v, want errDestExists", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "new" {
		t.Errorf("dest = %q, want it kept", got)
	}
}

func TestAgentRejectsWrongSecret(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	for _, secret := range []string{"", "wrong", testAgentSecret[:len(testAgentSecret)-1], testAgentSecret + "x", testAgentSecret} {
		req, err := http.NewRequest("GET", ta.url+"/version", nil)
		if err != nil {
			t.Fatal(err)
		}
		if secret != "" {
			req.Header.Set(agentSecretHeader, secret)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		want := http.StatusForbidden
		if secret == testAgentSecret {
			want = http.StatusOK
		}
		if resp.StatusCode != want {
			t.Errorf("secret %q: status %d, want %d", secret, resp.StatusCode, want)
		}
	}
}

// Ids from a remote control request are escaped into the Storage API path, so
// one can't name a different endpoint.
func TestStorageClientEscapesIDs(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.EscapedPath()+"?"+r.URL.RawQuery)
		http.Error(w, "no", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	sc := &storageClient{baseURL: server.URL, apiURL: server.URL, accountID: "acct-1", cfg: &Config{AccessToken: "t"}}
	ctx := context.Background()
	const id = "../../other?x=1"
	_, _ = sc.getNode(ctx, id)
	_ = sc.downloadToFile(ctx, id, filepath.Join(t.TempDir(), "f"), 1, nil)
	_, _ = sc.listChildren(ctx, id)
	want := []string{
		"/v1/accounts/acct-1/nodes/..%2F..%2Fother%3Fx=1?",
		"/v1/accounts/acct-1/files/..%2F..%2Fother%3Fx=1?",
		"/v1/accounts/acct-1/nodes/..%2F..%2Fother%3Fx=1/children?limit=200&offset=0",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("requested %q, want %q", paths, want)
	}
}
