package vterm

import (
	"fmt"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Pane-local history and viewport.
//
// A real terminal keeps the lines that scroll off the top of its primary
// screen and lets the user scroll back over them; the alternate screen has no
// such history, and a full-screen program running there scrolls its own
// content. amux mirrors a pane through an emulator, so the outer terminal's
// scrollback never sees the agent's output — this file gives each pane that
// history itself, with the same semantics:
//
//   - Primary screen. History is exactly the rows that physically scroll off the
//     top of the primary screen: a line feed, index or next-line at the bottom
//     margin, scroll-up (SU), or delete-line (DL) inside a full-width scrolling
//     region. That covers a shell, any program printing a transcript, and
//     Codex's inline transcript mode, which inserts each committed line above
//     its viewport with DECSTBM 1..top plus "\r\n" (codex-rs insert_history.rs)
//     — each inserted row scrolls off row 0 and is captured once, losslessly.
//     Rows the emulator captures keep the width they had at the time; they are
//     physical rows with no soft-wrap metadata and are not reflowed on resize
//     (a narrower pane truncates them, a wider one pads).
//   - Repaints never enter history. Programs that redraw with cursor
//     positioning (every ratatui/Ink frame, Codex's spinner and composer) touch
//     cells, not the scroll position, so nothing is captured. Erase-display 2
//     (ESC[2J, ratatui's clear, Codex's viewport change, a shell `clear`) is
//     overridden to erase without saving the screen to history, as xterm does;
//     the emulator's default would copy every non-blank row into scrollback on
//     each clear-and-repaint cycle and duplicate history. ESC[3J still clears
//     history, which is what `clear` means by it.
//   - Alternate screen: app-owned. No amux history is kept for it and
//     ScrollLines reports no change while it is active, so the embedder falls
//     back to the program's own scrolling: raw mouse events when it tracks the
//     mouse (MouseTracked), alternate-scroll cursor keys otherwise (MouseEvent).
//     This is what real Codex does in its forced alternate-screen mode
//     (tui.alternate_screen = "always", PR #132): a probe of Codex 0.157.1 shows
//     it enables mouse modes 1000/1002/1003/1006 and redraws an in-app
//     transcript inside synchronized-output frames with zero scroll operations,
//     so there are no scrolled-off rows to capture, and a frame-diff
//     reconstruction would be lossy (frames skip rows when streaming outpaces
//     the frame rate) and could not tell a repeated row from a re-shown one.
//     For amux-owned scrolling of Codex output, run Codex in its inline
//     transcript mode: the primary-screen rule above then captures every line.
//   - History is bounded (SetHistoryLimit, DefaultHistoryLines) and the primary
//     scrollback is the only buffer; the alternate screen's own scrollback is
//     never read.
//
// The viewport is an offset into that history: 0 is live, n shows the n newest
// history rows above the live screen's top rows (Render, Snapshot). It returns
// to live on its own whenever the screen it is looking at stops being the right
// one: new output that scrolls rows into history (a cursor-only repaint keeps
// the offset), input from the user (Write), a resize, the program entering the
// alternate screen, or a reset. RIS (ESC c) also clears history: the daemon
// sends it (Reset) before replaying a recent tail of output the history has
// already absorbed, so keeping the old rows would duplicate every line the
// replay scrolls.

// DefaultHistoryLines is the per-pane history bound unless SetHistoryLimit
// changes it. tmux's default history-limit; a row costs about its visible width
// in cells (trailing blanks are not stored), so a busy 200-column pane tops out
// around 40 MiB of cells and typical ones far less.
const DefaultHistoryLines = 2000

// ViewportState describes where the viewport is, for a UI that routes wheel and
// key events and draws its own affordances.
type ViewportState struct {
	// Offset is how many history rows the viewport is scrolled back; 0 is live.
	Offset int
	// HistoryLen is how many history rows exist to scroll over.
	HistoryLen int
	// Cols and Rows are the pane's current size, the shape of Render/Snapshot.
	Cols, Rows int
	// AltScreen reports that the program is on the alternate screen, where
	// history scrolling is unavailable and the program scrolls itself.
	AltScreen bool
	// Revision changes whenever what Render/Snapshot would show may have
	// changed: output, resize, scroll, reset.
	Revision uint64
}

// Snapshot is a detached copy of the cells Render would draw: Rows rows of Cols
// cells each, cell (x, y) at Cells[y][x], without the scroll indicator overlay.
// Rows are physical (no wrap metadata); a wide character occupies its first
// cell with Width 2 followed by a zero Cell placeholder. Styles and links are
// copied by value, so the snapshot stays valid after the pane moves on.
type Snapshot struct {
	Cols, Rows int
	Offset     int
	HistoryLen int
	AltScreen  bool
	Revision   uint64
	Cells      [][]uv.Cell
}

// Text returns row y as plain text with trailing blanks trimmed, or "" out of
// range.
func (s Snapshot) Text(y int) string {
	if y < 0 || y >= len(s.Cells) {
		return ""
	}
	return uv.Line(s.Cells[y]).String()
}

// histMark is the observable shape of the primary scrollback: its length and
// the identity of its two newest rows. Growth is a change in any of them —
// length alone stops moving once the ring is full, but every push replaces the
// newest row with a fresh clone (two consecutive blank rows share the empty
// slice, which is why the second-newest is compared too).
type histMark struct {
	n            int
	newest, prev uv.Line
}

func sameLine(a, b uv.Line) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

func (m histMark) equal(o histMark) bool {
	return m.n == o.n && sameLine(m.newest, o.newest) && sameLine(m.prev, o.prev)
}

// markHistory samples the primary scrollback; the caller holds t.mu.
func (t *Terminal) markHistory() histMark {
	sb := t.emu.Scrollback()
	n := sb.Len()
	return histMark{n: n, newest: sb.Line(n - 1), prev: sb.Line(n - 2)}
}

// installHistoryHooks bounds the primary scrollback and layers the two
// sequence handlers the policy above needs over the emulator's defaults. Both
// run synchronously inside emu.Write, under t.mu, like noteMode.
func (t *Terminal) installHistoryHooks() {
	t.emu.SetScrollbackSize(DefaultHistoryLines)
	// ED 2 as xterm does it: erase the screen, keep history. The emulator's
	// default copies every non-blank row into scrollback first, which would
	// duplicate history on every clear-and-repaint. Erasing below-and-at the
	// cursor (ED 0) and above it (ED 1) covers the same cells with the same
	// pen background and never pushes; the parser has finished dispatching
	// this sequence's bytes, so feeding two complete sequences back through
	// it is safe. Other ED variants fall through to the default.
	t.emu.RegisterCsiHandler('J', func(params ansi.Params) bool {
		if n, _, _ := params.Param(0, 0); n != 2 {
			return false
		}
		_, _ = t.emu.Write([]byte("\x1b[0J\x1b[1J"))
		return true
	})
	// RIS: the full reset the daemon sends before a resync replay (Reset).
	// The emulator resets both screens but keeps scrollback; the replay is a
	// tail of output already captured, so history must go too or every row the
	// replay scrolls would appear twice. Falls through to the default reset.
	t.emu.RegisterEscHandler('c', func() bool {
		t.emu.ClearScrollback()
		t.offset = 0
		return false
	})
}

// afterWrite runs under t.mu after each chunk reaches the emulator and applies
// the return-to-live rules: entering the alternate screen, or rows scrolling
// into history, both mean the viewport's old content is no longer what the
// program is showing. A chunk that only repainted cells leaves the viewport
// where it is.
func (t *Terminal) afterWrite() {
	t.rev++
	if alt := t.emu.IsAltScreen(); alt != t.wasAlt {
		t.wasAlt = alt
		if alt {
			t.offset = 0
		}
	}
	if m := t.markHistory(); !m.equal(t.hist) {
		t.hist = m
		t.offset = 0
	}
	t.clampOffset()
}

// clampOffset keeps the offset inside the history that exists; caller holds
// t.mu.
func (t *Terminal) clampOffset() {
	if h := t.emu.ScrollbackLen(); t.offset > h {
		t.offset = h
	}
	if t.offset < 0 {
		t.offset = 0
	}
}

// scrollTo moves the viewport to off history rows back, clamped to [0, history
// length], and reports whether it moved. On the alternate screen the only
// position is live. Caller holds t.mu.
func (t *Terminal) scrollTo(off int) bool {
	if t.emu.IsAltScreen() {
		off = 0
	}
	if h := t.emu.ScrollbackLen(); off > h {
		off = h
	}
	if off < 0 {
		off = 0
	}
	if off == t.offset {
		return false
	}
	t.offset = off
	t.rev++
	return true
}

// ScrollLines scrolls the viewport delta rows: positive into history (older),
// negative toward live. It clamps at both ends and reports whether the viewport
// moved — false at the limits and on the alternate screen, where the embedder
// should hand the event to the program instead (MouseEvent).
func (t *Terminal) ScrollLines(delta int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.scrollTo(t.offset + delta)
}

// ScrollPages is ScrollLines by whole pages of rows-1 rows (one row of overlap
// keeps the reader oriented).
func (t *Terminal) ScrollPages(delta int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.scrollTo(t.offset + delta*max(1, t.rows-1))
}

// ScrollToTop shows the oldest history.
func (t *Terminal) ScrollToTop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.scrollTo(t.emu.ScrollbackLen())
}

