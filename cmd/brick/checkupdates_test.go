package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newCheckUpdatesClient wires a storageClient at a test server. The server
// returns 200s with a valid bearer, so the token-refresh path is never taken.
func newCheckUpdatesClient(srv *httptest.Server) *storageClient {
	return &storageClient{
		baseURL:   srv.URL,
		apiURL:    srv.URL,
		accountID: "acct-1",
		cfg:       &Config{AccessToken: "test-token"},
	}
}

func writeUpdatesPage(t *testing.T, w http.ResponseWriter, data []storageNode, serverTime int64, nextCursor string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"data":       data,
		"count":      len(data),
		"serverTime": serverTime,
		"nextCursor": nextCursor,
	})
}

func TestCheckUpdates_NoChangesIsSingleRequest(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.URL.Query().Get("since"); got != "1000" {
			t.Errorf("since = %q, want 1000", got)
		}
		writeUpdatesPage(t, w, nil, 1234, "")
	}))
	defer srv.Close()

	changed, serverTime, err := newCheckUpdatesClient(srv).checkUpdates(context.Background(), 1000)
	if err != nil {
		t.Fatalf("checkUpdates: %v", err)
	}
	if changed {
		t.Error("expected changed=false for an empty feed")
	}
	if serverTime != 1234 {
		t.Errorf("serverTime = %d, want 1234", serverTime)
	}
	if calls != 1 {
		t.Errorf("expected exactly 1 request for a quiet account, got %d", calls)
	}
}

func TestCheckUpdates_ShortCircuitsOnFirstChange(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		// A change on the first page, plus a nextCursor: the client must NOT
		// follow the cursor, since it already knows a reconcile is needed.
		writeUpdatesPage(t, w, []storageNode{{ID: "n1", NodeType: "file"}}, 2000, "2000_n1")
	}))
	defer srv.Close()

	changed, serverTime, err := newCheckUpdatesClient(srv).checkUpdates(context.Background(), 500)
	if err != nil {
		t.Fatalf("checkUpdates: %v", err)
	}
	if !changed {
		t.Error("expected changed=true when the first page carries a change")
	}
	if serverTime != 2000 {
		t.Errorf("serverTime = %d, want 2000", serverTime)
	}
	if calls != 1 {
		t.Errorf("expected the walk to stop at the first change (1 request), got %d", calls)
	}
}

func TestCheckUpdates_FollowsCursorPastFilteredPages(t *testing.T) {
	// Two empty pages (changes the caller can't see, filtered server-side) then
	// a page that finally carries a visible change. The client must follow the
	// cursor through the empties rather than concluding "no changes" early.
	pages := []struct {
		data       []storageNode
		serverTime int64
		next       string
	}{
		{nil, 3000, "3000_a"},
		{nil, 3005, "3005_b"},
		{[]storageNode{{ID: "n9", NodeType: "file"}}, 3010, "3010_n9"},
	}
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cursor := r.URL.Query().Get("cursor")
		switch {
		case calls == 0 && cursor != "":
			t.Errorf("first request should carry no cursor, got %q", cursor)
		case calls == 1 && cursor != "3000_a":
			t.Errorf("second request cursor = %q, want 3000_a", cursor)
		case calls == 2 && cursor != "3005_b":
			t.Errorf("third request cursor = %q, want 3005_b", cursor)
		}
		p := pages[calls]
		calls++
		writeUpdatesPage(t, w, p.data, p.serverTime, p.next)
	}))
	defer srv.Close()

	changed, serverTime, err := newCheckUpdatesClient(srv).checkUpdates(context.Background(), 100)
	if err != nil {
		t.Fatalf("checkUpdates: %v", err)
	}
	if !changed {
		t.Error("expected changed=true once a visible change is found on a later page")
	}
	// The cursor adopted must be the FIRST page's serverTime, the earliest
	// snapshot — never a later page's, which could skip a change that landed
	// during pagination.
	if serverTime != 3000 {
		t.Errorf("serverTime = %d, want 3000 (first page's)", serverTime)
	}
	if calls != 3 {
		t.Errorf("expected 3 requests to reach the change, got %d", calls)
	}
}

func TestCheckUpdates_PropagatesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"boom"}}`))
	}))
	defer srv.Close()

	if _, _, err := newCheckUpdatesClient(srv).checkUpdates(context.Background(), 0); err == nil {
		t.Fatal("expected an error on a 500 response, got nil")
	}
}

func TestServerNow_ProbesWithFutureSince(t *testing.T) {
	var sawSince string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawSince = r.URL.Query().Get("since")
		writeUpdatesPage(t, w, nil, 4242, "")
	}))
	defer srv.Close()

	got, err := newCheckUpdatesClient(srv).serverNow(context.Background())
	if err != nil {
		t.Fatalf("serverNow: %v", err)
	}
	if got != 4242 {
		t.Errorf("serverNow = %d, want 4242", got)
	}
	// A far-future `since` is what keeps the probe's change set empty.
	if sawSince != fmt.Sprintf("%d", int64(1)<<62) {
		t.Errorf("since = %q, want far-future %d", sawSince, int64(1)<<62)
	}
}
