package main

import (
	"errors"
	"path/filepath"
)

// errInstanceLocked is returned by acquireInstanceLock when another brick
// process already holds the lock for this user.
var errInstanceLocked = errors.New("another brick instance is already running")

// instanceLockPath returns ~/.config/brick/brick.lock, the file whose flock
// (or Windows equivalent) enforces a single running sync engine per user.
func instanceLockPath() (string, error) {
	cfgPath, err := configPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(cfgPath), "brick.lock"), nil
}
