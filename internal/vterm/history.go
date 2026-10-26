package vterm

import (
	"fmt"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// Pane-local history and viewport.
//
// A real terminal keeps the lines that scroll off the top of its primary
// screen and lets the user scroll back over them; a full-screen program on the
// alternate screen scrolls its own content and the terminal keeps nothing. amux
// mirrors a pane through an emulator, so the outer terminal's scrollback never
// sees the agent's output — this file gives each pane that history itself, and
// goes one step further than a terminal for the alternate screen, where the
// agents amux runs (Codex in its forced alternate-screen mode) keep their
// transcript. The two histories are separate buffers with separate rules; the
// viewport scrolls whichever screen is active.
//
// Primary screen: scrolled-off rows.
//
//   - History is exactly the rows that physically scroll off the top of the
//     primary screen: a line feed, index or next-line at the bottom margin,
//     scroll-up (SU), or delete-line (DL) inside a full-width scrolling region.
//     That covers a shell, any program printing a transcript, and Codex's
//     inline transcript mode, which inserts each committed line above its
//     viewport with DECSTBM 1..top plus "\r\n" (codex-rs insert_history.rs) —
//     each inserted row scrolls off row 0 and is captured once, losslessly.
//     Rows keep the width they had at the time; they are physical rows with no
//     soft-wrap metadata and are not reflowed on resize (a narrower pane
//     truncates them, a wider one pads).
//   - Repaints never enter history. Programs that redraw with cursor
//     positioning (every ratatui/Ink frame, Codex's spinner and composer) touch
//     cells, not the scroll position, so nothing is captured. Erase-display 2
//     (ESC[2J, ratatui's clear, Codex's viewport change, a shell `clear`) is
//     overridden to erase without saving the screen to history, as xterm does;
//     the emulator's default would copy every non-blank row into scrollback on
//     each clear-and-repaint cycle and duplicate history. ESC[3J still clears
//     history, which is what `clear` means by it.
//
// Alternate screen: completed, distinct rendered frames.
//
//   - A probe of real Codex 0.157.1 in owned mode (tui.alternate_screen =
//     "always", PR #132) shows it redraws its in-app transcript inside
//     synchronized-output frames (DECSET/DECRST 2026) with zero scroll
//     operations — no LF, IND, RI, NEL, SU, SD or DL — so there are no
//     scrolled-off rows to capture. What there is, is a sequence of frames the
//     user saw. The alternate screen's history is that sequence: every
//     completed frame whose content differs from the frame before it, oldest
//     first, each frame being the pane's rows as they were painted.
//   - A frame is complete when the program begins the next one. DECSET 2026
//     commits the cells currently on the alternate screen to history before the
//     new frame paints over them; DECRST 2026 only closes the frame. The live
//     screen is therefore never in history ("previously painted only"), a
//     frame left half-painted by a nested DECSET is not committed, and the
//     capture happens at the sequence, so it does not depend on how the output
//     was chunked.
//   - Duplicate suppression: a frame identical to the last committed frame
//     (cell for cell: text, width, style, link) is dropped, so a cursor-only or
//     no-op repaint adds nothing. An all-blank frame (the screen the emulator
//     clears on entry) is dropped too. Frames that differ only by a spinner
//     glyph are distinct and are kept; the bound below is what limits them.
//   - Programs that do not use synchronized output get no alternate-screen
//     history: the byte stream has no frame boundary that is independent of
//     chunking and timing, so no frame is ever completed. They keep the
//     program's own scrolling (MouseTracked / MouseEvent) exactly as before.
//   - The buffer is tied to one program session: entering or leaving the
//     alternate screen (DECSET/DECRST 1047/1049) clears it, as does RIS.
//     Frames keep the size they were painted at; after a resize they are shown
//     cut or padded like primary rows.
//
// Both buffers are bounded by the same row limit (SetHistoryLimit,
// DefaultHistoryLines): the primary scrollback by the emulator, the frame
// history by dropping whole frames oldest first until the rows fit (the newest
// frame always survives, so the frame history holds at most limit rows plus
// one screen). The emulator's hidden alternate-screen scrollback is never read.
//
// The viewport is an offset into the active screen's history: 0 is live, n
// shows the n newest history rows above the live screen's top rows (Render,
// Snapshot). It returns to live on its own whenever the program or the user
// moves on: any output at all (a repaint that changes a word is new output the
// user asked to see; a program that keeps repainting while idle keeps the pane
// live, which is the trade the user asked for), input from the user (Write),
// a resize, a screen switch, or a reset. RIS (ESC c) also clears both
// histories: the daemon sends it (Reset) before replaying a recent tail of
// output the history has already absorbed, so keeping the old rows would
// duplicate every line the replay scrolls.
//
// Render and Snapshot show the same cells; the scroll indicator is not drawn
// into the pane. An embedder draws Indicator (or its own text from Viewport)
// in its border or status line, so a selection over the pane never copies a
// badge that was covering real text.

// DefaultHistoryLines is the per-pane history bound unless SetHistoryLimit
// changes it, applied to each of the two buffers. tmux's default history-limit;
// a row costs about its visible width in cells (trailing blanks are not
// stored), so a busy 200-column pane tops out around 40 MiB of cells per
// buffer and typical ones far less.
const DefaultHistoryLines = 2000

// ViewportState describes where the viewport is, for a UI that routes wheel and
// key events and draws its own affordances.
type ViewportState struct {
	// Offset is how many history rows the viewport is scrolled back; 0 is live.
	Offset int
	// HistoryLen is how many history rows of the active screen exist to scroll
	// over: scrolled-off rows on the primary screen, completed-frame rows on
	// the alternate screen.
	HistoryLen int
	// Cols and Rows are the pane's current size, the shape of Render/Snapshot.
	Cols, Rows int
	// AltScreen reports that the program is on the alternate screen, so
	// HistoryLen counts rendered frames and the program also scrolls itself.
	AltScreen bool
	// Revision changes whenever what Render/Snapshot would show may have
	// changed: output, resize, scroll, reset.
	Revision uint64
}

// Snapshot is a detached copy of the cells Render draws: Rows rows of Cols
// cells each, cell (x, y) at Cells[y][x]. Rows are physical (no wrap
// metadata); a wide character occupies its first cell with Width 2 followed by
// a zero Cell placeholder. Styles and links are copied by value, so the
// snapshot stays valid after the pane moves on.
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

// frameHistory is the alternate screen's history: completed, distinct frames,
// oldest first, addressed as one stream of rows so the viewport code treats
// it like the primary scrollback. Rows are stored with trailing blank cells
// trimmed. open tracks a synchronized-output frame in progress.
type frameHistory struct {
	frames [][]uv.Line
	rows   int
	limit  int
	open   bool
}

// Len is the total number of rows across frames.
func (f *frameHistory) Len() int { return f.rows }

// Line returns row i of the stream, 0 being the oldest row of the oldest
// frame, or nil out of range.
func (f *frameHistory) Line(i int) uv.Line {
	if i < 0 {
		return nil
	}
	for _, fr := range f.frames {
		if i < len(fr) {
			return fr[i]
		}
		i -= len(fr)
	}
	return nil
}

// push commits frame unless it is blank or identical to the newest frame, then
// trims to the limit. It reports whether history changed.
func (f *frameHistory) push(frame []uv.Line) bool {
	if blankFrame(frame) {
		return false
	}
	if n := len(f.frames); n > 0 && sameFrame(f.frames[n-1], frame) {
		return false
	}
	f.frames = append(f.frames, frame)
	f.rows += len(frame)
	f.trim()
	return true
}

// trim drops whole frames, oldest first, until the rows fit the limit, always
// keeping the newest frame.
func (f *frameHistory) trim() {
	for len(f.frames) > 1 && f.rows > f.limit {
		f.rows -= len(f.frames[0])
		f.frames[0] = nil
		f.frames = f.frames[1:]
	}
}

// clear forgets every frame and any frame in progress.
func (f *frameHistory) clear() {
	f.frames, f.rows, f.open = nil, 0, false
}

func blankFrame(frame []uv.Line) bool {
	for _, row := range frame {
		if len(row) > 0 {
			return false
		}
	}
	return true
}

func sameFrame(a, b []uv.Line) bool {
	if len(a) != len(b) {
		return false
	}
	for y := range a {
		if len(a[y]) != len(b[y]) {
			return false
		}
		for x := range a[y] {
			if !a[y][x].Equal(&b[y][x]) {
				return false
			}
		}
	}
	return true
}

// captureFrame copies the active screen's cells into detached rows with
// trailing blanks trimmed; caller holds t.mu.
func (t *Terminal) captureFrame() []uv.Line {
	frame := make([]uv.Line, t.rows)
	for y := range frame {
		n := t.cols
		for n > 0 {
			c := t.emu.CellAt(n-1, y)
			if c != nil && !c.Equal(&uv.EmptyCell) {
				break
			}
			n--
		}
		row := make(uv.Line, n)
		for x := range row {
			if c := t.emu.CellAt(x, y); c != nil {
				row[x] = *c
			} else {
				row[x] = uv.EmptyCell
			}
		}
		frame[y] = row
	}
	return frame
}

// beginFrame handles DECSET 2026: on the alternate screen it commits the frame
// on display before the program paints the next one. Caller holds t.mu.
func (t *Terminal) beginFrame() {
	if t.alt.open {
		return // nested begin: the screen is mid-paint, not a frame
	}
	t.alt.open = true
	if t.emu.IsAltScreen() {
		t.alt.push(t.captureFrame())
	}
}

// installHistoryHooks bounds both histories and layers the sequence handlers
// the policy above needs over the emulator's defaults. All run synchronously
// inside emu.Write, under t.mu, like noteMode; those returning false fall
// through to the emulator's own handling of the same sequence.
func (t *Terminal) installHistoryHooks() {
	t.emu.SetScrollbackSize(DefaultHistoryLines)
	t.alt.limit = DefaultHistoryLines
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
	// DEC private modes: synchronized-output frame boundaries (2026) drive the
	// alternate-screen frame history; an alternate-screen switch (1047/1049)
	// starts it over. The emulator applies the mode change after this returns.
	decMode := func(params ansi.Params, set bool) bool {
		params.ForEach(-1, func(_, p int, _ bool) {
			switch p {
			case 2026:
				if set {
					t.beginFrame()
				} else {
					t.alt.open = false
				}
			case 1047, 1049:
				t.alt.clear()
			}
		})
		return false
	}
	t.emu.RegisterCsiHandler(ansi.Command('?', 0, 'h'), func(p ansi.Params) bool { return decMode(p, true) })
	t.emu.RegisterCsiHandler(ansi.Command('?', 0, 'l'), func(p ansi.Params) bool { return decMode(p, false) })
	// RIS: the full reset the daemon sends before a resync replay (Reset).
	// The emulator resets both screens but keeps scrollback; the replay is a
	// tail of output already captured, so history must go too or every row the
	// replay scrolls would appear twice. Falls through to the default reset.
	t.emu.RegisterEscHandler('c', func() bool {
		t.emu.ClearScrollback()
		t.alt.clear()
		t.offset = 0
		return false
	})
}

// afterWrite runs under t.mu after a chunk of n bytes reaches the emulator:
// any output returns the viewport to live, and the offset is kept inside the
// history that exists.
func (t *Terminal) afterWrite(n int) {
	t.rev++
	if n > 0 {
		t.offset = 0
	}
	t.clampOffset()
}

// historyLen is the active screen's history length; caller holds t.mu.
func (t *Terminal) historyLen() int {
	if t.emu.IsAltScreen() {
		return t.alt.Len()
	}
	return t.emu.ScrollbackLen()
}

// historyLine returns row i (0 = oldest) of the active screen's history;
// caller holds t.mu.
func (t *Terminal) historyLine(i int) uv.Line {
	if t.emu.IsAltScreen() {
		return t.alt.Line(i)
	}
	return t.emu.Scrollback().Line(i)
}

// clampOffset keeps the offset inside the history that exists; caller holds
// t.mu.
func (t *Terminal) clampOffset() {
	if h := t.historyLen(); t.offset > h {
		t.offset = h
	}
	if t.offset < 0 {
		t.offset = 0
	}
}

// scrollTo moves the viewport to off history rows back, clamped to [0, history
// length], and reports whether it moved. Caller holds t.mu.
func (t *Terminal) scrollTo(off int) bool {
	if h := t.historyLen(); off > h {
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
// moved — false at the limits and when the active screen has no history (an
// alternate-screen program that never completed a frame), where the embedder
// may hand the event to the program instead (MouseEvent).
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

// ScrollHalfPages moves by half the current viewport height, rounded down,
// with a minimum of one row. Bounds and live/history semantics match ScrollLines.
func (t *Terminal) ScrollHalfPages(delta int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.scrollTo(t.offset + delta*max(1, t.rows/2))
}

// ScrollToTop shows the oldest history.
func (t *Terminal) ScrollToTop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.scrollTo(t.historyLen())
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
		HistoryLen: t.historyLen(),
		Cols:       t.cols,
		Rows:       t.rows,
		AltScreen:  t.emu.IsAltScreen(),
		Revision:   t.rev,
	}
}

// SetHistoryLimit bounds each history to lines rows (oldest rows, or oldest
// frames, are dropped first); lines < 1 restores DefaultHistoryLines.
// Shrinking below the current length drops immediately.
func (t *Terminal) SetHistoryLimit(lines int) {
	if lines < 1 {
		lines = DefaultHistoryLines
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.emu.SetScrollbackSize(lines)
	t.alt.limit = lines
	t.alt.trim()
	t.clampOffset()
	t.rev++
}

// Indicator is the scroll badge text for an embedder to draw in its border or
// status line while the pane is scrolled back — " ↑ offset/history " — or ""
// when live. Render never draws it into the pane's cells.
func (t *Terminal) Indicator() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.offset == 0 {
		return ""
	}
	return fmt.Sprintf(" ↑ %d/%d ", t.offset, t.historyLen())
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
// blank cells (history rows are stored trimmed) or cut at the pane's width.
// Caller holds t.mu.
func (t *Terminal) visibleCells() [][]uv.Cell {
	cols, rows := t.cols, t.rows
	h := t.historyLen()
	off := min(t.offset, h)
	grid := make([][]uv.Cell, rows)
	for y := range grid {
		row := make([]uv.Cell, cols)
		for x := range row {
			row[x] = uv.EmptyCell
		}
		if y < off {
			copyRow(row, t.historyLine(h-off+y))
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

// renderScrolled renders the scrolled-back viewport in the same cell renderer
// the emulator's Render uses, so styles and wide characters come out
// identically and Snapshot matches cell for cell. Caller holds t.mu.
func (t *Terminal) renderScrolled() string {
	grid := t.visibleCells()
	lines := make(uv.Lines, len(grid))
	for y, row := range grid {
		lines[y] = uv.Line(row)
	}
	return lines.Render()
}
