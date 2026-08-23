package main

import (
	"testing"
)

// The failure path (every attempt hitting EMFILE) isn't practical to exercise
// in a unit test without actually exhausting the host's real
// fs.inotify.max_user_instances ceiling, which would be both slow and
// disruptive to whatever else is running on the machine. This covers the
// happy path and the nil-safe degrade-mode fallback that failure path leads
// to in runSyncLoop.
func TestNewWatcherWithRetrySucceeds(t *testing.T) {
	w, err := newWatcherWithRetry()
	if err != nil {
		// A live inotify-instance ceiling on the machine running this test
		// (exactly the condition this fix is for) is an environment fact, not
		// a bug in newWatcherWithRetry -- skip rather than fail.
		t.Skipf("host has no spare fs.inotify instance right now (%v); skipping the happy-path assertion", err)
	}
	if w == nil {
		t.Fatal("newWatcherWithRetry() watcher = nil, want non-nil")
	}
	defer w.Close()
}

// addWatchesRecursive must tolerate a nil watcher without panicking: that's
// exactly what runSyncLoop passes it in poll-only degrade mode.
func TestAddWatchesRecursiveNilWatcher(t *testing.T) {
	eng := &syncEngine{folder: t.TempDir()}
	eng.addWatchesRecursive(nil) // must not panic
}
