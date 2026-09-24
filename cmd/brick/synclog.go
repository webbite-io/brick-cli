package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// syncLogFileName is the rolling log file sync output is mirrored to,
// alongside whatever's shown in the terminal.
const syncLogFileName = "brick.log"

// syncLogMaxLines is the maximum number of lines brick.log is allowed to
// hold — it's a rolling record of recent sync activity, not an
// ever-growing file, so once full the oldest lines are dropped to make
// room for new ones.
const syncLogMaxLines = 10000

// syncLogWriter is an io.Writer that appends to <configDir>/brick.log. It
// keeps the file capped at roughly syncLogMaxLines lines so it never grows
// unbounded across a long-lived daemon.
type syncLogWriter struct {
	mu        sync.Mutex
	path      string
	file      *os.File
	lineCount int
}

// newSyncLogWriter opens (creating if necessary) <configDir>/brick.log for
// appending.
func newSyncLogWriter() (*syncLogWriter, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("could not create config directory: %w", err)
	}
	path := filepath.Join(dir, syncLogFileName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("could not open %s: %w", path, err)
	}
	lineCount, err := countLines(path)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &syncLogWriter{path: path, file: f, lineCount: lineCount}, nil
}

// countLines returns the number of newline-terminated lines in path.
func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		count++
	}
	return count, scanner.Err()
}

// Write appends p to the log file, trimming the file back down to
// syncLogMaxLines lines whenever it grows past that cap.
func (w *syncLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n, err := w.file.Write(p)
	if err != nil {
		return n, err
	}
	w.lineCount += strings.Count(string(p), "\n")

	if w.lineCount > syncLogMaxLines {
		if trimErr := w.trimLocked(); trimErr != nil {
			return n, trimErr
		}
	}
	return n, nil
}

// trimLocked rewrites the log file to keep only its last syncLogMaxLines
// lines. Callers must hold w.mu.
func (w *syncLogWriter) trimLocked() error {
	data, err := os.ReadFile(w.path)
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > syncLogMaxLines {
		lines = lines[len(lines)-syncLogMaxLines:]
	}

	tmpPath := w.path + ".tmp"
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(tmpFile)
	for _, line := range lines {
		bw.WriteString(line)
		bw.WriteByte('\n')
	}
	if err := bw.Flush(); err != nil {
		tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}

	if err := w.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, w.path); err != nil {
		return err
	}
	f, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w.file = f
	w.lineCount = len(lines)
	return nil
}

// Close closes the underlying file.
func (w *syncLogWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}
