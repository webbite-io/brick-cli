package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeTokenServer is a token endpoint plus a Storage API /resolve that only
// accepts the access token it last issued. tokenStatus sets what a refresh
// answers: 200 issues a new pair, 400 rejects the refresh token
// (invalid_grant), anything else is an outage.
type fakeTokenServer struct {
	*httptest.Server
	mu           sync.Mutex
	tokenStatus  int
	access       string
	refreshCalls atomic.Int32
}

func newFakeTokenServer(t *testing.T) *fakeTokenServer {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".config", "brick"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeTokenServer{tokenStatus: http.StatusOK, access: "valid"}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		f.refreshCalls.Add(1)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch f.tokenStatus {
		case http.StatusOK:
			f.access = "fresh"
			writeJSON(w, http.StatusOK, map[string]string{"access_token": "fresh", "refresh_token": "next-refresh"})
		case http.StatusBadRequest:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
		default:
			w.WriteHeader(f.tokenStatus)
		}
	})
	mux.HandleFunc("/v1/accounts/acct-1/resolve", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		ok := r.Header.Get("Authorization") == "Bearer "+f.access
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, storageNode{ID: "root"})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeTokenServer) setTokenStatus(status int) {
	f.mu.Lock()
	f.tokenStatus = status
	f.mu.Unlock()
}

func (f *fakeTokenServer) client(cfg *Config) *storageClient {
	return &storageClient{baseURL: f.URL, apiURL: f.URL, accountID: "acct-1", cfg: cfg}
}

// A token endpoint that cannot answer (no network yet after a wake, a 5xx)
// says nothing about the session: the error must not read as expired, and the
// refresh token must survive for the next attempt.
func TestRefreshOutageIsNotSessionExpired(t *testing.T) {
	f := newFakeTokenServer(t)
	cfg := &Config{AccessToken: "stale", RefreshToken: "refresh-1"}
	f.setTokenStatus(http.StatusServiceUnavailable)

	_, err := f.client(cfg).resolveRoot(context.Background())
	if err == nil || errors.Is(err, errSessionExpired) {
		t.Fatalf("err = %v, want a plain error", err)
	}
	if cfg.RefreshToken != "refresh-1" {
		t.Errorf("refresh token changed during the outage: %q", cfg.RefreshToken)
	}

	f.setTokenStatus(http.StatusOK)
	root, err := f.client(cfg).resolveRoot(context.Background())
	if err != nil || root.ID != "root" {
		t.Fatalf("after the outage: %+v %v", root, err)
	}
	if cfg.RefreshToken != "next-refresh" {
		t.Errorf("rotated refresh token not kept: %q", cfg.RefreshToken)
	}
}

func TestRejectedRefreshIsSessionExpired(t *testing.T) {
	f := newFakeTokenServer(t)
	cfg := &Config{AccessToken: "stale", RefreshToken: "revoked"}
	f.setTokenStatus(http.StatusBadRequest)
	if _, err := f.client(cfg).resolveRoot(context.Background()); !errors.Is(err, errSessionExpired) {
		t.Fatalf("err = %v, want errSessionExpired", err)
	}
}

func TestEnsureAuthenticatedClassifiesRefreshFailures(t *testing.T) {
	f := newFakeTokenServer(t)
	if err := saveConfig(&Config{RefreshToken: "refresh-1"}); err != nil {
		t.Fatal(err)
	}

	f.setTokenStatus(http.StatusBadGateway)
	if _, err := ensureAuthenticated(f.URL, nil); err == nil || errors.Is(err, errSessionExpired) {
		t.Errorf("outage: err = %v, want a plain error", err)
	}
	f.setTokenStatus(http.StatusBadRequest)
	if _, err := ensureAuthenticated(f.URL, nil); !errors.Is(err, errSessionExpired) {
		t.Errorf("rejected: err = %v, want errSessionExpired", err)
	}
	f.setTokenStatus(http.StatusOK)
	cfg, err := ensureAuthenticated(f.URL, nil)
	if err != nil || cfg.AccessToken != "fresh" {
		t.Errorf("refresh: %+v %v", cfg, err)
	}
}
