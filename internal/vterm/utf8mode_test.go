package vterm

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
)

// TestOscTitleUtf8Preserved is the core regression guard for the ghost text
// bug, and proves the fix is lossless: Claude Code's animated title
// "✳ Claude Code" (OSC 0) contains 0x9C — a UTF-8 continuation byte the
// unpatched parser reads as the C1 String Terminator, ending the title early
// and printing the rest into the screen at the cursor. With the table patched
// (utf8mode.go), the full title reaches the Title callback intact and nothing
// leaks into the grid.
func TestOscTitleUtf8Preserved(t *testing.T) {
	emu := vt.NewEmulator(80, 24)
	var title string
	emu.SetCallbacks(vt.Callbacks{Title: func(s string) { title = s }})

	// "✳" is E2 9C B3: the 0x9C would dispatch the OSC on an unpatched table.
	_, _ = emu.Write([]byte("BEFORE \x1b]0;\xe2\x9c\xb3 GHOST-TITLE\x07 AFTER"))

	if want := "\xe2\x9c\xb3 GHOST-TITLE"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
	screen := emu.String()
	if strings.Contains(screen, "GHOST-TITLE") {
		t.Fatalf("title text leaked into the screen:\n%s", screen)
	}
	if !strings.Contains(screen, "BEFORE  AFTER") {
		t.Fatalf("surrounding ground text mangled:\n%s", screen)
	}
}

// TestOscTitleEscTerminator: a string ended by ESC \ (the ST form a UTF-8
// stream can carry) still terminates, with C1-range continuation bytes (œ is
// C5 93) kept as data.
func TestOscTitleEscTerminator(t *testing.T) {
	emu := vt.NewEmulator(80, 24)
	var title string
	emu.SetCallbacks(vt.Callbacks{Title: func(s string) { title = s }})

	_, _ = emu.Write([]byte("\x1b]2;\xc5\x93\x1b\\after"))

	if want := "\xc5\x93"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
	if screen := emu.String(); !strings.HasPrefix(screen, "after") {
		t.Fatalf("screen after ST = %q…, want it to start with %q", screen[:16], "after")
	}
}

// TestFeedTitleWithSpinnerDoesNotGhost is the end-to-end guard through the
// production pipeline: streaming the real-world byte pattern through Feed —
// split mid-rune across chunks, as PTY reads do — must leave the rendered
// screen free of leaked title text.
func TestFeedTitleWithSpinnerDoesNotGhost(t *testing.T) {
	term := NewRemote(80, 24, nil, nil)
	defer term.Close()

	term.Feed([]byte("BEFORE \x1b]0;\xe2"))
	term.Feed([]byte("\x9c\xb3 GHOST-TITLE\x07 AFTER"))

	ok := waitFor(time.Second, func() bool {
		return strings.Contains(term.Render(), "BEFORE  AFTER")
	})
	screen := term.Render()
	if !ok {
		t.Fatalf("expected BEFORE/AFTER text on screen, got:\n%s", screen)
	}
	if strings.Contains(screen, "GHOST-TITLE") {
		t.Fatalf("title text leaked into the screen:\n%s", screen)
	}
}
