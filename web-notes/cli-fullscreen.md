# Full-screen sync TUI — implementation plan

## Goal

Replace the current interactive sync-mode renderer (a "fake full screen" that
repaints a fixed-height scrolling window via ANSI cursor-up tricks) with a
real full-screen terminal UI:

- Fixed top row: `Brick` name + version (left), synced folder (right,
  ellipsized from the left if too wide).
- Fixed horizontal dividers directly below the top row and directly above the
  bottom two rows.
- A scrollable, growing-from-the-bottom log area in between, holding the last
  **1000** lines in memory (currently capped at 10 visible / unbounded
  history only in real terminal scrollback).
- Fixed bottom two rows: the existing `Commands: ...` and `Storage: ...`
  lines.
- All existing keyboard shortcuts keep working: `Ctrl+C` stop, `D` detach as
  daemon, `P` pause/resume.
- New: `/` opens a search box; typed text filters the 1000-line buffer
  fzf-style (non-matching rows hidden, matches highlighted), `Esc` exits
  search back to live tailing.
- Correct resize handling — no more relying on `term.GetSize` being polled
  only when a line happens to be written.

Scope is limited to interactive sync mode. The non-interactive paths (piped
output, `--background`/detached daemon child, `--self-test`,
`--setup-and-exit`) must keep behaving exactly as they do today — this is a
foreground-TTY-only feature, same as the current live-window banner is.

## Current implementation (for reference)

