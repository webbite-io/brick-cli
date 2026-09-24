package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// errInstanceLocked is returned by acquireInstanceLock when another brick
// process already holds the lock for this user.
var errInstanceLocked = errors.New("another brick instance is already running")

// instanceLockPath returns ~/.config/brick/brick.lock, the file whose flock
// (or Windows equivalent) enforces a single running sync engine per user.
// The lock is acquired before loadOrCreateConfig runs (see runStorageSync),
// so ~/.config/brick may not exist yet on a genuinely fresh install or right
// after 'brick restart' wipes it — create it here rather than failing.
func instanceLockPath() (string, error) {
	cfgPath, err := configPath()
	if err != nil {
		return "", err
	}
	cfgDir := filepath.Dir(cfgPath)
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return "", fmt.Errorf("could not create config directory: %w", err)
	}
	return filepath.Join(cfgDir, "brick.lock"), nil
}

// requireNoRunningInstance fails when another brick instance — this CLI or
// the desktop app — currently holds the per-user lock.
//
// Commands that reconfigure the active account or delete files out from
// under a running sync call this first. brick has no control API any more,
// so it can't pause or stop that instance remotely; refusing is what keeps a
// reconcile pass from racing the change (e.g. seeing a newly-excluded folder
// disappear and pushing that as a real local delete). The lock is released
// again immediately — it only answers "is anything running right now?", and
// the caller goes on to take it for itself if it needs to.
func requireNoRunningInstance(cmd string) error {
	path, err := instanceLockPath()
	if err != nil {
		return err
	}
	lock, err := acquireInstanceLock(path)
	if err != nil {
		if errors.Is(err, errInstanceLocked) {
			return fmt.Errorf("another brick instance is running for this user.\n"+
				"Stop it (Ctrl+C in its terminal, or quit the Brick app) and run '%s' again", cmd)
		}
		return err
	}
	lock.Release()
	return nil
}
