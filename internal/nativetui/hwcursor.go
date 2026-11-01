package nativetui

import (
	"bytes"
	"fmt"
	"io"
	"sync"
)

// Hardware cursor for the focused pane.
//
// The child's text cursor is shown with the outer terminal's real cursor, not a
// painted cell: only the real cursor can be a thin bar between characters
// (DECSCUSR 5/6, which Claude Code's vim INSERT mode and Codex's composer
// request), and it keeps the user's own cursor colour and blink.
//
// Bubble Tea v1 has no cursor API: it hides the cursor at startup and owns
// every byte written to the terminal. So cursorOut sits between Bubble Tea
// and the terminal. After each write it parks the cursor at the cell the model
// asked for, sets its shape and shows it; before each write it hides it, so
// the renderer never paints with a visible cursor in the wrong place. The
// pair is wrapped in a synchronized-output frame (mode 2026), so terminals
// that support it show the frame and cursor change atomically; others ignore
// the mode and still never see a visible cursor wander during the paint.
//
// Moving the cursor between frames is safe because in the alternate screen,
// which amux always uses, the renderer begins every frame with an absolute
// CSI H. The cursor is only touched while Bubble Tea's alternate screen is
// active (tracked from the 1049 switches Bubble Tea writes), so writes on the
// way out on quit or suspend pass through untouched and the user's shell gets
// its cursor where Bubble Tea leaves it, with the default shape restored.
//
// Bubble Tea v2 has native cursor support (tea.View.Cursor), but upgrading
// means migrating the whole TUI's key, mouse and renderer APIs; this wrapper
// gets the same result on v1.

const (
	altScreenOn   = "\x1b[?1049h"
	altScreenOff  = "\x1b[?1049l"
	showCursorSeq = "\x1b[?25h"
	hideCursorSeq = "\x1b[?25l"
	syncBegin     = "\x1b[?2026h"
	syncEnd       = "\x1b[?2026l"
)

// hwCursor is a requested outer-terminal cursor: a 0-based screen cell, and
// a DECSCUSR shape (0 is the terminal's default).
type hwCursor struct {
	visible bool
	x, y    int
	style   int
}

// ttyFile is what Bubble Tea needs from its output to treat it as a terminal
// (charmbracelet/x/term.File).
type ttyFile interface {
	io.ReadWriteCloser
	Fd() uintptr
}

type cursorOut struct {
	out ttyFile

	mu    sync.Mutex
	want  hwCursor // what the latest View asked for
	shown hwCursor // what the terminal currently shows
	style int      // DECSCUSR last sent; -1 when the terminal has its own default
	alt   bool     // Bubble Tea's alternate screen is active
}

func newCursorOut(out ttyFile) *cursorOut { return &cursorOut{out: out, style: -1} }

func (c *cursorOut) Fd() uintptr                { return c.out.Fd() }
func (c *cursorOut) Read(p []byte) (int, error) { return c.out.Read(p) }
func (c *cursorOut) Close() error               { return c.out.Close() }

// Write forwards Bubble Tea's output, re-placing the cursor around it.
func (c *cursorOut) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.writeLocked(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// set records the cursor the model wants for the frame View just built. The
// renderer skips a frame identical to the last one, so when only the cursor
// moved (an arrow key in a prompt) set writes the cursor update itself;
// otherwise the renderer's next write carries it, in step with the frame.
func (c *cursorOut) set(want hwCursor, frameUnchanged bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.want = want
	if frameUnchanged && c.alt && want != c.shown {
		_ = c.writeLocked(nil)
	}
}

func (c *cursorOut) writeLocked(p []byte) error {
	wasAlt := c.alt
	if i, j := bytes.LastIndex(p, []byte(altScreenOn)), bytes.LastIndex(p, []byte(altScreenOff)); i > j {
		c.alt = true
	} else if j > i {
		c.alt = false
	}
	var buf bytes.Buffer
	switch {
	case c.alt:
		involved := c.want.visible || c.shown.visible
		if involved {
			buf.WriteString(syncBegin)
			buf.WriteString(hideCursorSeq)
		}
		buf.Write(p)
		if c.want.visible {
			fmt.Fprintf(&buf, "\x1b[%d;%dH", c.want.y+1, c.want.x+1)
			if c.want.style != c.style {
				fmt.Fprintf(&buf, "\x1b[%d q", c.want.style)
				c.style = c.want.style
			}
			buf.WriteString(showCursorSeq)
		}
		if involved {
			buf.WriteString(syncEnd)
		}
		c.shown = c.want
	case wasAlt:
		// Leaving the alternate screen (quit, suspend): hand the terminal back
		// with its default cursor shape. Bubble Tea restores visibility.
		buf.Write(p)
		if c.style > 0 {
			buf.WriteString("\x1b[0 q")
		}
		c.style, c.shown = -1, hwCursor{}
	default:
		buf.Write(p)
	}
	if buf.Len() == 0 {
		return nil
	}
	n, err := c.out.Write(buf.Bytes())
	if err == nil && n < buf.Len() {
		err = io.ErrShortWrite
	}
	return err
}

// hardwareCursor is where the outer terminal's cursor belongs for the current
// frame: on the child's cursor while its pane has keyboard focus (the way a
// multiplexer shows the cursor only in its active pane) and nothing covers it.
func (m *model) hardwareCursor() hwCursor {
	if m.focus != focusAgent || m.accounts != nil || m.archiveSearch != nil ||
		m.picker != nil || m.form != nil || m.confirm != nil {
		return hwCursor{}
	}
	t := m.cur()
	if t == nil {
		return hwCursor{}
	}
	c := t.Cursor()
	// A pane not yet resized to the layout (mid-resize) must not put the
	// cursor in a border or the rail.
	if !c.Visible || c.X >= m.mainWidth() || c.Y >= m.paneRows() {
		return hwCursor{}
	}
	// The pane sits right of the rail and its divider, below the top border.
	return hwCursor{visible: true, x: sidebarWidth + 1 + c.X, y: 1 + c.Y, style: c.Style}
}
