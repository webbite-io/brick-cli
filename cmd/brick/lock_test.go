package main

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireInstanceLockSingleton(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brick.lock")

	l1, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer l1.Release()

	_, err = acquireInstanceLock(path)
	if !errors.Is(err, errInstanceLocked) {
		t.Fatalf("second acquire: err = %v, want errInstanceLocked", err)
	}

	l1.Release()

	l2, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	l2.Release()
}
