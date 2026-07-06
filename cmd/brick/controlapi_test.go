package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// newTestControlServer starts a control server against a bare-minimum
// syncEngine (no network-backed storageClient — none of the control
// endpoints under test touch it) and points HOME/XDG_RUNTIME_DIR/LOCALAPPDATA
// at a temp dir so it never touches the real user config.
func newTestControlServer(t *testing.T) (*controlServer, *syncEngine, func()) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("LOCALAPPDATA", dir)

	eng := &syncEngine{folder: "/tmp/does-not-matter"}
	eng.setState("starting")

	ctx, cancel := context.WithCancel(context.Background())
	notified := make(chan struct{}, 1)
	notify := func() {
		select {
		case notified <- struct{}{}:
		default:
		}
	}

	cs, err := startControlServer(eng, cancel, notify)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	return cs, eng, func() {
		cs.Close()
		cancel()
		_ = ctx
	}
}

// httpClientFor returns an http.Client that dials the control server's unix
// socket regardless of the URL host/scheme passed to it.
func httpClientFor(cs *controlServer) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", cs.socketPath)
			},
		},
		Timeout: 2 * time.Second,
	}
}

func TestControlServerDiscoveryFile(t *testing.T) {
	cs, _, cleanup := newTestControlServer(t)
	defer cleanup()

	data, err := os.ReadFile(cs.discoveryPath)
	if err != nil {
		t.Fatalf("reading discovery file: %v", err)
	}
	var disc controlDiscovery
	if err := json.Unmarshal(data, &disc); err != nil {
		t.Fatalf("unmarshal discovery file: %v", err)
	}
	if disc.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", disc.PID, os.Getpid())
	}
	if disc.Token != cs.token {
		t.Errorf("Token = %q, want %q", disc.Token, cs.token)
	}
	if disc.Transport != "unix" {
		t.Errorf("Transport = %q, want unix", disc.Transport)
	}
	if disc.ProtocolVersion != controlProtocolVersion {
		t.Errorf("ProtocolVersion = %d, want %d", disc.ProtocolVersion, controlProtocolVersion)
	}
	info, err := os.Stat(cs.discoveryPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("discovery file mode = %v, want 0600", perm)
	}
}

func TestControlServerAuth(t *testing.T) {
	cs, _, cleanup := newTestControlServer(t)
	defer cleanup()
	client := httpClientFor(cs)

	// No token -> forbidden.
	resp, err := client.Get("http://unix/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("no token: status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	// Wrong token -> forbidden.
	req, _ := http.NewRequest("GET", "http://unix/v1/status", nil)
	req.Header.Set(controlSecretHeader, "wrong-token")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong token: status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	// Correct token -> ok.
	req, _ = http.NewRequest("GET", "http://unix/v1/status", nil)
	req.Header.Set(controlSecretHeader, cs.token)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("correct token: status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	// /v1/health needs no token at all.
	resp, err = client.Get("http://unix/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("health: status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestControlServerStatusReflectsState(t *testing.T) {
	cs, eng, cleanup := newTestControlServer(t)
	defer cleanup()
	client := httpClientFor(cs)

	get := func() controlStatus {
		t.Helper()
		req, _ := http.NewRequest("GET", "http://unix/v1/status", nil)
		req.Header.Set(controlSecretHeader, cs.token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		var st controlStatus
		if err := json.Unmarshal(body, &st); err != nil {
			t.Fatalf("unmarshal status: %v (body=%s)", err, body)
		}
		return st
	}

	eng.setState("syncing")
	if st := get(); st.State != "syncing" {
		t.Errorf("state = %q, want syncing", st.State)
	}

	eng.uploaded.Add(3)
	eng.downloaded.Add(1)
	if st := get(); st.Counters.Uploaded != 3 || st.Counters.Downloaded != 1 {
		t.Errorf("counters = %+v, want uploaded=3 downloaded=1", st.Counters)
	}

	eng.setInFlight("docs/report.pdf", "upload")
	st := get()
	if st.InFlight == nil || st.InFlight.RelPath != "docs/report.pdf" || st.InFlight.Direction != "upload" {
		t.Errorf("inFlight = %+v, want docs/report.pdf upload", st.InFlight)
	}
	eng.clearInFlight()
	if st := get(); st.InFlight != nil {
		t.Errorf("inFlight after clear = %+v, want nil", st.InFlight)
	}

	// paused overlays whatever state reconcile last recorded.
	eng.setPaused(true)
	if st := get(); st.State != "paused" {
		t.Errorf("state while paused = %q, want paused", st.State)
	}
	eng.setPaused(false)
	if st := get(); st.State != "syncing" {
		t.Errorf("state after unpause = %q, want syncing (last recorded state)", st.State)
	}
}

func TestControlServerPauseResume(t *testing.T) {
	cs, eng, cleanup := newTestControlServer(t)
	defer cleanup()
	client := httpClientFor(cs)

	post := func(path string) int {
		t.Helper()
		req, _ := http.NewRequest("POST", "http://unix"+path, nil)
		req.Header.Set(controlSecretHeader, cs.token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if status := post("/v1/pause"); status != http.StatusOK {
		t.Fatalf("pause status = %d", status)
	}
	if !eng.paused.Load() {
		t.Error("expected eng.paused true after /v1/pause")
	}

	if status := post("/v1/resume"); status != http.StatusOK {
		t.Fatalf("resume status = %d", status)
	}
	if eng.paused.Load() {
		t.Error("expected eng.paused false after /v1/resume")
	}
}

func TestControlServerActivity(t *testing.T) {
	cs, eng, cleanup := newTestControlServer(t)
	defer cleanup()
	client := httpClientFor(cs)

	eng.publishActivity("upload", "a.txt")
	eng.publishActivity("download", "b.txt")
	eng.publishActivity("delete", "c.txt")

	req, _ := http.NewRequest("GET", "http://unix/v1/activity?limit=2", nil)
	req.Header.Set(controlSecretHeader, cs.token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []controlActivityEvent
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatalf("decode activity: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	// Newest first.
	if events[0].RelPath != "c.txt" || events[1].RelPath != "b.txt" {
		t.Errorf("events = %+v, want [c.txt, b.txt]", events)
	}
}

func TestControlServerQuitCancelsContext(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("LOCALAPPDATA", dir)

	eng := &syncEngine{folder: "/tmp/does-not-matter"}
	ctx, cancel := context.WithCancel(context.Background())
	notify := func() {}

	cs, err := startControlServer(eng, cancel, notify)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	defer cs.Close()

	client := httpClientFor(cs)
	req, _ := http.NewRequest("POST", "http://unix/v1/quit", nil)
	req.Header.Set(controlSecretHeader, cs.token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context was not cancelled by /v1/quit")
	}
}
