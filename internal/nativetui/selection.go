package nativetui

import (
	"strings"

	"amux/internal/termselect"
	"amux/internal/vterm"
	tea "github.com/charmbracelet/bubbletea"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

type paneSelection struct {
	term         *vterm.Terminal
	key          paneKey
	snapshot     vterm.Snapshot
	anchor, head termselect.Point
	moved        bool
}

type paneGesture struct {
	term       *vterm.Terminal
	local      bool
	cancelled  bool
	button     tea.MouseButton
	buttons    map[tea.MouseButton]bool // app gestures may hold multiple buttons
	cols, rows int                      // press-time bounds; growing a window does not expand capture
}

type clipboardQueuedMsg struct{}

func (m *model) mouseModal() bool {
	return m.accounts != nil || m.archiveSearch != nil || m.form != nil || m.confirm != nil || m.picker != nil
}

// clearSelection retains a cancelled local gesture until release. Dropping the
// capture would let its remaining events activate the rail or reach the child.
func (m *model) clearSelection() {
	m.selection = nil
	if g := m.gesture; g != nil && g.local {
		g.cancelled = true
	}
}

func (m *model) validateSelection() {
	s := m.selection
	if s == nil {
		return
	}
	if m.mouseModal() || s.key != (paneKey{m.attached, m.tab}) || m.cur() != s.term || s.term.Closed() ||
		s.snapshot.Revision != s.term.Viewport().Revision {
		m.clearSelection()
	}
}

func panePoint(ev tea.MouseMsg, cols, rows int) termselect.Point {
	return termselect.Point{
		X: min(max(ev.X-sidebarWidth-1, 0), cols-1),
		Y: min(max(ev.Y-1, 0), rows-1),
	}
}

// selectionMouse runs before modal/rail hit testing. A gesture keeps the owner
// chosen at press regardless of changed modifiers or an outside release.
func (m *model) selectionMouse(ev tea.MouseMsg) (bool, tea.Cmd) {
	m.validateSelection()
	if g := m.gesture; g != nil {
		v := g.term.Viewport()
		p := panePoint(ev, min(g.cols, v.Cols), min(g.rows, v.Rows))
		if tea.MouseEvent(ev).IsWheel() {
			// Wheel notches are independent scroll actions, not button
			// releases. Shift still forces local scrolling during an app
			// drag. A local drag keeps the wheel local and cancels selection.
			if g.local {
				m.clearSelection()
				ev.Shift = true
			}
			g.term.MouseEvent(mouseToVT(ev, p.X, p.Y))
			return true, nil
		}
		if !g.local {
			// A pane switch cannot redirect the release to another terminal.
			// Keep the original app's capture until it gets its release, even
			// when a modal, switch, or resize occurred during the gesture.
			if ev.Button == tea.MouseButtonNone {
				ev.Button = g.button // legacy releases omit the button
			}
			switch ev.Action {
			case tea.MouseActionPress:
				g.buttons[ev.Button] = true
				g.button = ev.Button
			case tea.MouseActionRelease:
				delete(g.buttons, ev.Button)
				if len(g.buttons) == 0 {
					m.gesture = nil
				} else if !g.buttons[g.button] {
					for button := range g.buttons {
						g.button = button
						break
					}
				}
			}
			g.term.ForwardMouse(mouseToVT(ev, p.X, p.Y))
			return true, nil
		}
		if ev.Action != tea.MouseActionMotion && ev.Action != tea.MouseActionRelease {
			return true, nil
		}
		if ev.Action == tea.MouseActionRelease {
			if ev.Button != g.button && ev.Button != tea.MouseButtonNone {
				return true, nil
			}
			m.gesture = nil
		}
		if g.cancelled || m.selection == nil {
			return true, nil
		}
		s := m.selection
		s.head = p
		s.moved = s.moved || p != s.anchor
		if ev.Action != tea.MouseActionRelease || !s.moved {
			if ev.Action == tea.MouseActionRelease {
				m.selection = nil
			}
			return true, nil
		}
		// Snapshot checks the revision and cells together under the terminal
		// lock. Copy from the captured cells, never re-extract at stale coords.
		if s.term.Snapshot().Revision != s.snapshot.Revision {
			m.clearSelection()
			return true, nil
		}
		text := selectedCells(s).Text
		if text == "" || m.clip == nil {
			return true, nil
		}
		clip := m.clip
		return true, func() tea.Msg {
			clip([]byte(text))
			return clipboardQueuedMsg{}
		}
	}
	if m.mouseModal() || tea.MouseEvent(ev).IsWheel() || ev.Action != tea.MouseActionPress {
		if ev.Action == tea.MouseActionRelease {
			return true, nil // no pane-owned press; never send an orphan release
		}
		return false, nil
	}
	t := m.cur()
	if t == nil {
		return false, nil
	}
	// Clip against the actual outer window as well as the layout's fallback
	// dimensions; a very small window has no selectable pane content.
	cols := min(m.mainWidth(), max(0, m.w-sidebarWidth-1))
	rows := min(m.paneRows(), max(0, m.h-3))
	x, y := ev.X-sidebarWidth-1, ev.Y-1
	if x < 0 || y < 0 || x >= cols || y >= rows {
		return false, nil
	}
	m.clearSelection()
	m.focus = focusAgent
	local := ev.Shift || !ev.Alt && !t.MouseTracked()
	m.gesture = &paneGesture{term: t, local: local, button: ev.Button, cols: cols, rows: rows}
	if !local {
		m.gesture.buttons = map[tea.MouseButton]bool{ev.Button: true}
		t.ForwardMouse(mouseToVT(ev, x, y))
		return true, nil
	}
	if ev.Button != tea.MouseButtonLeft {
		return true, nil
	}
	snap := t.Snapshot()
	p := termselect.Point{X: x, Y: y}
	m.selection = &paneSelection{term: t, key: paneKey{m.attached, m.tab}, snapshot: snap, anchor: p, head: p}
	return true, nil
}

func selectedCells(s *paneSelection) termselect.Selection {
	rows := make([][]termselect.Cell, len(s.snapshot.Cells))
	for y, row := range s.snapshot.Cells {
		rows[y] = make([]termselect.Cell, len(row))
		for x, c := range row {
			rows[y][x] = termselect.Cell{Text: c.Content, Width: c.Width}
		}
	}
	return termselect.Select(rows, s.snapshot.Cols, s.anchor, s.head)
}

func (m *model) renderSelectedPane(t *vterm.Terminal) string {
	// Output can be parsed between Update and View. Clear stale coordinates
	// here as well so a frame never highlights newly arrived text.
	m.validateSelection()
	s := m.selection
	if s == nil || !s.moved {
		return t.Render()
	}
	snap := t.Snapshot()
	if snap.Revision != s.snapshot.Revision {
		m.clearSelection()
		return t.Render()
	}
	for _, span := range selectedCells(s).Spans {
		for x := span.Start; x < span.End; x++ {
			c := &snap.Cells[span.Y][x]
			if c.IsZero() {
				continue // wide-cell continuation; styled by its leading cell
			}
			c.Style.Fg, c.Style.Bg = ansi.IndexedColor(231), ansi.IndexedColor(24)
			c.Style.Attrs &^= uv.AttrReverse | uv.AttrConceal | uv.AttrFaint | uv.AttrBlink | uv.AttrRapidBlink
		}
	}
	lines := make([]string, snap.Rows)
	for y, row := range snap.Cells {
		line := uv.Line(row).Render()
		lines[y] = line + strings.Repeat(" ", max(0, snap.Cols-ansi.StringWidth(line)))
	}
	return strings.Join(lines, "\n")
}
