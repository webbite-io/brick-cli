package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fastRetries shortens the retry timing for the duration of the test.
func fastRetries(t *testing.T) {
	t.Helper()
	oldMin, oldMax, oldWindow, oldStall := transferRetryMin, transferRetryMax, transferRetryWindow, transferStallTimeout
	transferRetryMin, transferRetryMax, transferRetryWindow, transferStallTimeout = time.Millisecond, 5*time.Millisecond, 5*time.Second, 200*time.Millisecond
	t.Cleanup(func() {
		transferRetryMin, transferRetryMax, transferRetryWindow, transferStallTimeout = oldMin, oldMax, oldWindow, oldStall
	})
}

// flakyFileServer serves one file, f1, the way the Storage API does (ETag,
// Range, If-Range), except that each request takes the next step in steps
// first. Steps:
//
//   - "503", "500": answer with that status
//   - "drop": send the first half of the file, then cut the connection
//   - "drop-change": as "drop", then replace the file's content
//   - "stall": send the first half, then send nothing more
type flakyFileServer struct {
	mu       sync.Mutex
	content  []byte
	etag     string
	steps    []string
	requests []*http.Request
	stalled  chan struct{} // closed at cleanup, releasing stalled handlers
}

func newFlakyFileServer(t *testing.T, content []byte, steps ...string) (*flakyFileServer, *storageClient) {
	t.Helper()
	fs := &flakyFileServer{content: content, etag: `"v1"`, steps: steps, stalled: make(chan struct{})}
	server := httptest.NewServer(fs)
	t.Cleanup(func() {
		close(fs.stalled)
		server.Close()
	})
	return fs, &storageClient{baseURL: server.URL, apiURL: server.URL, accountID: "acct-1", cfg: &Config{AccessToken: "test-token"}}
}

func (fs *flakyFileServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	fs.mu.Lock()
	fs.requests = append(fs.requests, r.Clone(context.Background()))
	step := ""
	if len(fs.steps) > 0 {
		step, fs.steps = fs.steps[0], fs.steps[1:]
	}
	content, etag := fs.content, fs.etag
	if step == "drop-change" {
		fs.content, fs.etag = []byte("completely different content"), `"v2"`
	}
	fs.mu.Unlock()

	switch step {
	case "503":
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	case "500":
		w.WriteHeader(http.StatusInternalServerError)
		return
	case "drop", "drop-change", "stall":
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content[:len(content)/2])
		w.(http.Flusher).Flush()
		if step == "stall" {
			select {
			case <-r.Context().Done():
			case <-fs.stalled:
			}
			return
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
		return
	}
	w.Header().Set("ETag", etag)
	http.ServeContent(w, r, "f1", time.Time{}, bytes.NewReader(content))
}

func (fs *flakyFileServer) requestCount() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.requests)
}

func (fs *flakyFileServer) request(i int) *http.Request {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.requests[i]
}

var retryTestContent = []byte("0123456789abcdefghijklmnopqrstuvwxyz")

func downloadTo(t *testing.T, ctx context.Context, sc *storageClient, total int64) (string, error) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "out.bin")
	return dest, sc.downloadToFile(ctx, "f1", dest, total, nil)
}

func assertFileContent(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
	if _, err := os.Stat(path + tmpSuffix); !os.IsNotExist(err) {
		t.Errorf("temp file %s left behind (stat err %v)", path+tmpSuffix, err)
	}
}

func TestDownloadResumesAfterDroppedConnection(t *testing.T) {
	fastRetries(t)
	fs, sc := newFlakyFileServer(t, retryTestContent, "drop")
	dest, err := downloadTo(t, context.Background(), sc, int64(len(retryTestContent)))
	if err != nil {
		t.Fatalf("downloadToFile: %v", err)
	}
	assertFileContent(t, dest, retryTestContent)
	if n := fs.requestCount(); n != 2 {
		t.Fatalf("server saw %d requests, want 2", n)
	}
	second := fs.request(1)
	wantRange := "bytes=" + strconv.Itoa(len(retryTestContent)/2) + "-"
	if got := second.Header.Get("Range"); got != wantRange {
		t.Errorf("resumed request Range = %q, want %q", got, wantRange)
	}
	if got := second.Header.Get("If-Range"); got != `"v1"` {
		t.Errorf("resumed request If-Range = %q, want the first response's ETag", got)
	}
}

