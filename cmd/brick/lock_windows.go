//go:build windows

package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// instanceLock holds an exclusive, advisory lock on a file. The lock is tied
// to the open file handle: it is released automatically by the OS if the
// process dies, so a crashed brick never leaves a stale lock behind.
type instanceLock struct {
	f *os.File
}

// acquireInstanceLock takes an exclusive, non-blocking lock on path, creating
// it if necessary. It returns errInstanceLocked if another process already
// holds it.
func acquireInstanceLock(path string) (*instanceLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("could not open lock file: %w", err)
	}
	ol := new(windows.Overlapped)
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	if err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, ol); err != nil {
		f.Close()
		if err == windows.ERROR_LOCK_VIOLATION {
			return nil, errInstanceLocked
		}
		return nil, fmt.Errorf("could not lock %s: %w", path, err)
	}
	return &instanceLock{f: f}, nil
}

// Release drops the lock and closes the underlying file.
func (l *instanceLock) Release() {
	ol := new(windows.Overlapped)
	windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, ol) //nolint:errcheck
	l.f.Close()
}