// ReturnLive returns the viewport to the live screen, reporting whether it was
// scrolled back. Write calls it; an embedder calls it for any other input it
// considers "the user is back".
func (t *Terminal) ReturnLive() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.scrollTo(0)
}

// Viewport reports the viewport state.
func (t *Terminal) Viewport() ViewportState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.viewport()
}

func (t *Terminal) viewport() ViewportState {
	return ViewportState{
		Offset:     t.offset,
		HistoryLen: t.emu.ScrollbackLen(),
		Cols:       t.cols,
		Rows:       t.rows,
		AltScreen:  t.emu.IsAltScreen(),
		Revision:   t.rev,
	}
}

// SetHistoryLimit bounds history to lines rows (oldest rows are dropped first);
// lines < 1 restores DefaultHistoryLines. Shrinking below the current length
// drops the oldest rows immediately.
func (t *Terminal) SetHistoryLimit(lines int) {
	if lines < 1 {
		lines = DefaultHistoryLines
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emu.SetScrollbackSize(lines)
	t.hist = t.markHistory()
	t.clampOffset()
	t.rev++
}

// Indicator is the scroll badge Render overlays on the top-right of the pane
// while scrolled back — " ↑ offset/history " — or "" when live, for an embedder
// that wants to draw it elsewhere.
func (t *Terminal) Indicator() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.indicator()
}

