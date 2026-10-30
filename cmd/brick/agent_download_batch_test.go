package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

type downloadDryRunResp struct {
	DryRun     bool                `json:"dryRun"`
	FileCount  int                 `json:"fileCount"`
	TotalBytes int64               `json:"totalBytes"`
	Files      []downloadBatchFile `json:"files"`
	Skipped    []batchSkip         `json:"skipped"`
}

// addFakeNode puts a node holding data in the fake storage API.
func addFakeNode(fake *fakeStorageAPI, id, parentID, name, nodeType, data string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.nodes[id] = &fakeNode{
		storageNode: storageNode{ID: id, ParentID: parentID, Name: name, NodeType: nodeType, SizeBytes: int64(len(data))},
		data:        []byte(data),
	}
}

// addFakeTree gives the fake storage API single.txt and Photos/{a.jpg,2024/b.jpg}.
func addFakeTree(fake *fakeStorageAPI) {
	addFakeNode(fake, "single", "root", "single.txt", "file", "1")
	addFakeNode(fake, "photos", "root", "Photos", "folder", "")
	addFakeNode(fake, "a", "photos", "a.jpg", "file", "aa")
	addFakeNode(fake, "year", "photos", "2024", "folder", "")
	addFakeNode(fake, "b", "year", "b.jpg", "file", "bbb")
}

func TestAgentDownloadBatchDryRun(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	addFakeTree(fake)
	addFakeNode(fake, "bad", "root", "..", "file", "x")
	writeTestFile(t, ta.root, "single.txt", "already here")

	var out downloadDryRunResp
	status := ta.do("POST", "/transfer/download-batch", map[string]interface{}{
		"nodeIds":   []string{"single", "photos", "a", "bad", "missing"},
		"localDir":  ta.root,
		"recursive": true,
		"dryRun":    true,
	}, &out)
	if status != http.StatusOK || !out.DryRun {
		t.Fatalf("POST /transfer/download-batch dryRun: status %d, %+v", status, out)
	}
	if out.FileCount != 2 || out.TotalBytes != 5 {
		t.Errorf("dry run = %d files / %d bytes, want 2 / 5: %+v", out.FileCount, out.TotalBytes, out.Files)
	}
	want := map[string]string{"a.jpg": "Photos", "b.jpg": "Photos/2024"}
	for _, f := range out.Files {
		if want[f.Name] != f.Dir || f.LocalPath != filepath.Join(ta.root, filepath.FromSlash(f.Dir), f.Name) {
			t.Errorf("file %+v, want it in %q under localDir", f, want[f.Name])
		}
	}
	codes := skipCodes(out.Skipped)
	for name, code := range map[string]string{
		"single.txt": "exists",         // already on disk
		"a.jpg":      "duplicate",      // picked on its own and inside Photos
		"..":         "invalid_name",   // would leave localDir
		"missing":    "upstream_error", // no such node
	} {
		if codes[name] != code {
			t.Errorf("skip for %s = %q, want %q (all: %+v)", name, codes[name], code, out.Skipped)
		}
	}

	// Without recursive a folder is skipped rather than walked.
	ta.do("POST", "/transfer/download-batch", map[string]interface{}{
		"nodeIds": []string{"photos"}, "dryRun": true,
	}, &out)
	if out.FileCount != 0 || len(out.Skipped) != 1 || out.Skipped[0].Code != "is_a_directory" {
		t.Errorf("non-recursive folder: %+v, want it skipped as is_a_directory", out)
	}

	if _, err := os.Stat(filepath.Join(ta.root, "Photos")); !os.IsNotExist(err) {
		t.Errorf("a dry run created Photos on disk (stat err %v), want nothing written", err)
	}
}

func TestAgentDownloadBatchDownloadsTree(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	addFakeTree(fake)
	// Photos already exists locally, so it's reused rather than failing.
	writeTestFile(t, ta.root, "Photos/old.jpg", "old")

	var out batchResp
	status := ta.do("POST", "/transfer/download-batch", map[string]interface{}{
		"nodeIds":   []string{"single", "photos"},
		"localDir":  ta.root,
		"recursive": true,
	}, &out)
	if status != http.StatusAccepted {
		t.Fatalf("POST /transfer/download-batch: status %d, want 202", status)
	}
	if out.Batch.Direction != "download" || out.Batch.LocalDir != ta.root || out.Batch.FileCount != 3 || out.Batch.TotalBytes != 6 {
		t.Fatalf("batch = %+v, want a 3-file / 6-byte download into %s", out.Batch, ta.root)
	}

	b := ta.waitForBatch(out.Batch.ID, func(b transferBatch) bool { return b.FinishedAt != nil })
	if b.State != transferCompleted || b.Completed != 3 || b.BytesDone != 6 {
		t.Errorf("finished batch = %+v, want 3 completed files and 6 bytes done", b)
	}
	for rel, data := range map[string]string{
		"single.txt":        "1",
		"Photos/a.jpg":      "aa",
		"Photos/2024/b.jpg": "bbb",
		"Photos/old.jpg":    "old",
	} {
		got, err := os.ReadFile(filepath.Join(ta.root, filepath.FromSlash(rel)))
		if err != nil || string(got) != data {
			t.Errorf("%s = %q (err %v), want %q", rel, got, err, data)
		}
	}
}

func TestAgentDownloadBatchRejects(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ro, rw := t.TempDir(), t.TempDir()
	ta := newTestAgentWithRoots(t, sc, fake, true, []agentRoot{{path: ro, readOnly: true}, {path: rw}})
	addFakeTree(fake)
	writeTestFile(t, rw, "single.txt", "already here")

	var errResp struct {
		Error apiError `json:"error"`
	}
	for _, tc := range []struct {
		name string
		body map[string]interface{}
		code string
	}{
		{"read-only localDir", map[string]interface{}{"nodeIds": []string{"single"}, "localDir": ro}, "read_only_root"},
		{"read-only localDir, dry run", map[string]interface{}{"nodeIds": []string{"single"}, "localDir": ro, "dryRun": true}, "read_only_root"},
		{"localDir outside the roots", map[string]interface{}{"nodeIds": []string{"single"}, "localDir": t.TempDir()}, "path_outside_roots"},
		{"no localDir", map[string]interface{}{"nodeIds": []string{"single"}}, "invalid_request"},
		{"no nodeIds", map[string]interface{}{"localDir": rw}, "invalid_request"},
		{"nothing left to download", map[string]interface{}{"nodeIds": []string{"single"}, "localDir": rw}, "no_files"},
	} {
		errResp.Error = apiError{}
		status := ta.do("POST", "/transfer/download-batch", tc.body, &errResp)
		if status < 400 || errResp.Error.Code != tc.code {
			t.Errorf("%s: status %d code %q, want %q", tc.name, status, errResp.Error.Code, tc.code)
		}
	}
}
