package main

import (
	"io"
	"time"
)

// logTimestampLayout is the timestamp format used for all log output
// ("YYYY-MM-DD HH:ii:ss"), in place of the standard log package's built-in
// "2006/01/02 15:04:05" format.
const logTimestampLayout = "2006-01-02 15:04:05"

// timestampWriter prepends the current time (in logTimestampLayout) to every
// line written to it before forwarding to the underlying writer. It's meant
// to be paired with log.SetFlags(0), so callers get a custom timestamp
// format instead of the standard log package's fixed one.
type timestampWriter struct {
	w io.Writer
}

// newTimestampWriter wraps w so every write to it is prefixed with the
// current timestamp.
func newTimestampWriter(w io.Writer) io.Writer {
	return &timestampWriter{w: w}
}

func (t *timestampWriter) Write(p []byte) (int, error) {
	prefixed := append([]byte(time.Now().Format(logTimestampLayout)+" "), p...)
	if _, err := t.w.Write(prefixed); err != nil {
		return 0, err
	}
	return len(p), nil
}
