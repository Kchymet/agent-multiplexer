package vterm

import (
	"strings"
	"testing"
	"time"
)

// TestScrubNeutralizesC1InOscTitle is the core regression guard for the ghost
// text bug: Claude Code's animated title "✳ Claude Code" (OSC 0) contains 0x9C
// — a UTF-8 continuation byte that the emulator's byte-oriented parser reads
// as the C1 String Terminator, ending the title early and printing the rest
// (" Claude Code") into the screen at the cursor. Scrubbed, the 0x9C becomes
// '?' and the sequence runs to its real BEL terminator.
func TestScrubNeutralizesC1InOscTitle(t *testing.T) {
	var s scrubber
	// "✳" is E2 9C B3: lead byte is fine, 0x9C collides with C1 ST.
	b := []byte("\x1b]0;\xe2\x9c\xb3 Claude Code\x07after")
	s.scrub(b)
	if got, want := string(b), "\x1b]0;\xe2?\xb3 Claude Code\x07after"; got != want {
		t.Fatalf("scrub = %q, want %q", got, want)
	}
	if s.st != scrubGround {
		t.Fatalf("state after complete sequence = %d, want ground", s.st)
	}
}

// TestScrubStatePersistsAcrossChunks: PTY reads split sequences arbitrarily, so
// the scrubber must carry its state between calls.
func TestScrubStatePersistsAcrossChunks(t *testing.T) {
	var s scrubber
	a := []byte("\x1b]0;\xe2")
	b := []byte("\x9c\xb3 title\x07")
	s.scrub(a)
	s.scrub(b)
	if b[0] != '?' {
		t.Fatalf("continuation byte across chunk boundary not scrubbed: %q", b)
	}
}

// TestScrubLeavesGroundTextAlone: the same glyphs as ordinary screen text must
// pass through untouched — there the parser collects whole UTF-8 runes first.
func TestScrubLeavesGroundTextAlone(t *testing.T) {
	var s scrubber
	in := "plain ✳ text \x1b[31mstyled\x1b[0m ✻"
	b := []byte(in)
	s.scrub(b)
	if string(b) != in {
		t.Fatalf("ground text mutated: %q", b)
	}
}

// TestScrubOscEscTerminator: an OSC ended by ESC \ (ST) returns to ground, and
// text after it is untouched.
func TestScrubOscEscTerminator(t *testing.T) {
	var s scrubber
	in := []byte("\x1b]2;\xc5\x93\x1b\\\xc5\x93")
	s.scrub(in)
	// œ (C5 93): 0x93 falls in the C1 range, but only inside the string; the
	// same glyph after the ST must pass through untouched.
	if got, want := string(in), "\x1b]2;\xc5?\x1b\\\xc5\x93"; got != want {
		t.Fatalf("scrub = %q, want %q", got, want)
	}
}

// TestScrubDcsIgnoresBel: BEL terminates OSC but not DCS — a BEL inside DCS
// data must not drop the scrubber back to ground early.
func TestScrubDcsIgnoresBel(t *testing.T) {
	var s scrubber
	in := []byte("\x1bPdata\x07\x9cmore\x1b\\")
	s.scrub(in)
	if got, want := string(in), "\x1bPdata\x07?more\x1b\\"; got != want {
		t.Fatalf("scrub = %q, want %q", got, want)
	}
}

// TestFeedTitleWithSpinnerDoesNotGhost is the end-to-end guard: streaming the
// real-world byte pattern through Feed must leave the screen free of leaked
// title text.
func TestFeedTitleWithSpinnerDoesNotGhost(t *testing.T) {
	term := NewRemote(80, 24, nil, nil)
	defer term.Close()

	term.Feed([]byte("BEFORE \x1b]0;\xe2\x9c\xb3 GHOST-TITLE\x07 AFTER"))

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
