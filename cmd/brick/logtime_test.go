package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// timestampWriter's output feeds brick.log, the same rolling log file the
// corresponding Desktop app also appends to (never at the same time),
// tagging its own lines "UI:" — this locks in the CLI's side of that
// convention: "<timestamp> CLI: <message>".
func TestTimestampWriterPrefixesTimestampAndCLISource(t *testing.T) {
	var buf bytes.Buffer
	w := newTimestampWriter(&buf)

	before := time.Now()
	if _, err := w.Write([]byte("uploaded vimwiki/muppp2.txt (updated)\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	after := time.Now()

	line := buf.String()
	if !strings.Contains(line, " "+logSourcePrefix) {
		t.Fatalf("line = %q, want it to contain %q", line, " "+logSourcePrefix)
	}
	if !strings.HasSuffix(line, logSourcePrefix+"uploaded vimwiki/muppp2.txt (updated)\n") {
		t.Errorf("line = %q, want it to end with %q", line, logSourcePrefix+"uploaded vimwiki/muppp2.txt (updated)\n")
	}

	tsField := strings.SplitN(line, " "+logSourcePrefix, 2)[0]
	ts, err := time.ParseInLocation(logTimestampLayout, tsField, time.Local)
	if err != nil {
		t.Fatalf("timestamp field %q did not parse as %q: %v", tsField, logTimestampLayout, err)
	}
	// Truncate to the second (the layout's own resolution) before comparing,
	// so a write that lands exactly on a second boundary can't spuriously fail.
	if ts.Before(before.Truncate(time.Second)) || ts.After(after.Truncate(time.Second).Add(time.Second)) {
		t.Errorf("timestamp %v not within [%v, %v]", ts, before, after)
	}
}
