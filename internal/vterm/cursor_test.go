package vterm

import (
	"strings"
	"testing"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// cellOf re-parses a rendered pane block in a fresh emulator and returns the
// cell at (x, y), so tests assert on what the outer terminal would display.
func cellOf(t *testing.T, block string, cols, rows, x, y int) uv.Cell {
	t.Helper()
	e := vt.NewEmulator(cols, rows)
	// The block's rows are joined by bare "\n"; a terminal needs CR as well.
	_, _ = e.WriteString(strings.ReplaceAll(block, "\n", "\r\n"))
	c := e.CellAt(x, y)
	if c == nil {
		t.Fatalf("no cell at %d,%d", x, y)
	}
	return *c
}

func isReverse(c uv.Cell) bool   { return c.Style.Attrs&uv.AttrReverse != 0 }
func isUnderline(c uv.Cell) bool { return c.Style.Underline != uv.UnderlineStyleNone }

// The pane's cursor is the child's hardware cursor: amux runs under Bubble Tea,
// which keeps the real terminal cursor hidden, so the pane must paint it. These
// are the sequences an agent's vim mode emits: INSERT switches to a bar
// (DECSCUSR 5/6), NORMAL back to a block (2), replace mode to underline (4).
func TestRenderCursorShapes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seq       string
		reverse   bool
		underline bool
	}{
		{"default block", "", true, false},
		{"insert bar blinking", "\x1b[5 q", true, false},
		{"insert bar steady", "\x1b[6 q", true, false},
		{"normal block", "\x1b[6 q\x1b[2 q", true, false},
		{"underline", "\x1b[4 q", false, true},
		{"bar then reset to default", "\x1b[6 q\x1b[0 q", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term := New(20, 4)
			defer term.Close()
			// Text, then the cursor parked on the "l" of "hello" and one past the end.
			term.ingest([]byte("> hello" + tc.seq + "\x1b[1;5H"))
			out := term.RenderCursor()
			c := cellOf(t, out, 20, 4, 4, 0)
			if c.Content != "l" {
				t.Fatalf("cursor cell content = %q, want %q (render %q)", c.Content, "l", out)
			}
			if isReverse(c) != tc.reverse || isUnderline(c) != tc.underline {
				t.Fatalf("cursor cell reverse=%v underline=%v, want %v/%v (render %q)",
					isReverse(c), isUnderline(c), tc.reverse, tc.underline, out)
			}
			// Neighbors are untouched.
			if n := cellOf(t, out, 20, 4, 3, 0); isReverse(n) || isUnderline(n) {
				t.Fatalf("neighbor cell styled: %+v", n.Style)
			}
			// Render (unfocused pane) never draws the cursor.
			if p := cellOf(t, term.Render(), 20, 4, 4, 0); isReverse(p) || isUnderline(p) {
				t.Fatalf("Render drew a cursor: %+v", p.Style)
			}
		})
	}
}

// Codex and Claude Code's INSERT mode put the cursor in the blank cell after
// the typed text; that cell must still be drawn (not trimmed away as blank).
func TestRenderCursorOnBlankCellAfterText(t *testing.T) {
	term := New(20, 4)
	defer term.Close()
	term.ingest([]byte("\x1b[6 q> hi"))
	out := term.RenderCursor()
	if c := cellOf(t, out, 20, 4, 4, 0); !isReverse(c) {
		t.Fatalf("cursor after text not drawn: %+v (render %q)", c, out)
	}
	if w := ansiWidth(strings.Split(out, "\n")[0]); w != 20 {
		t.Fatalf("row width = %d, want 20", w)
	}
}

// DECTCEM: a child that hides the cursor (to draw its own, or while it
// repaints) gets no painted cursor; showing it again brings it back. RIS
// (the daemon's resync reset) restores a visible block cursor.
func TestRenderCursorVisibility(t *testing.T) {
	term := New(20, 4)
	defer term.Close()
	term.ingest([]byte("abc\x1b[?25l\x1b[1;2H"))
	if c := cellOf(t, term.RenderCursor(), 20, 4, 1, 0); isReverse(c) {
		t.Fatal("hidden cursor was drawn")
	}
	term.ingest([]byte("\x1b[?25h"))
	if c := cellOf(t, term.RenderCursor(), 20, 4, 1, 0); !isReverse(c) {
		t.Fatal("shown cursor not drawn")
	}
	term.ingest([]byte("\x1b[?25l\x1b[4 q\x1bcxyz"))
	if c := cellOf(t, term.RenderCursor(), 20, 4, 3, 0); !isReverse(c) {
		t.Fatalf("cursor after RIS should be a visible block: %+v", c.Style)
	}
}

// Visibility is per screen: an alternate-screen app hiding the cursor must not
// leave the primary screen's cursor hidden after it exits.
func TestRenderCursorAltScreen(t *testing.T) {
	term := New(20, 4)
	defer term.Close()
	term.ingest([]byte("ab\x1b[?1049h\x1b[?25l"))
	if c := cellOf(t, term.RenderCursor(), 20, 4, 0, 0); isReverse(c) {
		t.Fatal("hidden alt-screen cursor drawn")
	}
	term.ingest([]byte("\x1b[?1049l"))
	if c := cellOf(t, term.RenderCursor(), 20, 4, 2, 0); !isReverse(c) {
		t.Fatalf("primary cursor not restored after leaving alt screen: %+v", c.Style)
	}
}

// Edge cells: the pending-wrap column draws on the last cell, and a cursor on a
// wide character's continuation draws on the character.
func TestRenderCursorEdges(t *testing.T) {
	term := New(4, 2)
	defer term.Close()
	term.ingest([]byte("abcd"))
	if c := cellOf(t, term.RenderCursor(), 4, 2, 3, 0); !isReverse(c) || c.Content != "d" {
		t.Fatalf("pending-wrap cursor not on last cell: %+v", c)
	}
	wide := New(6, 2)
	defer wide.Close()
	wide.ingest([]byte("世界\x1b[1;2H"))
	if c := cellOf(t, wide.RenderCursor(), 6, 2, 0, 0); !isReverse(c) || c.Content != "世" {
		t.Fatalf("cursor on wide continuation not on its character: %+v", c)
	}
}

// Scrolled back into history, the viewport shows no live cursor.
func TestRenderCursorHiddenWhileScrolledBack(t *testing.T) {
	term := New(10, 2)
	defer term.Close()
	term.ingest([]byte("1\r\n2\r\n3\r\n4"))
	if !term.ScrollLines(1) {
		t.Fatal("could not scroll back")
	}
	if term.RenderCursor() != term.Render() {
		t.Fatal("scrolled-back viewport drew a cursor")
	}
}
