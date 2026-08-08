package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/charmbracelet/x/term"
)

// liveWindowSize is the maximum number of log lines shown at once beneath the
// interactive sync banner (see printSyncBanner and liveWindow below).
const liveWindowSize = 10

const (
	ansiReset     = "\033[0m"
	ansiPurple    = "\033[38;5;135m"
	ansiDarkGreen = "\033[38;5;22m"
	ansiOrange    = "\033[38;5;208m"
	ansiRed       = "\033[38;5;196m"
)

// quotaWarnRatio and quotaCriticalRatio are the fractions of the account's
// storage quota at which the banner's storage line turns orange and then red.
const (
	quotaWarnRatio     = 0.75
	quotaCriticalRatio = 0.95
)

// printSyncBanner prints the header line shown just before the sync loop
// starts. In an interactive terminal the rest of the header block (commands,
// storage usage, separator) is rendered by syncHeaderLines into the live
// window instead of being printed here, so the storage line can be repainted
// in place whenever the quota is refreshed. Otherwise (redirected output, or
// the detached daemon child) this is the original plain one-liner.
func printSyncBanner(folder string, interactive bool) {
	if !interactive {
		fmt.Printf("Syncing %s with Brick. Press Ctrl+C to stop.\n", folder)
		return
	}
	fmt.Printf("Syncing %s with Brick.\n", folder)
}

// syncHeaderLines renders the block shown beneath the "Syncing ..." line in
// interactive mode: the keyboard shortcuts, the account's storage usage, and
// a separator sized to match. q is nil until the first quota fetch succeeds,
// in which case the storage line is simply left out.
func syncHeaderLines(q *storageQuota) []string {
	plain := "Ctrl+C: Stop"
	colored := ansiDarkGreen + "Ctrl+C" + ansiReset + ": Stop"
	if daemonSupported {
		plain += " • D: Detach as daemon"
		colored += " • " + ansiDarkGreen + "D" + ansiReset + ": Detach as daemon"
	}
	plain += " • P: Pause/resume"
	colored += " • " + ansiDarkGreen + "P" + ansiReset + ": Pause/resume"

	const prefix = "Commands: "
	lines := []string{fmt.Sprintf("%sCommands:%s %s", ansiPurple, ansiReset, colored)}
	if line := quotaLine(q); line != "" {
		lines = append(lines, line)
	}
	// Sized to the commands line, which is always the widest of the block.
	return append(lines, strings.Repeat("=", len(prefix)+len(plain)))
}

// quotaLine renders the banner's storage usage line, e.g.
//
//	Storage: 50.9 GB of 500.0 GB used total (your share 28.0 GB)
//
// The "X of Y used total" part turns orange once usage passes
// quotaWarnRatio of the quota and red past quotaCriticalRatio, so an account
// running out of room is visible at a glance. Returns "" when no quota has
// been fetched yet, or when the account reports no quota at all — there is no
// ratio to render against in that case.
func quotaLine(q *storageQuota) string {
	if q == nil || q.QuotaBytes <= 0 {
		return ""
	}
	used := fmt.Sprintf("Total use is %s of %s", humanSize(q.UsedBytes), humanSize(q.QuotaBytes))
	switch ratio := float64(q.UsedBytes) / float64(q.QuotaBytes); {
	case ratio > quotaCriticalRatio:
		used = ansiRed + used + ansiReset
	case ratio > quotaWarnRatio:
		used = ansiOrange + used + ansiReset
	}
	return fmt.Sprintf("%sStorage:%s %s (your share %s)", ansiPurple, ansiReset, used, humanSize(q.CallingUser.UsedBytes))
}

// readSyncKeys reads single-byte keypresses from r (stdin, put into raw mode
// by the caller) and reacts to the shortcuts advertised by printSyncBanner:
// Ctrl+C stops the loop the same way SIGINT normally would (raw mode
// disables the terminal's own SIGINT-on-Ctrl+C handling, so this is the only
// way it's caught); where daemon mode is supported, 'd'/'D' requests a
// detach; and 'p'/'P' calls togglePause to flip the sync engine's paused
// state (unlike the other two shortcuts, this doesn't end the loop — reading
// continues so the same key can toggle back). Returns once the context is
// cancelled by any means or the read fails (e.g. stdin closed as the process
// exits).
func readSyncKeys(r io.Reader, cancel context.CancelFunc, detachRequested *atomic.Bool, togglePause func()) {
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if err != nil || n == 0 {
			return
		}
		switch buf[0] {
		case 3: // Ctrl+C
			fmt.Print("\r\n")
			cancel()
			return
		case 'd', 'D':
			if !daemonSupported {
				continue
			}
			detachRequested.Store(true)
			cancel()
			return
		case 'p', 'P':
			togglePause()
		}
	}
}

