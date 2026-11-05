package main

import (
	"bytes"
	"log"
	"os"
	"slices"
	"strings"
	"testing"
)

// brick.log gets sync lines without the TUI's icons: "Sync: Uploaded a.txt".
func TestPlainLogWriterStripsSyncIcons(t *testing.T) {
	cases := map[string]string{
		syncLogPrefix + "↑ Uploaded a.txt":                   "Sync: Uploaded a.txt",
		syncLogPrefix + "↓ Downloaded a.txt":                 "Sync: Downloaded a.txt",
		syncLogPrefix + "🗑  Trashed a.txt (deleted locally)": "Sync: Trashed a.txt (deleted locally)",
		syncLogPrefix + "→ Moved a.txt to b.txt":             "Sync: Moved a.txt to b.txt",
		syncLogPrefix + "⧉ Kept both copies of a.txt (as b)": "Sync: Kept both copies of a.txt (as b)",
		syncLogPrefix + "⏸ Paused":                           "Sync: Paused",
		syncLogPrefix + "▶️ Resumed":                         "Sync: Resumed",
		`Remote Control: Reading "→ x".`:                     `Remote Control: Reading "→ x".`,
		"remote poll error: timeout":                         "remote poll error: timeout",
		fileOnlyMark + "Brick CLI disconnected (EOF)":        "Brick CLI disconnected (EOF)",
	}
	for in, want := range cases {
		var buf bytes.Buffer
		w := newTimestampWriter(newPlainLogWriter(&buf))
		if _, err := w.Write([]byte(in + "\n")); err != nil {
			t.Fatal(err)
		}
		if got := buf.String(); !strings.HasSuffix(got, " "+logSourcePrefix+want+"\n") {
			t.Errorf("%q logged as %q, want it to end with %q", in, got, logSourcePrefix+want)
		}
	}
}

// The TUI keeps the icons, but not the lines logged with logFileOnly.
func TestTUILogWriterSkipsFileOnlyLines(t *testing.T) {
	tw := &tuiLogWriter{lines: make(chan string, 10)}
	w := newTimestampWriter(tw)
	w.Write([]byte(syncLogPrefix + "↑ Uploaded a.txt\n"))
	w.Write([]byte(fileOnlyMark + "Brick CLI disconnected (EOF); reconnecting in 1s...\n"))
	close(tw.lines)

	var got []string
	for line := range tw.lines {
		got = append(got, line)
	}
	if len(got) != 1 || !strings.HasSuffix(got[0], "Sync: ↑ Uploaded a.txt") {
		t.Errorf("TUI got %q, want only the upload line, icon and all", got)
	}
}

// A lost connection is one row in the TUI until every source is back,
// while each failure's detail still reaches brick.log.
func TestConnectivityShowsOneRowUntilBackOnline(t *testing.T) {
	var file bytes.Buffer
	log.SetOutput(newTimestampWriter(newPlainLogWriter(&file)))
	t.Cleanup(func() { log.SetOutput(newTimestampWriter(newPlainLogWriter(os.Stderr))) })

	c := &connectivity{down: map[string]bool{}}
	var shown []string
	c.setDisplay(func(line string) { shown = append(shown, line) })

	if c.ok(connSourceAgent) {
		t.Error("ok() while online reported being offline before")
	}
	c.fail(connSourceAgent, "Brick CLI disconnected (%s)", "websocket: bad handshake")
	c.fail(connSourceSync, "remote poll error: %s", "connection refused")
	c.fail(connSourceAgent, "Brick CLI disconnected (%s)", "EOF")
	if want := []string{"Experiencing connectivity issues..."}; !slices.Equal(shown, want) {
		t.Fatalf("TUI shown %q while failing, want %q", shown, want)
	}

	if !c.ok(connSourceAgent) {
		t.Error("ok() after failures reported being online before")
	}
	if len(shown) != 1 {
		t.Fatalf("TUI shown %q with the sync source still failing, want no new row", shown)
	}
	c.ok(connSourceSync)
	if want := []string{"Experiencing connectivity issues...", "Now we're back online."}; !slices.Equal(shown, want) {
		t.Errorf("TUI shown %q, want %q", shown, want)
	}

	for _, detail := range []string{"websocket: bad handshake", "connection refused", "EOF"} {
		if !strings.Contains(file.String(), detail) {
			t.Errorf("brick.log = %q, want it to hold %q", file.String(), detail)
		}
	}
	if strings.Contains(file.String(), "Experiencing") || strings.Contains(file.String(), "back online") {
		t.Errorf("brick.log = %q, want the TUI-only notices left out", file.String())
	}
}
