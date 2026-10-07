package main

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func shortStartRetry(t *testing.T) {
	t.Helper()
	oldMin, oldMax := startRetryMin, startRetryMax
	startRetryMin, startRetryMax = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() { startRetryMin, startRetryMax = oldMin, oldMax })
}

func TestRetryWhileUnreachable(t *testing.T) {
	shortStartRetry(t)

	calls := 0
	err := retryWhileUnreachable(func() error {
		calls++
		if calls < 3 {
			return errUnreachable
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Errorf("err=%v calls=%d, want nil after 3", err, calls)
	}

	// Anything else ends it at once.
	calls = 0
	other := errors.New("boom")
	if err := retryWhileUnreachable(func() error { calls++; return other }); err != other || calls != 1 {
		t.Errorf("err=%v calls=%d, want boom after 1", err, calls)
	}
}

// configuredForTest saves a set-up config (account and sync folder) holding
// only a refresh token, so starting a sync has to refresh first.
func configuredForTest(t *testing.T) {
	t.Helper()
	cfg := &Config{RefreshToken: "refresh-1", ActiveAccountID: "acct-1", Accounts: map[string]*AccountConfig{
		"acct-1": {StorageSyncFolder: t.TempDir()},
	}}
	if err := saveConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

// A sync started while the auth server is out of reach (a machine waking
// before its network is back) waits for it instead of failing.
func TestPrepareSyncWaitsOutAnOutage(t *testing.T) {
	shortStartRetry(t)
	f := newFakeTokenServer(t)
	configuredForTest(t)
	f.setTokenStatus(http.StatusServiceUnavailable)
	go func() {
		for f.refreshCalls.Load() < 3 {
			time.Sleep(time.Millisecond)
		}
		f.setTokenStatus(http.StatusOK)
	}()

	setup, err := prepareSync(f.URL, f.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	if setup.rootID != "root" || f.refreshCalls.Load() < 4 {
		t.Errorf("rootID=%q refreshes=%d", setup.rootID, f.refreshCalls.Load())
	}
}

// Without retry (dry run, setup-and-exit, the daemon parent) it fails fast.
func TestPrepareSyncWithoutRetryFailsFast(t *testing.T) {
	f := newFakeTokenServer(t)
	configuredForTest(t)
	f.setTokenStatus(http.StatusServiceUnavailable)
	_, err := prepareSync(f.URL, f.URL, false)
	if !errors.Is(err, errUnreachable) || f.refreshCalls.Load() != 1 {
		t.Errorf("err=%v refreshes=%d, want errUnreachable after 1", err, f.refreshCalls.Load())
	}
}

// A rejected refresh token is not waited on: logging in again is the fix.
func TestPrepareSyncDoesNotRetryAnExpiredSession(t *testing.T) {
	shortStartRetry(t)
	f := newFakeTokenServer(t)
	configuredForTest(t)
	f.setTokenStatus(http.StatusBadRequest)
	_, err := prepareSync(f.URL, f.URL, true)
	if !errors.Is(err, errSessionExpired) || f.refreshCalls.Load() != 1 {
		t.Errorf("err=%v refreshes=%d, want errSessionExpired after 1", err, f.refreshCalls.Load())
	}
}

// The reachability check itself is retried too: a Storage API that fails
// before answering is waited for.
func TestResolveSyncRootUnreachableIsRetried(t *testing.T) {
	shortStartRetry(t)
	f := newFakeTokenServer(t)
	cfg := &Config{AccessToken: "stale", RefreshToken: "refresh-1"}
	f.setTokenStatus(http.StatusBadGateway)
	attempts := 0
	var root *storageNode
	err := retryWhileUnreachable(func() (err error) {
		attempts++
		if attempts == 2 {
			f.setTokenStatus(http.StatusOK)
		}
		root, err = resolveSyncRoot(f.client(cfg), f.URL)
		return err
	})
	if err != nil || root.ID != "root" || attempts != 2 {
		t.Errorf("root=%+v err=%v attempts=%d", root, err, attempts)
	}
}
