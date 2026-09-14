package main

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// logRingCap is the number of most-recent log lines kept in memory for the
// sync TUI's scrollback and search — deep enough to search back through a
// good chunk of history, capped so memory use stays bounded on a
// long-running sync.
const logRingCap = 1000

// logRing is a fixed-capacity circular buffer of log lines; once full, the
// oldest line is evicted to make room for each new one.
type logRing struct {
	lines [logRingCap]string
	start int
	count int
}

func (r *logRing) push(line string) {
	idx := (r.start + r.count) % logRingCap
	r.lines[idx] = line
	if r.count < logRingCap {
		r.count++
	} else {
		r.start = (r.start + 1) % logRingCap
	}
}

// all returns every buffered line, oldest first.
func (r *logRing) all() []string {
	out := make([]string, r.count)
	for i := 0; i < r.count; i++ {
		out[i] = r.lines[(r.start+i)%logRingCap]
	}
	return out
}

// logLineMsg carries one already-formatted log line (as produced by the
// standard log package, see tuiLogWriter) into the TUI's Update loop.
type logLineMsg string

// quotaMsg carries a refreshed storage quota into the TUI's Update loop; see
// syncEngine.onQuota.
type quotaMsg struct{ q *storageQuota }

// tuiLogWriter is an io.Writer that forwards each line written to it into a
// running sync TUI program, splitting on '\n' the same way liveWindow used
// to. Installed via log.SetOutput so every existing log.Printf call site
// across the binary keeps working unchanged.
type tuiLogWriter struct {
	prog *tea.Program
}

func (w *tuiLogWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		w.prog.Send(logLineMsg(line))
	}
	return len(p), nil
}

// searchHitStyle highlights a matched substring the way search hits are
// commonly shown in terminal UIs (reverse video), rather than relying on a
// specific foreground colour that might clash with a user's theme.
var searchHitStyle = lipgloss.NewStyle().Reverse(true).Bold(true)

// syncTUIModel is the Bubble Tea model behind the full-screen interactive
// sync view: a fixed top row (name/version, sync folder), a scrolling log
// pane in between fed by logLineMsg, and two fixed bottom rows (commands,
// storage), all separated by full-width dividers. It replaces the old
// liveWindow cursor-up repaint trick with a real alt-screen program, which
// also gets correct resize handling (tea.WindowSizeMsg) for free.
type syncTUIModel struct {
	version, folder string

	width, height int

	ring      logRing
	viewport  viewport.Model
	following bool // true = auto-scroll to the newest line as it arrives

	quota  *storageQuota
	paused bool

	searching  bool // the search input row is visible/focused
	search     textinput.Model
	matchCount int
	totalCount int

	// Outward signals — same contract the old readSyncKeys goroutine used,
	// so nothing downstream of runSyncLoop needs to change.
	cancel          context.CancelFunc
	detachRequested *atomic.Bool
	togglePause     func()
}

func newSyncTUIModel(version, folder string, cancel context.CancelFunc, detachRequested *atomic.Bool, togglePause func()) *syncTUIModel {
	search := textinput.New()
	search.Prompt = "/"
	search.CharLimit = 200

	return &syncTUIModel{
		version:         version,
		folder:          folder,
		following:       true,
		viewport:        viewport.New(0, 0),
		search:          search,
		cancel:          cancel,
		detachRequested: detachRequested,
		togglePause:     togglePause,
	}
}

func (m *syncTUIModel) Init() tea.Cmd { return nil }

func (m *syncTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		m.refreshContent()
		return m, nil

	case logLineMsg:
		m.ring.push(string(msg))
		m.refreshContent()
		return m, nil

	case quotaMsg:
		m.quota = msg.q
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m *syncTUIModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.searching {
		switch msg.String() {
		case "ctrl+c":
			m.cancel()
			return m, tea.Quit
		case "esc":
			m.searching = false
			m.search.Blur()
			m.search.SetValue("")
			m.following = true
			m.layout()
			m.refreshContent()
			return m, nil
		case "enter":
			// Leave search mode but keep the filtered results on screen,
			// frozen (not following), so they can be read/scrolled; '/'
			// re-opens the box to keep refining the same query.
			m.searching = false
			m.search.Blur()
			m.following = false
			m.layout()
			m.refreshContent()
			return m, nil
		}
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		m.refreshContent()
		return m, cmd
	}

	switch msg.String() {
	case "ctrl+c":
		m.cancel()
		return m, tea.Quit
	case "d", "D":
		if daemonSupported {
			m.detachRequested.Store(true)
			m.cancel()
			return m, tea.Quit
		}
	case "p", "P":
		m.togglePause()
		m.paused = !m.paused
		return m, nil
	case "/":
		m.searching = true
		m.following = true
		m.layout()
		m.refreshContent()
		return m, m.search.Focus()
	case "home":
		m.viewport.GotoTop()
		m.following = false
		return m, nil
	case "end":
		m.viewport.GotoBottom()
		m.following = true
		return m, nil
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	m.following = m.viewport.AtBottom()
	return m, cmd
}

// layout recomputes the viewport's dimensions from the terminal size and
// whether the search row is currently shown, so it always fills exactly the
// space left over by the fixed top row, dividers, search row and bottom two
// rows — called on every resize and whenever search is toggled.
func (m *syncTUIModel) layout() {
	fixed := 5 // top row + divider + divider + commands + storage
	if m.searching {
		fixed++ // search input row
	}
	h := m.height - fixed
	if h < 0 {
		h = 0
	}
	w := m.width
	if w < 0 {
		w = 0
	}
	m.viewport.Width = w
	m.viewport.Height = h

	// Reserve room after the input for the " N/M matches" counter appended
	// in searchRow — textinput.Width pads the value out to that many
	// columns, so without this reservation the counter gets pushed past the
	// terminal's right edge and truncated away.
	inputWidth := w - lipgloss.Width(m.search.Prompt) - searchCounterBudget
	if inputWidth < 1 {
		inputWidth = 1
	}
	m.search.Width = inputWidth
}

// searchCounterBudget is the columns reserved for searchRow's trailing
// " N/M matches" text — comfortably covers counts up to the 4-digit ring
// buffer capacity (" 1000/1000 matches" is 19 columns).
const searchCounterBudget = 20

// refreshContent rebuilds the viewport's content from the ring buffer,
// applying the active search filter (if any) and padding short content with
// leading blank lines so the log grows from the bottom of the pane upward
// rather than sitting pinned to the top.
func (m *syncTUIModel) refreshContent() {
	lines := m.ring.all()
	m.totalCount = len(lines)

	if query := m.search.Value(); query != "" {
		lines, m.matchCount = filterLines(lines, query)
	} else {
		m.matchCount = len(lines)
	}

	if pad := m.viewport.Height - len(lines); pad > 0 {
		lines = append(make([]string, pad), lines...)
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))
	if m.following {
		m.viewport.GotoBottom()
	}
}

