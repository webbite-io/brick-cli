package main

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
