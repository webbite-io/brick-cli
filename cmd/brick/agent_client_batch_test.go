package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newClientPair starts a source agent and a target agent whose Storage API
// proxies .../clients/src/* to the source, the way brick-api's client routes
// do, and serves everything else from a fake.
func newClientPair(t *testing.T) (src, dst *testAgent) {
	t.Helper()
	srcSC, srcFake := newTestTransferClient(t)
	src = newTestAgent(t, srcSC, srcFake, true)

	fake := newFakeStorageAPI()
	fakeMux := fake.mux("acct-1")
	const prefix = "/v1/accounts/acct-1/clients/src"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, prefix+"/") {
			fakeMux.ServeHTTP(w, r)
			return
		}
		target := src.url + strings.TrimPrefix(r.URL.Path, prefix)
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		req, _ := http.NewRequest(r.Method, target, r.Body)
		req.Header.Set(agentSecretHeader, testAgentSecret)
		req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(server.Close)
	dstSC := &storageClient{baseURL: server.URL, apiURL: server.URL, accountID: "acct-1", cfg: &Config{AccessToken: "test-token"}}
	dst = newTestAgent(t, dstSC, fake, true)
	return src, dst
}

func TestAgentClientBatchDryRun(t *testing.T) {
	src, dst := newClientPair(t)
	single := writeTestFile(t, src.root, "single.txt", "1")
	photos := filepath.Join(src.root, "Photos")
	writeTestFile(t, src.root, "Photos/a.jpg", "aa")
	writeTestFile(t, src.root, "Photos/2024/b.jpg", "bbb")
	writeTestFile(t, dst.root, "single.txt", "already here")

	var out downloadDryRunResp
	status := dst.do("POST", "/transfer/client-batch", map[string]interface{}{
		"sourceClientId": "src",
		"paths":          []string{single, photos, filepath.Join(src.root, "missing")},
		"localDir":       dst.root,
		"recursive":      true,
		"dryRun":         true,
	}, &out)
	if status != http.StatusOK || !out.DryRun {
		t.Fatalf("POST /transfer/client-batch dryRun: status %d, %+v", status, out)
	}
	if out.FileCount != 2 || out.TotalBytes != 5 {
		t.Errorf("dry run = %d files / %d bytes, want 2 / 5: %+v", out.FileCount, out.TotalBytes, out.Files)
	}
	want := map[string]string{"a.jpg": "Photos", "b.jpg": "Photos/2024"}
	for _, f := range out.Files {
		if want[f.Name] != f.Dir || f.LocalPath != filepath.Join(dst.root, filepath.FromSlash(f.Dir), f.Name) || f.SourcePath == "" {
			t.Errorf("file %+v, want it in %q under localDir, with its source path", f, want[f.Name])
		}
	}
	codes := map[string]string{}
	for _, s := range out.Skipped {
		codes[filepath.Base(s.Path)] = s.Code
	}
	// One the target already has, and one the source couldn't find.
	if codes["single.txt"] != "exists" || codes["missing"] != "not_found" {
		t.Errorf("skipped = %+v, want single.txt exists and missing not_found", out.Skipped)
	}
	if _, err := os.Stat(filepath.Join(dst.root, "Photos")); !os.IsNotExist(err) {
		t.Errorf("a dry run created Photos on disk (stat err %v), want nothing written", err)
	}
}

func TestAgentClientBatchCopiesTree(t *testing.T) {
	src, dst := newClientPair(t)
	writeTestFile(t, src.root, "Photos/a.jpg", "aa")
	writeTestFile(t, src.root, "Photos/2024/b.jpg", "bbb")

	var out batchResp
	status := dst.do("POST", "/transfer/client-batch", map[string]interface{}{
		"sourceClientId": "src",
		"paths":          []string{filepath.Join(src.root, "Photos")},
		"localDir":       dst.root,
		"recursive":      true,
	}, &out)
	if status != http.StatusAccepted {
		t.Fatalf("POST /transfer/client-batch: status %d, want 202", status)
	}
	if out.Batch.Direction != "copy" || out.Batch.SourceClientID != "src" || out.Batch.FileCount != 2 {
		t.Fatalf("batch = %+v, want a 2-file copy from src", out.Batch)
	}
	b := dst.waitForBatch(out.Batch.ID, func(b transferBatch) bool { return b.FinishedAt != nil })
	if b.State != transferCompleted || b.Completed != 2 || b.BytesDone != 5 {
		t.Errorf("finished batch = %+v, want 2 completed files and 5 bytes done", b)
	}
	for rel, data := range map[string]string{"Photos/a.jpg": "aa", "Photos/2024/b.jpg": "bbb"} {
		got, err := os.ReadFile(filepath.Join(dst.root, filepath.FromSlash(rel)))
		if err != nil || string(got) != data {
			t.Errorf("%s = %q (err %v), want %q", rel, got, err, data)
		}
	}
}