- `cmd/brick/live.go` — everything about the interactive banner:
  - `printSyncBanner` prints the one-line "I'm Webbite Brick CLI vX,
    syncing ..." banner (interactive and non-interactive variants).
  - `syncHeaderLines`/`quotaLine` render the `Commands:` / `Storage:` /
    separator block as plain strings.
  - `readSyncKeys` reads raw single-byte keypresses off stdin
    (`Ctrl+C` = 3, `d`/`D`, `p`/`P`) and calls `cancel()` /
    `detachRequested.Store(true)` / `togglePause()`.
  - `liveWindow` is an `io.Writer` that keeps only the last
    `liveWindowSize` (10) lines, repaints in place with `\033[%dA` cursor-up
    + `\033[2K\r` per-line clear on every `Write`, and caps what it shows to
    whatever fits under the terminal height via `term.GetSize` (checked only
    at redraw time, i.e. only when something is written — a resize with no
    new log output doesn't get picked up until the next line arrives).
  - `truncateLine`/`visibleWidth` do ANSI-aware line clipping so a line never
    wraps to a second terminal row (required for the cursor-up math to stay
    correct).
- `cmd/brick/sync.go` (`runSyncLoop`, ~line 2170–2476) wires it together:
  - `interactive := !background && term.IsTerminal(stdin) && term.IsTerminal(stdout)`.
  - If interactive: puts stdin into raw mode (`term.MakeRaw`, from
    `github.com/charmbracelet/x/term`), constructs a `liveWindow`, points
    `eng.onQuota` at `win.setHeader(syncHeaderLines(q))` so a quota refresh
    repaints the footer, and — critically — calls `log.SetOutput(win)`. All
    sync log lines go through the **standard `log` package**
    (`log.Printf(...)`), so redirecting `log`'s output is the entire
    integration seam; no call site needs to change.
  - Log line producers (plain text, no per-line ANSI color today):
    `cmd/brick/sync.go:1629` `↓ downloaded %s`,
    `cmd/brick/sync.go:1691`/`1710` `↑ uploaded %s`,
    `cmd/brick/agent.go:755` `Brick CLI connected to Brick Online`,
    `cmd/brick/agent.go:796` `Brick CLI disconnected (...); reconnecting in ...`,
    `cmd/brick/sync.go:2301/2366/2419/2458` various `... error: %v`.
  - `togglePause` flips `eng.paused` (`atomic.Bool`) and wakes the debounce
    worker on resume.
  - On `Ctrl+C`/`D`, `readSyncKeys` calls `cancel()`, which unblocks
    `<-ctx.Done()` at `sync.go:2464`; `cleanupInteractive()` restores the
    terminal (`term.Restore`) and `log.SetOutput(os.Stderr)` *before*
    `eng.printSummary()` is printed with plain `\n` — raw mode disables
    output post-processing, so this ordering matters.
  - Detach (`D`) just sets `detachRequested`; the caller above
    `runSyncLoop` (in `runStorageSync`) is what actually re-execs/spawns the
    background daemon process — terminal must be fully restored first,
    which it already is by the time `runSyncLoop` returns.
- `cmd/brick/live_test.go` covers `quotaLine`, `syncHeaderLines`,
  `truncateLine`, `availableLines`, `visibleWidth` — these test pure
  string-formatting functions, not terminal I/O, so most either port
  directly or get superseded by the new renderer's own tests.
- `cmd/brick/sync_pause_test.go` covers `eng.paused`/`reconcileAll`
  pause-mid-pass behavior — unaffected by this change, no updates needed.

## Dependency choice: Bubble Tea

`go.mod` already pulls in the full Charm stack **transitively** (via
`github.com/charmbracelet/huh`, used for onboarding prompts):

```
github.com/charmbracelet/bubbletea v1.3.6   // indirect
github.com/charmbracelet/bubbles  v0.21.1-...
github.com/charmbracelet/lipgloss v1.1.0    // indirect
github.com/charmbracelet/x/term   v0.2.1    // already a direct dep, used today
```

Recommendation: use **Bubble Tea** (`tea.Program`) for the full-screen sync
UI, with `bubbles/viewport` for the scrolling log pane and
`bubbles/textinput` for the search box, styled with `lipgloss`. This adds no
new third-party dependency (just promotes existing indirect ones to direct
in `go.mod`), fits the existing `Write`-based `log` integration almost
unchanged, and gives us `tea.WindowSizeMsg` for correct resize handling for
free (Bubble Tea already listens for `SIGWINCH` internally on unix and
polls appropriately on Windows via `x/term`).

Alternative considered: `gdamore/tcell` / a hand-rolled ANSI renderer
(extending `liveWindow`). Rejected — would mean hand-writing viewport
scrolling, search-input handling, and resize plumbing that Bubble Tea/Bubbles
already provide, for no benefit since the Charm stack is already vendored.

## New file layout

- `cmd/brick/tui.go` (or `cmd/brick/synctui.go`) — new package-local file
  holding the Bubble Tea model, replacing `liveWindow`,
  `syncHeaderLines`/`quotaLine` rendering (logic reused, output target
  changes), and `readSyncKeys`.
- `cmd/brick/live.go` — keep `printSyncBanner`'s non-interactive branch,
  `quotaLine`'s pure formatting helper (reused by the new footer), and
  `ansi*` color constants. Remove `liveWindow`, `readSyncKeys`,
  `truncateLine`/`visibleWidth`/`availableLines` (superseded by
  viewport/lipgloss) once the new renderer is in and its own tests exist.
- `cmd/brick/live_test.go` — trim to what's kept (`quotaLine`,
  `TestQuotaLineThresholds`/`TestQuotaLineOmitted`); add
  `cmd/brick/tui_test.go` for the new model's pure logic (see Testing below).

## Model design

```go
type syncTUIModel struct {
    // static
    version, folder string

    // layout
    width, height int

    // log content
    ring       *logRing         // capped ring buffer, size 1000
    viewport   viewport.Model   // bubbles/viewport, renders the visible slice
    following  bool             // true = auto-scroll to bottom on new lines

    // footer
    quota  *storageQuota
    paused bool

    // search
    searching bool
    search    textinput.Model
    matches   []int            // indices into ring matching the query

    // outward signals (same contract callers already use today)
    cancel          context.CancelFunc
    detachRequested *atomic.Bool
    togglePause     func()
}
```

### Message types (fed in from other goroutines via `Program.Send`)

- `logLineMsg string` — one already-formatted log line (what `log.Printf`
  produced, timestamp included, same as today).
- `quotaMsg *storageQuota` — sent from `eng.onQuota`, same trigger as today.
- (Bubble Tea's built-in `tea.WindowSizeMsg` and `tea.KeyMsg` cover resize
  and input; no custom messages needed for those.)

### `Update`

- `tea.WindowSizeMsg`: store `width`/`height`, recompute viewport dimensions
  (`height - 1 (top row) - 1 (top divider) - 2 (bottom rows) - 1 (bottom
  divider)`, minus 1 more row for the search input when `searching`).
