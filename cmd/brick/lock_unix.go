//go:build unix

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// instanceLock holds an exclusive, advisory lock on a file. The lock is tied
// to the open file descriptor: it is released automatically by the kernel if
// the process dies, so a crashed brick never leaves a stale lock behind.
type instanceLock struct {
	f *os.File
}

// acquireInstanceLock takes an exclusive, non-blocking flock on path,
// creating it if necessary. It returns errInstanceLocked if another process
// already holds it.
func acquireInstanceLock(path string) (*instanceLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("could not open lock file: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if err == unix.EWOULDBLOCK {
			return nil, errInstanceLocked
		}
		return nil, fmt.Errorf("could not lock %s: %w", path, err)
	}
	return &instanceLock{f: f}, nil
}

// Release drops the lock and closes the underlying file.
func (l *instanceLock) Release() {
	unix.Flock(int(l.f.Fd()), unix.LOCK_UN) //nolint:errcheck
	l.f.Close()
}
