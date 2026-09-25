package main

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestLogRingEvictsOldest(t *testing.T) {
	var r logRing
	for i := 0; i < logRingCap+10; i++ {
		r.push(fmt.Sprintf("line %d", i))
	}
	all := r.all()
	if len(all) != logRingCap {
		t.Fatalf("all() = %d lines, want capacity %d", len(all), logRingCap)
	}
	if want := "line 10"; all[0] != want {
		t.Errorf("oldest surviving line = %q, want %q (the first 10 should have been evicted)", all[0], want)
	}
	if want := fmt.Sprintf("line %d", logRingCap+9); all[len(all)-1] != want {
		t.Errorf("newest line = %q, want %q", all[len(all)-1], want)
	}
}

func TestLogRingBelowCapacity(t *testing.T) {
	var r logRing
	r.push("a")
	r.push("b")
	all := r.all()
	if got := strings.Join(all, ","); got != "a,b" {
		t.Errorf("all() = %q, want \"a,b\"", got)
	}
}

func TestFilterLinesHidesNonMatches(t *testing.T) {
	lines := []string{
		"downloaded Obsidian/Matinköp.md",
		"uploaded Obsidian/Webbite/brick-hq.md",
		"remote poll error: timeout",
	}
	out, matched := filterLines(lines, "upload")
	if matched != 1 {
		t.Fatalf("matched = %d, want 1", matched)
	}
	if len(out) != 1 {
		t.Fatalf("out = %v, want exactly the one matching line", out)
	}
	if !strings.Contains(out[0], "uploaded") {
		t.Errorf("out[0] = %q, want it to still contain the original text", out[0])
	}
}

func TestFilterLinesCaseInsensitive(t *testing.T) {
	_, matched := filterLines([]string{"Brick CLI disconnected"}, "DISCONNECTED")
	if matched != 1 {
		t.Errorf("matched = %d, want 1 (case-insensitive match)", matched)
	}
}

func TestFilterLinesNoMatch(t *testing.T) {
	out, matched := filterLines([]string{"a", "b", "c"}, "zzz")
	if matched != 0 || len(out) != 0 {
		t.Errorf("filterLines(no match) = %v, %d, want empty/0", out, matched)
	}
}

func TestHighlightMatchesWrapsEveryOccurrence(t *testing.T) {
	got := highlightMatches("foo foo bar", "foo")
	want := searchHitStyle.Render("foo") + " " + searchHitStyle.Render("foo") + " bar"
	if got != want {
		t.Errorf("highlightMatches = %q, want %q", got, want)
	}
}

func TestHighlightMatchesCaseInsensitive(t *testing.T) {
	got := highlightMatches("Disconnected", "disconnected")
	want := searchHitStyle.Render("Disconnected")
	if got != want {
		t.Errorf("highlightMatches = %q, want %q", got, want)
	}
}

func TestEllipsizeLeft(t *testing.T) {
	tests := []struct {
		name  string
		s     string
		width int
		want  string
	}{
		{"fits as-is", "short", 10, "short"},
		{"exact fit", "short", 5, "short"},
		{"zero width", "anything", 0, ""},
		{"single column", "anything", 1, "…"},
		{"keeps the tail, not the head", "/home/user/Documents/Obsidian", 15, "…ments/Obsidian"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ellipsizeLeft(tc.s, tc.width)
			if got != tc.want {
				t.Errorf("ellipsizeLeft(%q, %d) = %q, want %q", tc.s, tc.width, got, tc.want)
			}
		})
	}
}

func TestSyncTUIModelCtrlCCancelsAndQuits(t *testing.T) {
	var cancelled atomic.Bool
	m := newSyncTUIModel("1.2.3", "/sync/folder", func() { cancelled.Store(true) }, new(atomic.Bool), func() {})
	m.width, m.height = 80, 24

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !cancelled.Load() {
		t.Error("Ctrl+C did not call cancel()")
	}
	if cmd == nil {
		t.Fatal("Ctrl+C should return a tea.Quit command")
	}
}

func TestSyncTUIModelDetachSetsFlagAndCancels(t *testing.T) {
	if !daemonSupported {
		t.Skip("daemon detach not supported on this platform")
	}
	var cancelled atomic.Bool
	var detach atomic.Bool
	m := newSyncTUIModel("1.2.3", "/sync/folder", func() { cancelled.Store(true) }, &detach, func() {})
	m.width, m.height = 80, 24

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if !detach.Load() {
		t.Error("'d' did not set detachRequested")
	}
	if !cancelled.Load() {
		t.Error("'d' did not call cancel()")
	}
	if cmd == nil {
		t.Fatal("'d' should return a tea.Quit command")
	}
}

