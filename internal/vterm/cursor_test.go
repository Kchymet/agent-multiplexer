package vterm

import "testing"

// Cursor reports the child's DECSCUSR parameter verbatim so the embedder can
// forward it to the outer terminal's hardware cursor. These are the sequences
// an agent's vim mode emits: INSERT switches to a bar (5/6), NORMAL back to a
// block (2), replace mode to underline (4); 0 restores the terminal default.
func TestCursorStyle(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  string
		want int
	}{
		{"never set is terminal default", "", 0},
		{"insert bar blinking", "\x1b[5 q", 5},
		{"insert bar steady", "\x1b[6 q", 6},
		{"normal block", "\x1b[6 q\x1b[2 q", 2},
		{"blinking block", "\x1b[1 q", 1},
		{"underline blinking", "\x1b[3 q", 3},
		{"underline steady", "\x1b[4 q", 4},
		{"bar then terminal default", "\x1b[6 q\x1b[0 q", 0},
		{"bar then bare reset", "\x1b[6 q\x1b[ q", 0},
		{"out of range is default", "\x1b[6 q\x1b[9 q", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term := New(20, 4)
			defer term.Close()
			term.ingest([]byte("> hello" + tc.seq + "\x1b[1;5H"))
			want := Cursor{X: 4, Y: 0, Visible: true, Style: tc.want}
			if got := term.Cursor(); got != want {
				t.Fatalf("Cursor() = %+v, want %+v", got, want)
			}
		})
	}
}

// Codex and Claude Code's INSERT mode put the cursor in the blank cell after
// the typed text.
func TestCursorAfterText(t *testing.T) {
	term := New(20, 4)
	defer term.Close()
	term.ingest([]byte("\x1b[6 q> hi\r\n  x\x1b[1;5H"))
	if got, want := term.Cursor(), (Cursor{X: 4, Y: 0, Visible: true, Style: 6}); got != want {
		t.Fatalf("Cursor() = %+v, want %+v", got, want)
	}
}

// DECTCEM: a child that hides the cursor (to draw its own, or while it
// repaints) reports it invisible; showing it again brings it back. RIS (the
// daemon's resync reset) restores a visible, default-shaped cursor.
func TestCursorVisibility(t *testing.T) {
	term := New(20, 4)
	defer term.Close()
	term.ingest([]byte("abc\x1b[?25l\x1b[1;2H"))
	if term.Cursor().Visible {
		t.Fatal("hidden cursor reported visible")
	}
	term.ingest([]byte("\x1b[?25h"))
	if c := term.Cursor(); !c.Visible || c.X != 1 {
		t.Fatalf("shown cursor = %+v", c)
	}
	term.ingest([]byte("\x1b[?25l\x1b[6 q\x1bcxyz"))
	if got, want := term.Cursor(), (Cursor{X: 3, Y: 0, Visible: true}); got != want {
		t.Fatalf("cursor after RIS = %+v, want %+v", got, want)
	}
}

// Visibility is per screen: an alternate-screen app hiding the cursor must not
// leave the primary screen's cursor hidden after it exits.
func TestCursorAltScreen(t *testing.T) {
	term := New(20, 4)
	defer term.Close()
	term.ingest([]byte("ab\x1b[?1049h\x1b[?25l"))
	if term.Cursor().Visible {
		t.Fatal("hidden alt-screen cursor reported visible")
	}
	term.ingest([]byte("\x1b[?1049l"))
	if c := term.Cursor(); !c.Visible || c.X != 2 {
		t.Fatalf("primary cursor not restored after leaving alt screen: %+v", c)
	}
}

// The pending-wrap column reports the last cell, never a column past the pane
// (which would land the hardware cursor in a neighbouring pane or border).
func TestCursorPendingWrap(t *testing.T) {
	term := New(4, 2)
	defer term.Close()
	term.ingest([]byte("abcd"))
	if c := term.Cursor(); c.X != 3 || c.Y != 0 {
		t.Fatalf("pending-wrap cursor = %+v, want last cell", c)
	}
}

// Scrolled back into history, the viewport shows no live cursor.
func TestCursorHiddenWhileScrolledBack(t *testing.T) {
	term := New(10, 2)
	defer term.Close()
	term.ingest([]byte("1\r\n2\r\n3\r\n4"))
	if !term.ScrollLines(1) {
		t.Fatal("could not scroll back")
	}
	if term.Cursor().Visible {
		t.Fatal("scrolled-back viewport reports a visible cursor")
	}
	term.ScrollLines(-1)
	if !term.Cursor().Visible {
		t.Fatal("cursor not back at the live edge")
	}
}
