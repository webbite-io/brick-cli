package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestConnectAgentWithReconnectSignalsConnectedOnce guards the ordering fix
// in runSyncLoop: the "Brick CLI connected..." log line should reliably land
// ahead of the initial reconcile pass's own output, via a short bounded wait
// on the channel connectAgentWithReconnect signals once a connection actually
// succeeds. This locks in that the channel really is closed after a
// successful connect, and that connectAgentWithReconnect still returns
// promptly once ctx is cancelled afterward.
func TestConnectAgentWithReconnectSignalsConnectedOnce(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// connectAgentOnce only needs a successful WS upgrade to get past
		// yamux.Server() and log "connected" — it doesn't need this fake
		// peer to speak yamux at all, so just idle until the client hangs up.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	cfg := &Config{AccessToken: "test-token", ActiveAccountID: "acct-1", ClientID: "client-1"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connected := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		connectAgentWithReconnect(ctx, server.URL, server.URL, cfg, "secret", "127.0.0.1:0", false, connected)
	}()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("connected channel was never closed after a successful connection")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("connectAgentWithReconnect did not return promptly after ctx cancellation")
	}
}

// A nil connected channel (the common case: runSyncLoop only allocates one
// when the agent server itself started successfully) must never be sent to
// or closed — connectAgentWithReconnect must guard every use of it.
func TestConnectAgentWithReconnectToleratesNilConnectedChannel(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	cfg := &Config{AccessToken: "test-token", ActiveAccountID: "acct-1", ClientID: "client-1"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		connectAgentWithReconnect(ctx, server.URL, server.URL, cfg, "secret", "127.0.0.1:0", false, nil)
	}()

	// No connected channel to wait on — just give it a moment to actually
	// connect (proving the nil channel didn't panic), then shut down.
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("connectAgentWithReconnect did not return promptly after ctx cancellation")
	}
}
