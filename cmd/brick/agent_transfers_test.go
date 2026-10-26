package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const testAgentSecret = "test-secret"

// testAgent is an agentServer served over httptest, backed by a
// fakeStorageAPI, with one temp dir as its only allowed root.
type testAgent struct {
	t      *testing.T
	url    string
	root   string
	fake   *fakeStorageAPI
	cancel context.CancelFunc
}

func newTestAgent(t *testing.T, sc *storageClient, fake *fakeStorageAPI, remoteControl bool) *testAgent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	root := t.TempDir()
	a := newAgentServer(ctx, []string{root}, testAgentSecret, sc, remoteControl, 0)
	server := httptest.NewServer(a.handler())
	t.Cleanup(server.Close)
	return &testAgent{t: t, url: server.URL, root: root, fake: fake, cancel: cancel}
}

// do sends a request carrying the agent secret and decodes the JSON
// response into out (if non-nil), returning the status code.
func (ta *testAgent) do(method, path string, body interface{}, out interface{}) int {
	ta.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			ta.t.Fatal(err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, ta.url+path, r)
	if err != nil {
		ta.t.Fatal(err)
	}
	req.Header.Set(agentSecretHeader, testAgentSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		ta.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			ta.t.Fatalf("%s %s: decode response: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func (ta *testAgent) transfer(id string) transferRecord {
	ta.t.Helper()
	var out struct {
		Transfer transferRecord `json:"transfer"`
	}
	if status := ta.do("GET", "/transfers/"+id, nil, &out); status != http.StatusOK {
		ta.t.Fatalf("GET /transfers/%s: status %d", id, status)
	}
	return out.Transfer
}

// waitForTransfer polls GET /transfers/{id} until cond holds, the way Brick
// HQ is expected to.
func (ta *testAgent) waitForTransfer(id string, cond func(transferRecord) bool) transferRecord {
	ta.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec := ta.transfer(id)
		if cond(rec) {
			return rec
		}
		if time.Now().After(deadline) {
			ta.t.Fatalf("transfer %s never reached the expected state; last: %+v", id, rec)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// addFakeFile seeds the fake Storage API with a file node.
func addFakeFile(fake *fakeStorageAPI, id, name string, data []byte) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.nodes[id] = &fakeNode{
		storageNode: storageNode{ID: id, ParentID: "root", Name: name, NodeType: "file", SizeBytes: int64(len(data))},
		data:        data,
	}
}

func TestAgentVersionAvailableWithoutRemoteControl(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, false)

	var version map[string]interface{}
	if status := ta.do("GET", "/version", nil, &version); status != http.StatusOK {
		t.Fatalf("GET /version: status %d, want 200", status)
	}
	if version["version"] != Version || version["remoteControl"] != false {
		t.Errorf("GET /version = %v, want version %q and remoteControl false", version, Version)
	}

	var errResp struct {
		Error apiError `json:"error"`
	}
	if status := ta.do("GET", "/fs/roots", nil, &errResp); status != http.StatusForbidden || errResp.Error.Code != "remote_control_disabled" {
		t.Errorf("GET /fs/roots with remote control off: status %d code %q, want 403 remote_control_disabled", status, errResp.Error.Code)
	}

	resp, err := http.Get(ta.url + "/version")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /version without the agent secret: status %d, want 403", resp.StatusCode)
	}
}

func TestAgentUploadStreamsAndRecordsTransfer(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)

	data := bytes.Repeat([]byte("brick"), 100_000)
	local := filepath.Join(ta.root, "big.bin")
	if err := os.WriteFile(local, data, 0o644); err != nil {
		t.Fatal(err)
	}

	transferID := uuid.NewString()
	var out struct {
		Node     storageNode    `json:"node"`
		Transfer transferRecord `json:"transfer"`
	}
	status := ta.do("POST", "/transfer/upload", map[string]string{
		"localPath": local, "parentId": "root", "transferId": transferID,
	}, &out)
	if status != http.StatusOK {
		t.Fatalf("POST /transfer/upload: status %d, want 200", status)
	}
	if out.Node.Name != "big.bin" || out.Node.SizeBytes != int64(len(data)) {
		t.Errorf("uploaded node = %+v, want big.bin of %d bytes", out.Node, len(data))
	}
	fake.mu.Lock()
	uploaded := fake.nodes[out.Node.ID].data
	fake.mu.Unlock()
	if !bytes.Equal(uploaded, data) {
		t.Errorf("Storage API received %d bytes, want the %d-byte file", len(uploaded), len(data))
	}

	rec := ta.transfer(transferID)
	if rec.State != transferCompleted || rec.Direction != "upload" || rec.BytesDone != int64(len(data)) || rec.BytesTotal != int64(len(data)) {
		t.Errorf("transfer record = %+v, want a completed upload of %d bytes", rec, len(data))
	}
	if rec.Node == nil || rec.Node.ID != out.Node.ID || rec.FinishedAt == nil {
		t.Errorf("transfer record = %+v, want it to carry the uploaded node and a finish time", rec)
	}

	if status := ta.do("POST", "/transfer/upload", map[string]string{
		"localPath": local, "parentId": "root", "transferId": transferID,
	}, nil); status != http.StatusConflict {
		t.Errorf("reusing a transferId: status %d, want 409", status)
	}
	if status := ta.do("POST", "/transfer/upload", map[string]string{
		"localPath": ta.root, "parentId": "root",
	}, nil); status != http.StatusBadRequest {
		t.Errorf("uploading a directory: status %d, want 400", status)
	}
}