- `logLineMsg`: append to ring buffer (evict oldest past 1000); if
  `searching`, re-run the filter (a fresh line might match); if not
  searching and `following`, append to viewport content and
  `viewport.GotoBottom()`.
- `quotaMsg`: store, re-render footer line (footer is drawn directly in
  `View`, not via the viewport, so this is just a state update).
- `tea.KeyMsg`, not searching:
  - `ctrl+c`: call `cancel()`, return `tea.Quit`.
  - `d`/`D` (if `daemonSupported`): `detachRequested.Store(true)`,
    `cancel()`, return `tea.Quit`.
  - `p`/`P`: call `togglePause()` (same closure as today — flips
    `eng.paused`, wakes debounce worker on resume); flip local `paused` for
    footer display.
  - `/`: enter search mode — `searching = true`, focus `search` textinput,
    resize viewport to make room for the input row.
  - arrow/PgUp/PgDn/Home/End: forward to `viewport.Update` (manual scroll
    sets `following = false` when not already at the bottom; landing back at
    the bottom via scroll or `End` sets `following = true` again — mirrors
    normal `tail -f` / pager behavior).
- `tea.KeyMsg`, searching:
  - `esc`: clear query, `searching = false`, restore full buffer to
    viewport, `viewport.GotoBottom()`, `following = true`.
  - `enter`: leave search mode but keep the filtered result set frozen in
    the viewport (so it can be read/scrolled); `/` again re-opens search
    (prefilled with the last query) or `esc` returns to live tailing. This
    mirrors how `less`/`fzf` treat "confirm and stop typing" vs. "cancel."
  - any other key: forward to `search` textinput, then re-run the filter
    against `ring` and rebuild the viewport content from `matches`.
  - `Ctrl+C` still quits even while searching (safety net — don't trap the
    user in search mode).

### `View`

```
<top row: "Brick vX.Y.Z" ... ellipsized "…/path/to/folder">
<divider, width-sized>
<viewport content — log lines, or filtered+highlighted matches, growing
 bottom-up, blank-padded above if fewer than fit>
[<search box row, only while searching: "/query_"  •  N/1000 matches>]
<divider, width-sized>
<Commands: ...>
<Storage: ...>
```

- Top row: `lipgloss` horizontal join, left segment `Brick v{Version}`,
  right segment the sync folder path. If `left+right` wider than `width`,
  ellipsize the **folder** from the left (`…/deeply/nested/folder` — the
  end of the path is more identifying than the start), same rationale as
  `truncateLine` today but applied to the path specifically per the spec
  ("with ... ellipses to the left if too wide").
