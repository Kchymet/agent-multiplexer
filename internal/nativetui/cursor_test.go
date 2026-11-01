package nativetui

import (
	"bytes"
	"strings"
	"testing"
)

// recordTTY records each write separately, to assert what one renderer flush
// puts on the wire.
type recordTTY struct{ writes []string }

func (r *recordTTY) Write(p []byte) (int, error) {
	r.writes = append(r.writes, string(p))
	return len(p), nil
}
func (r *recordTTY) Read([]byte) (int, error) { return 0, nil }
func (r *recordTTY) Close() error             { return nil }
func (r *recordTTY) Fd() uintptr              { return ^uintptr(0) }
func (r *recordTTY) last() string             { return r.writes[len(r.writes)-1] }

// The focused pane's cursor is the outer terminal's hardware cursor, so a bar
// is a real thin bar. The pane sits right of the rail and divider, below the
// top border; nothing covers or hides it. Unfocused, scrolled back or covered
// by a modal, there is no cursor.
func TestHardwareCursorPlacement(t *testing.T) {
	m, term, _ := scrollModel(t, 12, 4)
	feedPane(t, term, "\x1b[6 q> hi")
	want := hwCursor{visible: true, x: sidebarWidth + 1 + 4, y: 1, style: 6}
	if got := m.hardwareCursor(); got != want {
		t.Fatalf("hardwareCursor() = %+v, want %+v", got, want)
	}
	// The painted-cell fallback is gone: the frame itself carries no cursor.
	if v := m.View(); strings.Contains(v, "\x1b[7m") {
		t.Fatalf("frame paints a cursor cell: %q", v)
	}
	m.focus = focusSidebar
	if m.hardwareCursor().visible {
		t.Fatal("unfocused pane shows a cursor")
	}
	m.focus = focusAgent
	m.confirm = &confirmState{message: "sure?"}
	if m.hardwareCursor().visible {
		t.Fatal("cursor shows through a modal dialog")
	}
	m.confirm = nil
	feedPane(t, term, "\x1b[?25l")
	if m.hardwareCursor().visible {
		t.Fatal("cursor the child hid is shown")
	}
	feedPane(t, term, "\x1b[?25h"+historyRows(8))
	if !term.ScrollLines(1) || m.hardwareCursor().visible {
		t.Fatal("scrolled-back pane shows the live cursor")
	}
}

// One renderer flush is one write: hidden while painting, then the cursor is
// parked, shaped and shown, all inside a synchronized-output frame so it
// never flickers or wanders across the repaint.
func TestCursorOutWrapsFrames(t *testing.T) {
	rec := &recordTTY{}
	c := newCursorOut(rec)
	if _, err := c.Write([]byte(altScreenOn)); err != nil {
		t.Fatal(err)
	}
	if rec.last() != altScreenOn {
		t.Fatalf("hidden cursor changed the passthrough: %q", rec.last())
	}

	bar := hwCursor{visible: true, x: 31, y: 1, style: 6}
	c.set(bar, false) // a new frame is coming; the renderer's write carries the cursor
	if len(rec.writes) != 1 {
		t.Fatalf("set wrote ahead of its frame: %q", rec.writes[1:])
	}
	frame := "\x1b[Hframe\x1b[10;1H"
	_, _ = c.Write([]byte(frame))
	if got, want := rec.last(), syncBegin+hideCursorSeq+frame+"\x1b[2;32H\x1b[6 q"+showCursorSeq+syncEnd; got != want {
		t.Fatalf("frame write = %q, want %q", got, want)
	}

	// Same shape again: no redundant DECSCUSR.
	_, _ = c.Write([]byte(frame))
	if got, want := rec.last(), syncBegin+hideCursorSeq+frame+"\x1b[2;32H"+showCursorSeq+syncEnd; got != want {
		t.Fatalf("second frame = %q, want %q", got, want)
	}

	// Cursor-only change (arrow key in a prompt): the renderer skips the
	// identical frame, so set writes the move itself.
	n := len(rec.writes)
	c.set(hwCursor{visible: true, x: 30, y: 1, style: 2}, true)
	if len(rec.writes) != n+1 {
		t.Fatal("cursor-only change was not written")
	}
	if got, want := rec.last(), syncBegin+hideCursorSeq+"\x1b[2;31H\x1b[2 q"+showCursorSeq+syncEnd; got != want {
		t.Fatalf("cursor-only write = %q, want %q", got, want)
	}
	c.set(hwCursor{visible: true, x: 30, y: 1, style: 2}, true)
	if len(rec.writes) != n+1 {
		t.Fatal("unchanged cursor was rewritten")
	}

	// Hidden (rail focus, modal, child DECTCEM off): frames leave it hidden.
	c.set(hwCursor{}, true)
	if got, want := rec.last(), syncBegin+hideCursorSeq+syncEnd; got != want {
		t.Fatalf("hide write = %q, want %q", got, want)
	}
	_, _ = c.Write([]byte(frame))
	if got := rec.last(); got != frame {
		t.Fatalf("frame with hidden cursor = %q, want passthrough", got)
	}

	// Leaving the alternate screen (quit, suspend) restores the default shape
	// and never moves the cursor on the user's main screen.
	c.set(bar, false)
	_, _ = c.Write([]byte(frame))
	exit := altScreenOff + showCursorSeq
	_, _ = c.Write([]byte(exit))
	if got, want := rec.last(), exit+"\x1b[0 q"; got != want {
		t.Fatalf("exit write = %q, want %q", got, want)
	}
	_, _ = c.Write([]byte("\r\n"))
	if got := rec.last(); got != "\r\n" {
		t.Fatalf("write after exit touched the cursor: %q", got)
	}
	// Resume: back in the alternate screen, the shape is sent again.
	_, _ = c.Write([]byte(altScreenOn))
	if got := rec.last(); !strings.HasSuffix(got, "\x1b[2;32H\x1b[6 q"+showCursorSeq+syncEnd) {
		t.Fatalf("resume did not restore the cursor: %q", got)
	}
}

// No write may show the cursor before the frame's paint is finished.
func TestCursorOutNeverShowsMidFrame(t *testing.T) {
	rec := &recordTTY{}
	c := newCursorOut(rec)
	_, _ = c.Write([]byte(altScreenOn))
	c.set(hwCursor{visible: true, x: 3, y: 2, style: 5}, false)
	_, _ = c.Write([]byte("\x1b[Hline one\r\nline two" + showCursorSeq + "tail"))
	w := []byte(rec.last())
	hide := bytes.Index(w, []byte(hideCursorSeq))
	paint := bytes.Index(w, []byte("line one"))
	park := bytes.LastIndex(w, []byte("\x1b[3;4H"))
	show := bytes.LastIndex(w, []byte(showCursorSeq))
	if hide < 0 || hide > paint || park < bytes.Index(w, []byte("tail")) || show < park {
		t.Fatalf("cursor not hidden during paint then shown after parking: %q", w)
	}
}
