package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/yamux"
)

// tunnelPeer plays the storage API's side of the agent tunnel: it accepts the
// agent's WebSocket, takes the yamux client role, and sends requests over
// streams of it, one per request, as the storage API does.
type tunnelPeer struct {
	t       *testing.T
	session chan *yamux.Session
}

func newTunnelPeer(t *testing.T) (*tunnelPeer, *httptest.Server) {
	p := &tunnelPeer{t: t, session: make(chan *yamux.Session, 1)}
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		cfg := yamux.DefaultConfig()
		cfg.LogOutput = io.Discard
		session, err := yamux.Client(newWSConn(ws), cfg)
		if err != nil {
			ws.Close()
			return
		}
		p.session <- session
		<-session.CloseChan()
	}))
	t.Cleanup(server.Close)
	return p, server
}

// do sends req over a new stream and returns the response, with its body
// read, and whether the agent closed the stream after it.
func (p *tunnelPeer) do(session *yamux.Session, req *http.Request) (*http.Response, []byte, bool) {
	p.t.Helper()
	stream, err := session.Open()
	if err != nil {
		p.t.Fatal(err)
	}
	defer stream.Close()
	if err := req.Write(stream); err != nil {
		p.t.Fatal(err)
	}
	br := bufio.NewReader(stream)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		p.t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		p.t.Fatal(err)
	}
	stream.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = br.ReadByte()
	return resp, body, err == io.EOF
}

func TestAgentServedOverTunnel(t *testing.T) {
	var logBuf bytes.Buffer
	var logMu sync.Mutex
	log.SetOutput(writerFunc(func(b []byte) (int, error) {
		logMu.Lock()
		defer logMu.Unlock()
		return logBuf.Write(b)
	}))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	peer, server := newTunnelPeer(t)
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	agent := startAgentServer(ctx, []agentRoot{{path: root}}, testAgentSecret, nil, true, 0, nil)
	cfg := &Config{AccessToken: "test-token", ActiveAccountID: "acct-1", ClientID: "client-1"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		connectAgentWithReconnect(ctx, server.URL, server.URL, cfg, testAgentSecret, agent.handler(), true, nil)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	var session *yamux.Session
	select {
	case session = <-peer.session:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent never connected")
	}

	request := func(method, target string, body []byte, secret string) *http.Request {
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		req, err := http.NewRequest(method, "http://agent"+target, r)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(agentSecretHeader, secret)
		return req
	}

	resp, body, closed := peer.do(session, request("GET", "/version", nil, testAgentSecret))
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"remoteControl":true`) {
		t.Errorf("GET /version = %d %s, want 200 with remoteControl", resp.StatusCode, body)
	}
	if !closed {
		t.Error("the stream stayed open after the response, want one request per stream")
	}

	resp, _, _ = peer.do(session, request("GET", "/version", nil, "wrong"))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("wrong secret: status %d, want 403", resp.StatusCode)
	}

	// A body larger than any buffer on the way, written and read back whole
	// and in part.
	data := bytes.Repeat([]byte("0123456789abcdef"), 256<<10) // 4 MiB
	file := filepath.Join(root, "big.bin")
	resp, body, _ = peer.do(session, request("PUT", "/fs/write?path="+url.QueryEscape(file), data, testAgentSecret))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /fs/write: %d %s", resp.StatusCode, body)
	}
	if got, _ := os.ReadFile(file); !bytes.Equal(got, data) {
		t.Fatalf("written file is %d bytes, want the %d sent", len(got), len(data))
	}
	resp, body, _ = peer.do(session, request("GET", "/fs/read?path="+url.QueryEscape(file), nil, testAgentSecret))
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, data) {
		t.Errorf("GET /fs/read: status %d, %d bytes, want 200 and the whole file", resp.StatusCode, len(body))
	}
	req := request("GET", "/fs/read?path="+url.QueryEscape(file), nil, testAgentSecret)
	req.Header.Set("Range", "bytes=100-199")
	resp, body, _ = peer.do(session, req)
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, data[100:200]) {
		t.Errorf("ranged GET /fs/read: status %d, %q, want 206 and bytes 100-199", resp.StatusCode, body)
	}

	logMu.Lock()
	logged := logBuf.String()
	logMu.Unlock()
	for _, want := range []string{
		"Remote Control: Checking the version.",
		"Remote Control: Checking the version failed (403).",
		`Remote Control: Writing "big.bin".`,
		`Remote Control: Reading "big.bin".`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log is missing %q; got:\n%s", want, logged)
		}
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
