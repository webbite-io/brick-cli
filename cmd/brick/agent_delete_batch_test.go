package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type deleteBatchResp struct {
	Deleted []string    `json:"deleted"`
	Skipped []batchSkip `json:"skipped"`
}

func TestAgentDeleteBatch(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ro := t.TempDir()
	rw := filepath.Join(ro, "inbox") // a writable root nested inside the read-only one
	if err := os.MkdirAll(rw, 0o755); err != nil {
		t.Fatal(err)
	}
	ta := newTestAgentWithRoots(t, sc, fake, true, []agentRoot{{path: ro, readOnly: true}, {path: rw}})
	a := writeTestFile(t, rw, "a.txt", "a")
	dir := filepath.Join(rw, "dir")
	inner := writeTestFile(t, rw, "dir/inner.txt", "i")
	keep := writeTestFile(t, ro, "keep.txt", "k")

	var out deleteBatchResp
	status := ta.do("POST", "/fs/delete-batch", map[string]interface{}{
		// inner is gone by the time its turn comes, which still counts.
		"paths": []string{a, dir, inner, keep, rw, t.TempDir()},
	}, &out)
	if status != http.StatusOK {
		t.Fatalf("POST /fs/delete-batch: status %d, want 200", status)
	}
	if len(out.Deleted) != 3 {
		t.Errorf("deleted = %v, want a.txt, dir and dir/inner.txt", out.Deleted)
	}
	codes := map[string]string{}
	for _, s := range out.Skipped {
		codes[s.Path] = s.Code
	}
	if len(out.Skipped) != 3 || codes[keep] != "read_only_root" || codes[rw] != "root_protected" {
		t.Errorf("skipped = %+v, want keep.txt read_only_root, the root root_protected and one outside the roots", out.Skipped)
	}
	for _, p := range []string{a, dir} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists (stat err %v)", p, err)
		}
	}
	for _, p := range []string{keep, rw} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}

func TestAgentDeleteBatchRejects(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	file := writeTestFile(t, ta.root, "a.txt", "a")
	tooMany := make([]string, maxBatchFiles+1)
	for i := range tooMany {
		tooMany[i] = file
	}

	var errResp struct {
		Error apiError `json:"error"`
	}
	for _, tc := range []struct {
		name string
		body interface{}
		code string
	}{
		{"no paths", map[string]interface{}{}, "invalid_request"},
		{"too many paths", map[string]interface{}{"paths": tooMany}, "too_many_files"},
	} {
		errResp.Error = apiError{}
		if status := ta.do("POST", "/fs/delete-batch", tc.body, &errResp); status != http.StatusBadRequest || errResp.Error.Code != tc.code {
			t.Errorf("%s: status %d code %q, want 400 %q", tc.name, status, errResp.Error.Code, tc.code)
		}
	}
	if _, err := os.Stat(file); err != nil {
		t.Errorf("a rejected batch removed %s: %v", file, err)
	}
}