- Dividers: `strings.Repeat("─", width)`, full terminal width (today's
  divider is sized to the commands/storage line, not full width, because
  it's not truly full-screen; now it can just be `width`).
- Bottom two rows: reuse `quotaLine`'s formatting logic as-is
  (`cmd/brick/live.go`), just rendered by `View` instead of
  `liveWindow.setHeader`.
- Search-hit highlighting: for each matched line, wrap the matched
  substring(s) in a `lipgloss.NewStyle().Reverse(true)` (or a bright
  background color) span — the common "search hit" look terminals/editors
  use (e.g. `fzf`'s cyan-on-match, `less`'s reverse-video). Case-insensitive
  substring match for v1 (see Open questions for fuzzy matching as a
  stretch goal).

### Log ring buffer

Note: `syncEngine` already has a separate bounded feed —
`ctrlActivity`/`publishActivity`/`recentActivity` (`sync.go:647-710`,
capped at `controlActivityCap = 200`) — backing the control API's
`/v1/activity` endpoint (`controlapi.go:213-221`). That feed only covers
file-level events (`download`, `upload`, `trash`, `move`, `remove`,
`keep-both`, `update`) and is structured (`kind`, `relPath`, `at`), not
free text. It does **not** cover connection/reconnect messages
(`agent.go:755/796`), watcher/poll/sync errors, or pause/resume — all of
which the current banner *does* show and which the spec's "log growing
from the bottom up" is expected to keep showing. So the new 1000-line ring
buffer should stay sourced from the `log.SetOutput` interception (a
superset of everything users see today), not from `ctrlActivity` — the two
remain separate, unrelated feeds serving different consumers (control-API
JSON vs. this on-screen log).

```go
type logRing struct {
    lines [1000]string
    start, count int
}
func (r *logRing) push(line string)
func (r *logRing) all() []string // oldest-to-newest, count entries
```

Simple fixed-size circular buffer; no locking needed if only ever touched
from the Bubble Tea `Update` goroutine (all writers go through
`Program.Send`, which is safe to call from other goroutines and serializes
into `Update`).

### Search filtering

- v1: case-insensitive substring match against each ring line, computed
  fresh over the full 1000-line buffer on every keystroke (1000 short-string
  scans is trivial, no need to optimize/debounce).
- Hide non-matches entirely (fzf-style), preserve chronological order,
  header shows `N/1000 matches` (or `N/{count}` if the buffer hasn't filled
  yet) next to the search input, same idea as `fzf`'s match counter.

## Wiring into `runSyncLoop` (`cmd/brick/sync.go`)

Replace the `interactive` block currently at `sync.go:2233–2252`:

1. Build `model := newSyncTUIModel(Version, folder, &detachRequested, cancel, togglePause)`.
2. `program := tea.NewProgram(model, tea.WithAltScreen())` — alt-screen mode
   is what makes this genuinely full-screen (separate terminal buffer,
   restored automatically on exit) instead of the current in-place
   scrollback repaint trick.
3. `log.SetOutput(newTUILogWriter(program))` — a small `io.Writer` whose
   `Write` splits on `\n` and calls `program.Send(logLineMsg(line))` per
   line. This is the *only* required change on the "producer" side — every
   existing `log.Printf(...)` call site is untouched.
4. `eng.onQuota = func(q *storageQuota) { program.Send(quotaMsg(q)) }`.
5. Run the program: since `runSyncLoop` has other concurrent goroutines
   (watcher, poll ticker, agent server) that must keep running and must be
   able to `cancel()` the shared `ctx` independent of the TUI, run
   `program.Run()` in its own goroutine and wait on it the same way the code
   today waits on `<-ctx.Done()` — i.e. `program.Run()` returning (whether
   from `Ctrl+C`/`D` inside the model, or from an external `cancel()`, e.g.
   the `SIGTERM` handler at `sync.go:2181-2187` or a fatal error elsewhere)
   is the new "the interactive session is over" signal. On external
   cancellation, send `program.Quit()`/`program.Send(tea.Quit)` so the alt
   screen still gets torn down cleanly rather than left dangling.
6. No more manual `term.MakeRaw`/`term.Restore` — Bubble Tea's alt-screen +
   raw-input handling owns that lifecycle; `cleanupInteractive` shrinks to
   just `log.SetOutput(os.Stderr)`.
7. Detach (`D`) and Ctrl+C keep working exactly as observed externally:
   `detachRequested`/`cancel()` are the same variables `runStorageSync`
   already inspects after `runSyncLoop` returns — nothing downstream of
   `runSyncLoop` needs to change.
8. Non-interactive branch (`!interactive`) is untouched: no Bubble Tea
   program is created, `log` keeps writing to its default/`os.Stderr`
   destination, exactly as today.

## Resize handling

Bubble Tea delivers `tea.WindowSizeMsg` on start and on every terminal
resize (via `SIGWINCH` on unix, polled equivalent on Windows through
`x/term`) — this directly replaces the current approach of only calling
`term.GetSize` reactively inside `redrawLocked` when a line happens to be
written. The new `Update` handler recomputes the viewport height and
re-wraps the top-row ellipsis on every such message, so resizing while
idle (no new log lines) now repaints correctly, which it does not today.

## Things explicitly preserved / not touched

- The pause/resume semantics in `syncEngine` (`eng.paused`, `setPaused`,
  `errPausedMidPass`) — only the key handler that calls `togglePause()`
  moves, the engine-side logic is unchanged.
- Daemon detach mechanics in `daemon_unix.go`/`daemon_json.go` — unaffected;
  they only ever observed `detachRequested`, never touched terminal state.
- `daemonSupported` gating of the `D` shortcut (Windows has no daemon
  support today) — same conditional, just moved into the new `Update`.
- Non-interactive/background/self-test/setup-and-exit code paths — zero
  changes; `interactive` still gates whether any of this new code runs at
  all.
- Log line *content* and the standard `log` package as the producer API —
  unchanged; every existing `log.Printf` call site anywhere in the codebase
  keeps working without modification.

## Testing plan

- Unit tests (`cmd/brick/tui_test.go`), no real terminal needed:
  - `logRing`: push past capacity evicts oldest, `all()` returns
    oldest→newest, capacity exactly 1000.
  - Search filter: substring matching (case-insensitivity, no match →
    empty, highlighting span offsets correct including multi-byte/unicode
    lines like the `↓`/`↑` markers already in real log lines).
  - Top-row folder ellipsis: exact width-fit boundary cases (mirrors
    today's `TestTruncateLine` cases, but for left-side ellipsis).
  - `quotaLine` threshold tests (`TestQuotaLineThresholds`,
    `TestQuotaLineOmitted`) port unchanged — pure function, untouched.
  - Bubble Tea models are plain `tea.Model` (`Init`/`Update`/`View`), so
    `Update` can be exercised directly with synthetic `tea.KeyMsg`/
    `tea.WindowSizeMsg`/`logLineMsg` values and `View()` asserted against
    expected strings without spinning up a real program — no PTY needed for
    core logic coverage.
- Manual/interactive verification (can't be meaningfully unit-tested):
  - Run `brick sync` in a real terminal (and inside `tmux`, which is a good
    resize-stress test): confirm alt-screen enter/exit is clean, resize
    while idle and while log lines are streaming, `Ctrl+C`/`D`/`P` all
    behave as before, `/` search hides/highlights/updates live, `Esc`
    returns to tailing, over-1000-lines behavior (oldest scroll out of the
    buffer, search no longer finds them).
  - Confirm non-interactive paths are unaffected: `brick sync | cat`,
    `brick sync --background`, `brick --self-test`, `brick --setup-and-exit`.
  - Confirm terminal is left in a sane state after every exit path
    (`Ctrl+C`, `D`, `SIGTERM`, a fatal error mid-loop) — no leftover
    alt-screen, no lost cursor, no raw mode stuck on (this was already a
    sharp edge in the current code, per the ordering comment at
    `sync.go:2466-2471`; Bubble Tea's `tea.WithAltScreen()` teardown needs
    the same scrutiny on every exit path, including a panic).

## Rollout

Straightforward in-place replacement (single binary, no config/schema
migration, no server-side coordination) — no need for a flag or gradual
rollout. Land it as one PR that:

1. Adds the new `cmd/brick/tui.go` + `tui_test.go`.
2. Rewires `runSyncLoop`'s interactive branch to use it.
3. Deletes the now-dead parts of `cmd/brick/live.go`/`live_test.go`
   (`liveWindow`, `readSyncKeys`, `truncateLine`, `availableLines`,
   `visibleWidth`) once the new path is confirmed working end-to-end.
4. Promotes `bubbletea`, `lipgloss` (and `bubbles/viewport`,
   `bubbles/textinput` if not already used) from indirect to direct in
   `go.mod`/`go.sum` (automatic via `go mod tidy` once imported).

## Open questions (for a follow-up decision, not blocking the plan)

- **Enter vs. Esc semantics while searching** — proposed above (`Enter`
  freezes the filtered view, `Esc` always cancels back to live tail).
  Worth confirming this matches user expectations before implementing,
  since it's the one genuinely new interaction (everything else preserves
  existing behavior).
- **Fuzzy vs. substring search** — the spec says "similar to how fzf does
  it" specifically about *hiding non-matching rows*, not necessarily fzf's
  fuzzy-match algorithm. Proposed v1 is plain case-insensitive substring
  matching (simpler, predictable, easy to highlight correctly); fuzzy
  matching can be layered in later without changing the surrounding UI if
  desired.
- **Per-line coloring by event type** — not in the original spec, but since
  the new renderer already classifies nothing today (log lines are plain
  text), it would be low-cost to color by the existing emoji/prefix
  conventions already used across log messages (`↓` download, `↑` upload,
  `🗑` trash/remove, `→` move, `⏸`/`▶` pause/resume, `⚠` warnings,
  `error`/`disconnected` text) — mirroring the existing `ansiRed`/
  `ansiOrange` palette used elsewhere — while the search-highlight work is
  already touching line rendering. Flagging as an easy adjacent win, not
  required for parity.
