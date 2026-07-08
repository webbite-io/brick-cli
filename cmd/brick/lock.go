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
// after 'brick --restart' wipes it — create it here rather than failing.
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