func TestAgentClientBatchRejects(t *testing.T) {
	src, dst := newClientPair(t)
	file := writeTestFile(t, src.root, "a.txt", "a")

	var errResp struct {
		Error apiError `json:"error"`
	}
	for _, tc := range []struct {
		name   string
		body   map[string]interface{}
		status int
		code   string
	}{
		{"no sourceClientId", map[string]interface{}{"paths": []string{file}, "localDir": dst.root}, http.StatusBadRequest, "invalid_request"},
		{"no paths", map[string]interface{}{"sourceClientId": "src", "localDir": dst.root}, http.StatusBadRequest, "invalid_request"},
		{"localDir outside the roots", map[string]interface{}{"sourceClientId": "src", "paths": []string{file}, "localDir": t.TempDir()}, http.StatusForbidden, "path_outside_roots"},
		// The proxy knows no client but src: its plain-text 404 is the one an
		// outdated source answers with.
		{"unknown source", map[string]interface{}{"sourceClientId": "other", "paths": []string{file}, "localDir": dst.root}, http.StatusBadGateway, "source_outdated"},
	} {
		errResp.Error = apiError{}
		status := dst.do("POST", "/transfer/client-batch", tc.body, &errResp)
		if status != tc.status || errResp.Error.Code != tc.code {
			t.Errorf("%s: status %d code %q, want %d %q", tc.name, status, errResp.Error.Code, tc.status, tc.code)
		}
	}
}

func TestAgentMoveCopyRefuseToOverwrite(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	a := writeTestFile(t, ta.root, "a.txt", "a")
	b := writeTestFile(t, ta.root, "b.txt", "b")
	dir := filepath.Join(ta.root, "dir")
	writeTestFile(t, ta.root, "dir/inner.txt", "i")

	var errResp struct {
		Error apiError `json:"error"`
	}
	for _, tc := range []struct {
		name, op string
		req      fromToReq
		status   int
		code     string
	}{
		{"move onto a file", "/fs/move", fromToReq{From: a, To: b}, http.StatusConflict, "exists"},
		{"copy onto a file", "/fs/copy", fromToReq{From: a, To: b}, http.StatusConflict, "exists"},
		{"copy onto itself", "/fs/copy", fromToReq{From: a, To: a}, http.StatusConflict, "exists"},
		{"copy a folder into itself", "/fs/copy", fromToReq{From: dir, To: filepath.Join(dir, "dir")}, http.StatusBadRequest, "invalid_destination"},
		{"move a folder into itself", "/fs/move", fromToReq{From: dir, To: filepath.Join(dir, "sub", "dir")}, http.StatusBadRequest, "invalid_destination"},
	} {
		errResp.Error = apiError{}
		if status := ta.do("POST", tc.op, tc.req, &errResp); status != tc.status || errResp.Error.Code != tc.code {
			t.Errorf("%s: status %d code %q, want %d %q", tc.name, status, errResp.Error.Code, tc.status, tc.code)
		}
	}
	if got, _ := os.ReadFile(b); string(got) != "b" {
		t.Errorf("b.txt = %q, want it untouched", got)
	}

	// A free destination still works, a folder's sibling with its name as a
	// prefix included.
	if status := ta.do("POST", "/fs/copy", fromToReq{From: dir, To: dir + "-copy"}, nil); status != http.StatusOK {
		t.Errorf("copy dir to dir-copy: status %d, want 200", status)
	}
	if status := ta.do("POST", "/fs/move", fromToReq{From: a, To: filepath.Join(dir, "a.txt")}, nil); status != http.StatusOK {
		t.Errorf("move a.txt into dir: status %d, want 200", status)
	}
}