func TestAgentDownloadStreamsToFile(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)

	data := bytes.Repeat([]byte("0123456789"), 50_000)
	addFakeFile(fake, "f1", "report.pdf", data)

	local := filepath.Join(ta.root, "nested", "dir", "report.pdf")
	var out struct {
		Path     string         `json:"path"`
		Transfer transferRecord `json:"transfer"`
	}
	if status := ta.do("POST", "/transfer/download", map[string]string{"nodeId": "f1", "localPath": local}, &out); status != http.StatusOK {
		t.Fatalf("POST /transfer/download: status %d, want 200", status)
	}
	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("downloaded %d bytes, want %d", len(got), len(data))
	}
	if _, err := os.Stat(local + tmpSuffix); !os.IsNotExist(err) {
		t.Errorf("temp file %s left behind (stat err %v)", local+tmpSuffix, err)
	}
	if rec := out.Transfer; rec.State != transferCompleted || rec.Direction != "download" || rec.NodeID != "f1" || rec.BytesDone != int64(len(data)) {
		t.Errorf("transfer record = %+v, want a completed download of f1", rec)
	}
}

func TestAgentDownloadFailureIsRecorded(t *testing.T) {
	sc, fake := newTestTransferClient(t)
	ta := newTestAgent(t, sc, fake, true)
	addFakeFile(fake, "f1", "a.txt", []byte("hello"))
	fake.failNextDownload = 1

	transferID := uuid.NewString()
	if status := ta.do("POST", "/transfer/download", map[string]string{
		"nodeId": "f1", "localPath": filepath.Join(ta.root, "a.txt"), "transferId": transferID,
	}, nil); status != http.StatusBadGateway {
		t.Fatalf("POST /transfer/download: status %d, want 502", status)
	}
	if rec := ta.transfer(transferID); rec.State != transferFailed || rec.Error == "" {
		t.Errorf("transfer record = %+v, want failed with an error", rec)
	}
}

// gatedDownloadServer serves the fake Storage API, except that file
// downloads send their first half, then wait for release before sending the
// rest — so a test can observe a transfer while it's in flight.
func gatedDownloadServer(t *testing.T, fake *fakeStorageAPI, release <-chan struct{}) *storageClient {
	t.Helper()
	inner := fake.mux("acct-1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/v1/accounts/acct-1/files/"
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, prefix) {
			inner.ServeHTTP(w, r)
			return
		}
		fake.mu.Lock()
		data := fake.nodes[strings.TrimPrefix(r.URL.Path, prefix)].data
		fake.mu.Unlock()
		half := len(data) / 2
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data[:half])
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = w.Write(data[half:])
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	return &storageClient{baseURL: server.URL, apiURL: server.URL, accountID: "acct-1", cfg: &Config{AccessToken: "test-token"}}
}