func (t *Terminal) indicator() string {
	if t.offset == 0 {
		return ""
	}
	return fmt.Sprintf(" ↑ %d/%d ", t.offset, t.emu.ScrollbackLen())
}

// Snapshot copies the cells the viewport currently shows; see the type.
func (t *Terminal) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := t.viewport()
	return Snapshot{
		Cols:       v.Cols,
		Rows:       v.Rows,
		Offset:     v.Offset,
		HistoryLen: v.HistoryLen,
		AltScreen:  v.AltScreen,
		Revision:   v.Revision,
		Cells:      t.visibleCells(),
	}
}

// visibleCells builds the rows×cols grid the viewport shows: the offset newest
// history rows, then the live screen from its top row, each row padded with
// blank cells (history rows were stored trimmed) or cut at the pane's width.
// Caller holds t.mu.
func (t *Terminal) visibleCells() [][]uv.Cell {
	cols, rows := t.cols, t.rows
	sb := t.emu.Scrollback()
	h := sb.Len()
	off := min(t.offset, h)
	grid := make([][]uv.Cell, rows)
	for y := range grid {
		row := make([]uv.Cell, cols)
		for x := range row {
			row[x] = uv.EmptyCell
		}
		if y < off {
			copyRow(row, sb.Line(h-off+y))
		} else {
			for x := range row {
				if c := t.emu.CellAt(x, y-off); c != nil {
					row[x] = *c
				}
			}
		}
		grid[y] = row
	}
	return grid
}

// copyRow copies src into dst, cut at dst's width. A wide character the cut
// splits is blanked: its placeholder cells fell past the edge, so it could not
// render correctly.
func copyRow(dst []uv.Cell, src uv.Line) {
	n := min(len(dst), len(src))
	copy(dst, src[:n])
	for x := 0; x < n; x++ {
		if w := dst[x].Width; w > 1 && x+w > len(dst) {
			for i := x; i < len(dst); i++ {
				dst[i] = uv.EmptyCell
			}
			return
		}
	}
}

// renderScrolled renders the scrolled-back viewport with the indicator
// overlaid, in the same cell renderer the emulator's Render uses so styles and
// wide characters come out identically. Caller holds t.mu.
func (t *Terminal) renderScrolled() string {
	grid := t.visibleCells()
	t.overlayIndicator(grid)
	lines := make(uv.Lines, len(grid))
	for y, row := range grid {
		lines[y] = uv.Line(row)
	}
	return lines.Render()
}

// overlayIndicator writes the scroll badge into the top-right cells of row 0 in
// reverse video, if the pane is wide enough to hold it beside at least a few
// cells of content. Snapshot never includes it.
func (t *Terminal) overlayIndicator(grid [][]uv.Cell) {
	text := t.indicator()
	if text == "" || len(grid) == 0 {
		return
	}
	runes := []rune(text)
	row := grid[0]
	x0 := len(row) - len(runes)
	if x0 < 4 {
		return
	}
	if x0 > 0 && row[x0-1].Width > 1 {
		row[x0-1] = uv.EmptyCell // a wide char cut by the badge can't be drawn
	}
	for i, r := range runes {
		row[x0+i] = uv.Cell{Content: string(r), Width: 1, Style: uv.Style{Attrs: uv.AttrReverse}}
	}
}