func TestDownloadStartsOverWhenFileChanged(t *testing.T) {
	fastRetries(t)
	_, sc := newFlakyFileServer(t, retryTestContent, "drop-change")
	dest, err := downloadTo(t, context.Background(), sc, 0)
	if err != nil {
		t.Fatalf("downloadToFile: %v", err)
	}
	assertFileContent(t, dest, []byte("completely different content"))
}

func TestDownloadResumesAfterStall(t *testing.T) {
	fastRetries(t)
	fs, sc := newFlakyFileServer(t, retryTestContent, "stall")
	dest, err := downloadTo(t, context.Background(), sc, int64(len(retryTestContent)))
	if err != nil {
		t.Fatalf("downloadToFile: %v", err)
	}
	assertFileContent(t, dest, retryTestContent)
	if got := fs.request(1).Header.Get("Range"); got == "" {
		t.Error("the request after the stall had no Range header, want it to resume")
	}
}

func TestDownloadRetriesWhileServerUnavailable(t *testing.T) {
	fastRetries(t)
	fs, sc := newFlakyFileServer(t, retryTestContent, "503", "503")
	var retries []error
	ctx := withTransferRetryObserver(context.Background(), func(err error, _ time.Duration) {
		retries = append(retries, err)
	})
	dest, err := downloadTo(t, ctx, sc, int64(len(retryTestContent)))
	if err != nil {
		t.Fatalf("downloadToFile: %v", err)
	}
	assertFileContent(t, dest, retryTestContent)
	if len(retries) != 2 || fs.requestCount() != 3 {
		t.Errorf("observer saw %d retries over %d requests, want 2 over 3", len(retries), fs.requestCount())
	}
}

func TestDownloadNotRetriedOnServerError(t *testing.T) {
	fastRetries(t)
	fs, sc := newFlakyFileServer(t, retryTestContent, "500")
	dest, err := downloadTo(t, context.Background(), sc, int64(len(retryTestContent)))
	if err == nil {
		t.Fatal("downloadToFile succeeded, want the 500 to fail it")
	}
	if n := fs.requestCount(); n != 1 {
		t.Errorf("server saw %d requests, want 1", n)
	}
	if _, err := os.Stat(dest + tmpSuffix); !os.IsNotExist(err) {
		t.Errorf("temp file left behind (stat err %v)", err)
	}
}

func TestDownloadGivesUpAfterRetryWindow(t *testing.T) {
	fastRetries(t)
	transferRetryWindow = 30 * time.Millisecond
	steps := make([]string, 1000)
	for i := range steps {
		steps[i] = "503"
	}
	_, sc := newFlakyFileServer(t, retryTestContent, steps...)
	dest, err := downloadTo(t, context.Background(), sc, int64(len(retryTestContent)))
	if err == nil || !strings.Contains(err.Error(), "gave up") {
		t.Fatalf("downloadToFile = %v, want it to give up", err)
	}
	if _, err := os.Stat(dest + tmpSuffix); !os.IsNotExist(err) {
		t.Errorf("temp file left behind (stat err %v)", err)
	}
}

func TestDownloadStopsRetryingWhenCanceled(t *testing.T) {
	fastRetries(t)
	transferRetryMin, transferRetryMax = time.Hour, time.Hour
	_, sc := newFlakyFileServer(t, retryTestContent, "503")
	ctx, cancel := context.WithCancel(context.Background())
	ctx = withTransferRetryObserver(ctx, func(error, time.Duration) { cancel() })
	done := make(chan error, 1)
	go func() {
		_, err := downloadTo(t, ctx, sc, int64(len(retryTestContent)))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("downloadToFile succeeded, want the cancellation to end it")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("downloadToFile kept waiting to retry after its context was canceled")
	}
}