// liveWindow is an io.Writer that renders the tail of whatever is written to
// it as a fixed-height "window" of at most maxLines: once more lines have
// been written than that, the oldest is dropped instead of letting the
// terminal scroll, so content printed above it (the sync banner) always
// stays in view rather than scrolling out of sight.
type liveWindow struct {
	mu       sync.Mutex
	maxLines int
	// header holds fixed lines drawn above the log lines and repainted on
	// every redraw, so they can be updated in place (see setHeader) rather
	// than scrolling away like ordinary output.
	header []string
	lines  []string
	drawn  int // number of lines on screen from the previous redraw
}

func newLiveWindow(maxLines int) *liveWindow {
	return &liveWindow{maxLines: maxLines}
}

// setHeader replaces the fixed lines drawn above the log window and repaints
// straight away, so a caller can refresh them (e.g. the storage quota line)
// at any time without waiting for the next log write.
func (w *liveWindow) setHeader(lines []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.header = lines
	w.redrawLocked()
}

func (w *liveWindow) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lines = append(w.lines, strings.Split(strings.TrimRight(string(p), "\n"), "\n")...)
	if len(w.lines) > w.maxLines {
		w.lines = w.lines[len(w.lines)-w.maxLines:]
	}
	w.redrawLocked()
	return len(p), nil
}

// redrawLocked repaints the window in place: it moves the cursor up over the
// lines drawn by the previous call, clears each, and reprints the current
// buffer. Raw terminal mode (enabled by runSyncLoop while this is in use)
// disables output post-processing, so every line ends in "\r\n" rather than
// a bare "\n" to keep the cursor at column 0.
func (w *liveWindow) redrawLocked() {
	width := 0
	if wd, _, err := term.GetSize(os.Stdout.Fd()); err == nil {
		width = wd
	}

	var b strings.Builder
	if w.drawn > 0 {
		fmt.Fprintf(&b, "\033[%dA", w.drawn)
	}
	for _, lines := range [][]string{w.header, w.lines} {
		for _, line := range lines {
			b.WriteString("\033[2K\r")
			b.WriteString(truncateLine(line, width))
			b.WriteString("\r\n")
		}
	}
	w.drawn = len(w.header) + len(w.lines)
	os.Stdout.WriteString(b.String())
}

// truncateLine clips line to fit within width columns so that a single
// logical line never wraps onto a second terminal row, which would desync
// redrawLocked's cursor-up math against the actual number of screen rows
// used. ANSI colour escapes (which the header lines carry) are copied through
// without counting toward the width, so a coloured line is cut at the same
// visible column a plain one would be, and a reset is appended in case the
// cut landed mid-colour. Counting runes is only an approximation for
// wide/combining characters, but is good enough for the lines this window
// renders. A width of 0 means the terminal size couldn't be determined, so
// the line is left untouched.
func truncateLine(line string, width int) string {
	if width <= 0 || visibleWidth(line) <= width {
		return line
	}
	if width == 1 {
		return "…"
	}
	var b strings.Builder
	visible, inEscape, styled := 0, false, false
	for _, r := range line {
		switch {
		case inEscape:
			b.WriteRune(r)
			inEscape = r != 'm'
		case r == '\033':
			b.WriteRune(r)
			inEscape, styled = true, true
		case visible < width-1:
			b.WriteRune(r)
			visible++
		default:
			b.WriteString("…")
			if styled {
				b.WriteString(ansiReset)
			}
			return b.String()
		}
	}
	return b.String()
}

// visibleWidth counts the runes in line that occupy a terminal column,
// ignoring ANSI escape sequences.
func visibleWidth(line string) int {
	n, inEscape := 0, false
	for _, r := range line {
		switch {
		case inEscape:
			inEscape = r != 'm'
		case r == '\033':
			inEscape = true
		default:
			n++
		}
	}
	return n
}