// filterLines returns the lines matching query (case-insensitive substring,
// with the matched span highlighted), hiding the rest — similar to how fzf
// hides non-matching entries — and the number of matches.
func filterLines(lines []string, query string) (out []string, matched int) {
	q := strings.ToLower(query)
	out = make([]string, 0, len(lines))
	for _, line := range lines {
		if !strings.Contains(strings.ToLower(line), q) {
			continue
		}
		out = append(out, highlightMatches(line, query))
		matched++
	}
	return out, matched
}

// highlightMatches wraps every case-insensitive occurrence of query in line
// with searchHitStyle.
func highlightMatches(line, query string) string {
	lower, q := strings.ToLower(line), strings.ToLower(query)
	var b strings.Builder
	i := 0
	for {
		rel := strings.Index(lower[i:], q)
		if rel < 0 {
			b.WriteString(line[i:])
			return b.String()
		}
		start := i + rel
		end := start + len(q)
		b.WriteString(line[i:start])
		b.WriteString(searchHitStyle.Render(line[start:end]))
		i = end
	}
}

func (m *syncTUIModel) View() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	divider := strings.Repeat("─", m.width)

	var b strings.Builder
	b.WriteString(m.topRow())
	b.WriteByte('\n')
	b.WriteString(divider)
	b.WriteByte('\n')
	b.WriteString(m.viewport.View())
	b.WriteByte('\n')
	if m.searching {
		b.WriteString(m.searchRow())
		b.WriteByte('\n')
	}
	b.WriteString(divider)
	b.WriteByte('\n')
	b.WriteString(m.commandsLine())
	b.WriteByte('\n')
	b.WriteString(m.footerStorageLine())
	return b.String()
}

// topRow renders "Webbite Brick CLI vX.Y.Z" on the left and the synced folder
// on the right, ellipsizing the folder from the left (keeping its more
// identifying tail) if the two don't both fit.
func (m *syncTUIModel) topRow() string {
	title := "Webbite Brick CLI"
	left := fmt.Sprintf("%s v%s", title, m.version)
	budget := m.width - lipgloss.Width(left) - 1
	if budget < 0 {
		budget = 0
	}
	right := ellipsizeLeft(m.folder, budget)
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		gap = 0
	}
	styledLeft := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("226")).Render(title) +
		lipgloss.NewStyle().Bold(true).Render(fmt.Sprintf(" v%s", m.version))
	return styledLeft + strings.Repeat(" ", gap) + right
}

// ellipsizeLeft clips s to width columns, prefixing an ellipsis and keeping
// the tail when it's too wide — for a path, the end is more identifying
// than the start.
func ellipsizeLeft(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	r := []rune(s)
	keep := width - 1
	if keep > len(r) {
		keep = len(r)
	}
	return "…" + string(r[len(r)-keep:])
}

func (m *syncTUIModel) searchRow() string {
	counts := fmt.Sprintf(" %d/%d matches", m.matchCount, m.totalCount)
	return m.search.View() + counts
}

func (m *syncTUIModel) commandsLine() string {
	colored := ansiLightGreen + "Ctrl+C" + ansiReset + ": Stop"
	if daemonSupported {
		colored += " • " + ansiLightGreen + "D" + ansiReset + ": Detach as daemon"
	}
	colored += " • " + ansiLightGreen + "P" + ansiReset + ": Pause/resume"
	colored += " • " + ansiLightGreen + "/" + ansiReset + ": Search"
	if m.paused {
		colored += "  " + ansiOrange + "[paused]" + ansiReset
	}
	return fmt.Sprintf("%sCommands:%s %s", ansiPurple, ansiReset, colored)
}

func (m *syncTUIModel) footerStorageLine() string {
	return quotaLine(m.quota)
}
