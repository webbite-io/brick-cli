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
)

// printSyncBanner prints the line(s) shown just before the sync loop starts.
// In an interactive terminal it prints a colored "Commands" line listing the
// available keyboard shortcuts, followed by a separator sized to match;
// otherwise (redirected output, or the detached daemon child) it falls back
// to the original plain one-liner.
func printSyncBanner(folder string, interactive bool) {
	if !interactive {
		fmt.Printf("Syncing %s with the Storage API. Press Ctrl+C to stop.\n", folder)
		return
	}

	fmt.Printf("Syncing %s with the Storage API.\n", folder)

	plain := "Ctrl+C: Stop"
	colored := ansiDarkGreen + "Ctrl+C" + ansiReset + ": Stop"
	if daemonSupported {
		plain += " • D: Detach as daemon"
		colored += " • " + ansiDarkGreen + "D" + ansiReset + ": Detach as daemon"
	}
	plain += " • P: Pause/resume"
	colored += " • " + ansiDarkGreen + "P" + ansiReset + ": Pause/resume"

	const prefix = "Commands: "
	fmt.Printf("%sCommands:%s %s\n", ansiPurple, ansiReset, colored)
	fmt.Println(strings.Repeat("=", len(prefix)+len(plain)))
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
	lines    []string
	drawn    int // number of lines on screen from the previous redraw
}

func newLiveWindow(maxLines int) *liveWindow {
	return &liveWindow{maxLines: maxLines}
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
	for _, line := range w.lines {
		b.WriteString("\033[2K\r")
		b.WriteString(truncateLine(line, width))
		b.WriteString("\r\n")
	}
	w.drawn = len(w.lines)
	os.Stdout.WriteString(b.String())
}

// truncateLine clips line to fit within width columns so that a single
// logical line never wraps onto a second terminal row, which would desync
// redrawLocked's cursor-up math against the actual number of screen rows
// used. Counting runes is only an approximation for wide/combining
// characters, but is good enough for the plain-text log lines this window
// renders. A width of 0 means the terminal size couldn't be determined, so
// the line is left untouched.
func truncateLine(line string, width int) string {
	if width <= 0 {
		return line
	}
	r := []rune(line)
	if len(r) <= width {
		return line
	}
	if width <= 1 {
		return string(r[:width])
	}
	return string(r[:width-1]) + "…"
}
