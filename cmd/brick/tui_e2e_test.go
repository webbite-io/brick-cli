package main

import (
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// These tests drive syncTUIModel through a real Bubble Tea program — reading
// keystrokes off a pipe the way it reads a real terminal, rather than
// calling m.Update directly — so they exercise the actual production
// plumbing (including tuiLogWriter -> prog.Send) that the deadlock fixed in
// this commit lived in. TestSyncTUIModel* in tui_test.go cover the same key
// handling at the Update level; these confirm it also works end to end
// through the real event loop.

// startTUIForTest wires model up to a real *tea.Program with a pipe standing
// in for stdin, starts it running in the background, and gives it a moment
// to begin reading input and to receive an initial window size (real
// terminals get this automatically; the test pipe doesn't).
func startTUIForTest(t *testing.T, model *syncTUIModel) (prog *tea.Program, in *io.PipeWriter, done chan tea.Model) {
	t.Helper()

	inR, inW := io.Pipe()
	prog = tea.NewProgram(model, tea.WithInput(inR), tea.WithOutput(io.Discard))

	done = make(chan tea.Model, 1)
	go func() {
		fm, _ := prog.Run()
		done <- fm
	}()

	time.Sleep(100 * time.Millisecond)
	prog.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	time.Sleep(50 * time.Millisecond)

	return prog, inW, done
}

// waitForQuit waits for the program to quit, failing the test if it doesn't
// within timeout — the symptom a deadlocked event loop produces.
func waitForQuit(t *testing.T, done chan tea.Model, timeout time.Duration, msg string) *syncTUIModel {
	t.Helper()
	select {
	case fm := <-done:
		final, ok := fm.(*syncTUIModel)
		if !ok {
			t.Fatalf("unexpected final model type %T", fm)
		}
		return final
	case <-time.After(timeout):
		t.Fatal(msg)
		return nil
	}
}

// TestE2ECtrlCQuits presses Ctrl+C through the real event loop and checks
// the program actually quits and cancel() was called.
func TestE2ECtrlCQuits(t *testing.T) {
	var cancelled atomic.Bool
	model := newSyncTUIModel("1.2.3", "/tmp/somefolder", func() { cancelled.Store(true) }, new(atomic.Bool), func() {})
	_, in, done := startTUIForTest(t, model)

	if _, err := in.Write([]byte{3}); err != nil {
		t.Fatalf("write ctrl+c: %v", err)
	}
	waitForQuit(t, done, 2*time.Second, "ctrl+c did not quit the program within 2s")

	if !cancelled.Load() {
		t.Error("ctrl+c did not call cancel()")
	}
}

// TestE2EDetachKeySetsFlagAndQuits presses 'd' through the real event loop
// and checks it requests a daemon detach and quits.
func TestE2EDetachKeySetsFlagAndQuits(t *testing.T) {
	if !daemonSupported {
		t.Skip("daemon detach not supported on this platform")
	}
	var cancelled atomic.Bool
	var detach atomic.Bool
	model := newSyncTUIModel("1.2.3", "/tmp/somefolder", func() { cancelled.Store(true) }, &detach, func() {})
	_, in, done := startTUIForTest(t, model)

	if _, err := in.Write([]byte("d")); err != nil {
		t.Fatalf("write 'd': %v", err)
	}
	waitForQuit(t, done, 2*time.Second, "'d' did not quit the program within 2s")

	if !detach.Load() {
		t.Error("'d' did not set detachRequested")
	}
	if !cancelled.Load() {
		t.Error("'d' did not call cancel()")
	}
}

// TestE2EPauseKeyTogglesStateWithoutDeadlocking guards against a regression
// where pressing 'p' froze the sync TUI until killed: togglePause runs on
// the TUI's own Update goroutine and (via setPaused) calls log.Printf,
// which in interactive mode is routed through tuiLogWriter into prog.Send —
// a blocking send on the same goroutine that's the sole reader of that
// channel. It also checks the command actually took effect (m.paused flips,
// and the top bar/commands line reflect it), not just that the program
// stayed alive.
func TestE2EPauseKeyTogglesStateWithoutDeadlocking(t *testing.T) {
	var cancelled atomic.Bool
	model := newSyncTUIModel("1.2.3", "/tmp/somefolder", func() { cancelled.Store(true) }, new(atomic.Bool), nil)
	prog, in, done := startTUIForTest(t, model)

	// Wire log output the same way runSyncLoop does for the interactive
	// case: through tuiLogWriter -> prog.Send. togglePause here mirrors
	// syncEngine.setPaused calling log.Printf.
	tw := newTUILogWriter(prog)
	model.togglePause = func() { tw.Write([]byte("⏸ sync paused\n")) }

	if _, err := in.Write([]byte("p")); err != nil {
		t.Fatalf("write 'p': %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	if _, err := in.Write([]byte{3}); err != nil { // ctrl+c to end the test
		t.Fatalf("write ctrl+c: %v", err)
	}
	final := waitForQuit(t, done, 2*time.Second, "deadlock: program did not process ctrl+c after 'p' within 2s")

	if !final.paused {
		t.Error("'p' did not toggle model.paused to true")
	}
	if line := final.commandsLine(); !strings.Contains(line, "Resume sync") {
		t.Errorf("commandsLine() after pausing = %q, want it to offer Resume sync", line)
	}
	if row := final.topRow(); !strings.Contains(row, "Syncing is paused") {
		t.Errorf("topRow() after pausing = %q, want the paused notice", row)
	}
}

// TestE2ESearchKeyFiltersRing presses '/' through the real event loop, types
// a query, and checks matching/non-matching lines are filtered as expected.
func TestE2ESearchKeyFiltersRing(t *testing.T) {
	model := newSyncTUIModel("1.2.3", "/tmp/somefolder", func() {}, new(atomic.Bool), func() {})
	prog, in, done := startTUIForTest(t, model)

	for _, line := range []string{"downloaded a.md", "uploaded b.md", "remote poll error: timeout"} {
		prog.Send(logLineMsg(line))
	}
	time.Sleep(50 * time.Millisecond)

	if _, err := in.Write([]byte("/")); err != nil {
		t.Fatalf("write '/': %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := in.Write([]byte("error")); err != nil {
		t.Fatalf("write 'error': %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if _, err := in.Write([]byte{3}); err != nil { // ctrl+c to end the test
		t.Fatalf("write ctrl+c: %v", err)
	}
	final := waitForQuit(t, done, 2*time.Second, "search input did not process within 2s")

	if final.matchCount != 1 {
		t.Errorf("matchCount = %d, want 1 (only the error line matches)", final.matchCount)
	}
	view := final.viewport.View()
	if !strings.Contains(view, "poll error") {
		t.Errorf("viewport content = %q, want it to contain the matching line", view)
	}
	if strings.Contains(view, "downloaded a.md") {
		t.Errorf("viewport content = %q, want the non-matching line hidden", view)
	}
}
