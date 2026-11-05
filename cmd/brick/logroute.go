package main

import (
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Log lines are written in the form the sync TUI shows them. The other
// outputs — brick.log, and the terminal when there's no TUI — get them
// through plainLogWriter, which drops what's only there for the TUI.

// syncLogPrefix starts every line about something the sync engine did,
// the way "Remote Control: " starts every remote control one. In the TUI
// it's followed by an icon (e.g. "Sync: ↑ Uploaded a.txt"), which
// plainLogWriter strips.
const syncLogPrefix = "Sync: "

// fileOnlyMark starts a log message that's written to brick.log and plain
// output but kept out of the TUI — see logFileOnly. A NUL can't otherwise
// appear in a message: no filename may hold one.
const fileOnlyMark = "\x00"

// logFileOnly logs a line to brick.log and plain output, but not the TUI.
func logFileOnly(format string, v ...any) {
	log.Printf(fileOnlyMark+format, v...)
}

// plainLogWriter forwards each line written to it to w in its plain form
// (see plainLogLine). It sits under timestampWriter, so it sees each line
// with its timestamp already in front.
type plainLogWriter struct {
	w io.Writer
}

func newPlainLogWriter(w io.Writer) io.Writer {
	return &plainLogWriter{w: w}
}

func (p *plainLogWriter) Write(b []byte) (int, error) {
	lines := strings.SplitAfter(string(b), "\n")
	var out strings.Builder
	for _, line := range lines {
		out.WriteString(plainLogLine(line))
	}
	if _, err := io.WriteString(p.w, out.String()); err != nil {
		return 0, err
	}
	return len(b), nil
}

// plainLogLine drops fileOnlyMark from line and, from a sync line, the icon
// after syncLogPrefix: "… Sync: ↑ Uploaded a.txt" becomes
// "… Sync: Uploaded a.txt".
func plainLogLine(line string) string {
	line = strings.Replace(line, fileOnlyMark, "", 1)
	before, rest, ok := strings.Cut(line, logSourcePrefix+syncLogPrefix)
	if !ok {
		return line
	}
	return before + logSourcePrefix + syncLogPrefix + stripLeadingIcon(rest)
}

// stripLeadingIcon removes an icon — symbol characters, with any variation
// selector — and the spaces after it from the start of s. s is left as it
// is if it doesn't start with one followed by a space.
func stripLeadingIcon(s string) string {
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !unicode.IsSymbol(r) && !unicode.Is(unicode.Variation_Selector, r) {
			break
		}
		i += size
	}
	if i == 0 || i == len(s) || s[i] != ' ' {
		return s
	}
	return strings.TrimLeft(s[i:], " ")
}

// tuiLogLine is the form of a log line the TUI shows, or false for one it
// doesn't (logged with logFileOnly).
func tuiLogLine(line string) (string, bool) {
	if strings.Contains(line, fileOnlyMark) {
		return "", false
	}
	return line, true
}

// Connectivity sources: the agent's tunnel to Brick, and the sync engine's
// requests to the Storage API. brick counts as offline while either fails.
const (
	connSourceAgent = "agent"
	connSourceSync  = "sync"
)

// connectivity tracks whether brick can currently reach Brick, so the TUI
// can show a lost connection as a single "Experiencing connectivity
// issues..." row and its return as "Now we're back online.", instead of
// every failed reconnect, poll and transfer along the way. Those details
// still go to brick.log, as they always have.
type connectivity struct {
	mu   sync.Mutex
	down map[string]bool
	// show puts a line in the TUI, timestamped; nil when there's no TUI.
	show func(line string)
}

var connStatus = &connectivity{down: map[string]bool{}}

// setDisplay sets where the TUI-only connectivity notices go: show, or
// nowhere for nil.
func (c *connectivity) setDisplay(show func(line string)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.show = show
}

// fail logs a failure to reach Brick from source to brick.log only and,
// if brick was online until now, shows the TUI it's having connectivity
// issues.
func (c *connectivity) fail(source, format string, v ...any) {
	logFileOnly(format, v...)
	c.mu.Lock()
	defer c.mu.Unlock()
	wasOnline := len(c.down) == 0
	c.down[source] = true
	if wasOnline && c.show != nil {
		c.show("Experiencing connectivity issues...")
	}
}

// ok records that source reaches Brick again and, once every source does,
// tells the TUI brick is back online. It reports whether brick was offline
// before the call, in which case the caller's own "connected" line belongs
// in brick.log only.
func (c *connectivity) ok(source string) (wasOffline bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	wasOffline = len(c.down) > 0
	delete(c.down, source)
	if wasOffline && len(c.down) == 0 && c.show != nil {
		c.show("Now we're back online.")
	}
	return wasOffline
}

// logSyncErr logs a sync error, as fmt.Sprintf(format, v...): through
// connStatus.fail when err means Brick couldn't be reached, so the TUI isn't
// flooded with it while offline, and as an ordinary line otherwise.
func logSyncErr(err error, format string, v ...any) {
	if isTransient(err) {
		connStatus.fail(connSourceSync, format, v...)
		return
	}
	log.Print(fmt.Sprintf(format, v...))
}