func TestSyncTUIModelPauseTogglesAndCallsCallback(t *testing.T) {
	var toggled int
	m := newSyncTUIModel("1.2.3", "/sync/folder", func() {}, new(atomic.Bool), func() { toggled++ })
	m.width, m.height = 80, 24

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if toggled != 1 || !m.paused {
		t.Fatalf("after one 'p': toggled=%d paused=%v, want 1/true", toggled, m.paused)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if toggled != 2 || m.paused {
		t.Fatalf("after two 'p': toggled=%d paused=%v, want 2/false", toggled, m.paused)
	}
}

func TestSyncTUIModelSearchFiltersRingAndEscRestores(t *testing.T) {
	m := newSyncTUIModel("1.2.3", "/sync/folder", func() {}, new(atomic.Bool), func() {})
	m.width, m.height = 80, 24
	m.layout()

	for _, line := range []string{"downloaded a.md", "uploaded b.md", "remote poll error: timeout"} {
		m.Update(logLineMsg(line))
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if !m.searching {
		t.Fatal("'/' did not enter search mode")
	}

	for _, r := range "error" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if m.matchCount != 1 {
		t.Fatalf("matchCount = %d, want 1 (only the error line matches)", m.matchCount)
	}
	if !strings.Contains(m.viewport.View(), "poll error") {
		t.Errorf("viewport content = %q, want it to contain the matching line", m.viewport.View())
	}
	if strings.Contains(m.viewport.View(), "downloaded a.md") {
		t.Errorf("viewport content = %q, want the non-matching line hidden", m.viewport.View())
	}

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.searching {
		t.Error("Esc did not exit search mode")
	}
	if m.search.Value() != "" {
		t.Errorf("search query = %q after Esc, want cleared", m.search.Value())
	}
	if !strings.Contains(m.viewport.View(), "downloaded a.md") {
		t.Error("Esc did not restore the full unfiltered log")
	}
}

func TestSyncTUIModelWindowResizeRelayoutsViewport(t *testing.T) {
	m := newSyncTUIModel("1.2.3", "/sync/folder", func() {}, new(atomic.Bool), func() {})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if m.viewport.Width != 100 {
		t.Errorf("viewport.Width = %d, want 100", m.viewport.Width)
	}
	// 5 fixed rows: top row, two dividers, commands, storage.
	if want := 30 - 5; m.viewport.Height != want {
		t.Errorf("viewport.Height = %d, want %d", m.viewport.Height, want)
	}
}

func TestCommandsLineShowsPauseOrResumeLabel(t *testing.T) {
	m := newSyncTUIModel("1.2.3", "/sync/folder", func() {}, new(atomic.Bool), func() {})
	m.width, m.height = 80, 24

	// The command letter itself is ANSI-colored separately from the label
	// (e.g. "...\x1b[...mP\x1b[0m: Pause sync"), so check for the ": <label>"
	// tail rather than a "P: <label>" substring split by escape codes.
	line := m.commandsLine()
	if !strings.Contains(line, ": Pause sync") {
		t.Errorf("commandsLine() while running = %q, want it to contain %q", line, ": Pause sync")
	}
	if strings.Contains(line, "Resume sync") {
		t.Errorf("commandsLine() while running = %q, should not mention Resume yet", line)
	}

	m.paused = true
	line = m.commandsLine()
	if !strings.Contains(line, ": Resume sync") {
		t.Errorf("commandsLine() while paused = %q, want it to contain %q", line, ": Resume sync")
	}
	if strings.Contains(line, "Pause sync") {
		t.Errorf("commandsLine() while paused = %q, should not still say Pause sync", line)
	}
}

func TestTopRowShowsPausedNoticeOnlyWhenPaused(t *testing.T) {
	m := newSyncTUIModel("1.2.3", "/sync/folder", func() {}, new(atomic.Bool), func() {})
	m.width, m.height = 80, 24

	row := m.topRow()
	if strings.Contains(row, "Syncing is paused") {
		t.Errorf("topRow() while running = %q, should not show the paused notice", row)
	}

	m.paused = true
	row = m.topRow()
	if strings.Contains(row, "\n") {
		t.Fatalf("topRow() must be a single line, got %q", row)
	}
	if !strings.Contains(row, "⚠️ Syncing is paused") {
		t.Errorf("topRow() while paused = %q, want it to contain the warning notice", row)
	}
	if verIdx, noticeIdx := strings.Index(row, "v1.2.3"), strings.Index(row, "⚠️"); noticeIdx < verIdx {
		t.Errorf("paused notice must appear after the version in topRow(), got %q", row)
	}
	if !strings.Contains(row, "v1.2.3 · ⚠️") {
		t.Errorf("topRow() = %q, want a \" · \" separator directly between the version and the notice", row)
	}
}

func TestSyncTUIModelTopRowEllipsizesFolder(t *testing.T) {
	m := newSyncTUIModel("1.2.3", "/very/long/path/that/does/not/fit/on/screen", func() {}, new(atomic.Bool), func() {})
	m.width, m.height = 40, 24
	row := m.topRow()
	if strings.Contains(row, "\n") {
		t.Fatalf("topRow() must be a single line, got %q", row)
	}
	if !strings.Contains(row, "…") {
		t.Errorf("topRow() = %q, want the folder ellipsized to fit", row)
	}
}