func TestAgentAsyncDownloadReportsProgress(t *testing.T) {
	fake := newFakeStorageAPI()
	release := make(chan struct{})
	ta := newTestAgent(t, gatedDownloadServer(t, fake, release), fake, true)

	data := bytes.Repeat([]byte("x"), 4096)
	addFakeFile(fake, "f1", "x.bin", data)
	local := filepath.Join(ta.root, "x.bin")

	var accepted struct {
		Transfer transferRecord `json:"transfer"`
	}
	if status := ta.do("POST", "/transfer/download", map[string]interface{}{
		"nodeId": "f1", "localPath": local, "async": true,
	}, &accepted); status != http.StatusAccepted {
		t.Fatalf("async POST /transfer/download: status %d, want 202", status)
	}
	id := accepted.Transfer.ID
	if id == "" || accepted.Transfer.BytesTotal != int64(len(data)) {
		t.Fatalf("202 response transfer = %+v, want an id and bytesTotal %d", accepted.Transfer, len(data))
	}

	half := int64(len(data) / 2)
	ta.waitForTransfer(id, func(rec transferRecord) bool { return rec.State == transferRunning && rec.BytesDone == half })

	var listing struct {
		Active  []transferRecord `json:"active"`
		History []transferRecord `json:"history"`
	}
	if status := ta.do("GET", "/transfers?state=active", nil, &listing); status != http.StatusOK {
		t.Fatalf("GET /transfers?state=active: status %d", status)
	}
	if len(listing.Active) != 1 || listing.Active[0].ID != id || listing.History != nil {
		t.Errorf("GET /transfers?state=active = %+v, want just the running transfer and no history", listing)
	}

	close(release)
	ta.waitForTransfer(id, func(rec transferRecord) bool { return rec.State == transferCompleted })
	got, err := os.ReadFile(local)
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("downloaded file = %d bytes (err %v), want %d", len(got), err, len(data))
	}

	listing.Active, listing.History = nil, nil
	ta.do("GET", "/transfers", nil, &listing)
	if len(listing.Active) != 0 || len(listing.History) != 1 || listing.History[0].ID != id {
		t.Errorf("GET /transfers = %+v, want no active transfers and the finished one in history", listing)
	}
}

func TestAgentAsyncTransferCanceledOnShutdown(t *testing.T) {
	fake := newFakeStorageAPI()
	ta := newTestAgent(t, gatedDownloadServer(t, fake, make(chan struct{})), fake, true)
	addFakeFile(fake, "f1", "x.bin", bytes.Repeat([]byte("x"), 4096))
	local := filepath.Join(ta.root, "x.bin")

	var accepted struct {
		Transfer transferRecord `json:"transfer"`
	}
	ta.do("POST", "/transfer/download", map[string]interface{}{"nodeId": "f1", "localPath": local, "async": true}, &accepted)
	id := accepted.Transfer.ID
	ta.waitForTransfer(id, func(rec transferRecord) bool { return rec.BytesDone > 0 })

	ta.cancel()
	ta.waitForTransfer(id, func(rec transferRecord) bool { return rec.State == transferCanceled })
	if _, err := os.Stat(local + tmpSuffix); !os.IsNotExist(err) {
		t.Errorf("temp file left behind after cancel (stat err %v)", err)
	}
}

func TestTransferRegistryHistoryLimit(t *testing.T) {
	tr := newTransferRegistry(2)
	ctx := context.Background()
	var ids []string
	for i := 0; i < 3; i++ {
		id, err := tr.begin("", transferRecord{Direction: "upload"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		tr.finish(ctx, id, nil, errors.New("boom"))
	}

	_, history := tr.list()
	if len(history) != 2 || history[0].ID != ids[2] || history[1].ID != ids[1] {
		t.Errorf("history = %+v, want the two newest (%s, %s), newest first", history, ids[2], ids[1])
	}
	if _, ok := tr.get(ids[0]); ok {
		t.Errorf("evicted transfer %s still retrievable", ids[0])
	}
	// An evicted id may be reused.
	if _, err := tr.begin(ids[0], transferRecord{}); err != nil {
		t.Errorf("reusing an evicted transferId: %v", err)
	}

	var ae *apiError
	if _, err := tr.begin("not-a-uuid", transferRecord{}); !errors.As(err, &ae) || ae.Code != "invalid_transfer_id" {
		t.Errorf("non-UUID transferId: err %v, want invalid_transfer_id", err)
	}
	if newTransferRegistry(0).limit != defaultTransferHistoryLimit {
		t.Errorf("limit 0 should fall back to %d", defaultTransferHistoryLimit)
	}
}