func TestUploadRetriedWhileServerUnavailable(t *testing.T) {
	fastRetries(t)
	fake := newFakeStorageAPI()
	inner := fake.mux("acct-1")
	var mu sync.Mutex
	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/accounts/acct-1/files" {
			mu.Lock()
			posts++
			first := posts == 1
			mu.Unlock()
			if first {
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	sc := &storageClient{baseURL: server.URL, apiURL: server.URL, accountID: "acct-1", cfg: &Config{AccessToken: "test-token"}}

	node, err := sc.uploadStream(context.Background(), "root", "a.bin", bytes.NewReader(retryTestContent), int64(len(retryTestContent)), nil)
	if err != nil {
		t.Fatalf("uploadStream: %v", err)
	}
	if posts != 2 {
		t.Errorf("server saw %d uploads, want 2", posts)
	}
	fake.mu.Lock()
	got := fake.nodes[node.ID].data
	fake.mu.Unlock()
	if !bytes.Equal(got, retryTestContent) {
		t.Errorf("uploaded content = %q, want %q", got, retryTestContent)
	}
}

func TestIsTransient(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"503", &storageStatusError{code: 503}, true},
		{"502", &storageStatusError{code: 502}, true},
		{"500", &storageStatusError{code: 500}, false},
		{"404", &storageStatusError{code: 404}, false},
		{"stalled", errTransferStalled, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"session expired", errSessionExpired, false},
		{"local disk", &os.PathError{Op: "write", Path: "/x", Err: errors.New("no space left on device")}, false},
		{"marked", &transientError{errors.New("x")}, true},
	} {
		if got := isTransient(tc.err); got != tc.want {
			t.Errorf("isTransient(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSourceReadErrorRetriesWhileSourceReconnects(t *testing.T) {
	answer := func(status int, body string) *http.Response {
		return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(body))}
	}
	if err := sourceReadError(answer(http.StatusConflict, `{"error":{"code":"conflict","message":"Client is not currently connected"}}`)); !isTransient(err) {
		t.Errorf("409 from the Storage API: %v, want it retried", err)
	}
	if err := sourceReadError(answer(http.StatusServiceUnavailable, `{"error":{"code":"service_unavailable","message":"Client did not respond"}}`)); !isTransient(err) {
		t.Errorf("503 from the Storage API: %v, want it retried", err)
	}
	if err := sourceReadError(answer(http.StatusNotFound, `{"error":{"code":"not_found","message":"No such file."}}`)); isTransient(err) {
		t.Errorf("404 from the source: %v, want it not retried", err)
	}
}

func TestAgentReadServesRanges(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	path := filepath.Join(ta.root, "a.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	get := func(headers map[string]string) (int, string, http.Header) {
		req, _ := http.NewRequest("GET", ta.url+"/fs/read?path="+path, nil)
		req.Header.Set(agentSecretHeader, testAgentSecret)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body), resp.Header
	}

	status, body, hdr := get(nil)
	if status != http.StatusOK || body != "hello" || hdr.Get("Accept-Ranges") != "bytes" || hdr.Get("ETag") == "" {
		t.Fatalf("GET /fs/read = %d %q (Accept-Ranges %q, ETag %q), want 200 hello with ranges and an ETag",
			status, body, hdr.Get("Accept-Ranges"), hdr.Get("ETag"))
	}
	if status, body, _ := get(map[string]string{"Range": "bytes=2-", "If-Range": hdr.Get("ETag")}); status != http.StatusPartialContent || body != "llo" {
		t.Errorf("GET /fs/read with a matching If-Range = %d %q, want 206 llo", status, body)
	}
	if status, body, _ := get(map[string]string{"Range": "bytes=2-", "If-Range": `"stale"`}); status != http.StatusOK || body != "hello" {
		t.Errorf("GET /fs/read with a stale If-Range = %d %q, want 200 hello", status, body)
	}
}

func TestAgentDownloadRecordsRetries(t *testing.T) {
	fastRetries(t)
	fake := newFakeStorageAPI()
	addFakeFile(fake, "f1", "a.txt", []byte("hello"))
	inner := fake.mux("acct-1")
	var mu sync.Mutex
	failed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fail := !failed && r.Method == http.MethodGet && r.URL.Path == "/v1/accounts/acct-1/files/f1"
		failed = failed || fail
		mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	sc := &storageClient{baseURL: server.URL, apiURL: server.URL, accountID: "acct-1", cfg: &Config{AccessToken: "test-token"}}
	ta := newTestAgent(t, sc, fake, true)

	transferID := uuid.NewString()
	if status := ta.do("POST", "/transfer/download", map[string]string{
		"nodeId": "f1", "localPath": filepath.Join(ta.root, "a.txt"), "transferId": transferID,
	}, nil); status != http.StatusOK {
		t.Fatalf("POST /transfer/download: status %d, want 200", status)
	}
	rec := ta.transfer(transferID)
	if rec.State != transferCompleted || rec.Retries != 1 || rec.RetryError != "" {
		t.Errorf("transfer record = %+v, want completed after 1 retry, with retryError cleared", rec)
	}
	assertFileContent(t, filepath.Join(ta.root, "a.txt"), []byte("hello"))
}
