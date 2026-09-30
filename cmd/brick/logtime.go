package main

import (
	"io"
	"time"
)

// logTimestampLayout is the timestamp format used for all log output
// ("YYYY-MM-DD HH:ii:ss"), in place of the standard log package's built-in
// "2006/01/02 15:04:05" format.
const logTimestampLayout = "2006-01-02 15:04:05"

// logSourcePrefix tags every line this CLI writes to brick.log, the same
// rolling log file the corresponding Desktop app also appends to (never at
// the same time) tagging its own lines "UI:" — so a line's origin is clear
// regardless of which app most recently wrote to the file.
const logSourcePrefix = "CLI: "

// timestampWriter prepends the current time (in logTimestampLayout) and
// logSourcePrefix to every line written to it before forwarding to the
// underlying writer. It's meant to be paired with log.SetFlags(0), so
// callers get this custom format instead of the standard log package's own.
type timestampWriter struct {
	w io.Writer
}

// newTimestampWriter wraps w so every write to it is prefixed with the
// current timestamp and logSourcePrefix.
func newTimestampWriter(w io.Writer) io.Writer {
	return &timestampWriter{w: w}
}

func (t *timestampWriter) Write(p []byte) (int, error) {
	prefixed := append([]byte(time.Now().Format(logTimestampLayout)+" "+logSourcePrefix), p...)
	if _, err := t.w.Write(prefixed); err != nil {
		return 0, err
	}
	return len(p), nil
}
