package vterm

// scrubber neutralizes C1-range bytes (0x80–0x9F) inside ANSI string sequences
// (OSC, DCS, SOS, PM, APC) before the stream reaches the emulator.
//
// The x/ansi parser the emulator uses is byte-oriented inside string sequences
// and still honors single-byte C1 controls there — but those byte values are
// also UTF-8 continuation bytes. Claude Code animates its window title with
// spinner glyphs ("✳ Claude Code", OSC 0): ✳ is E2 9C B3, and 0x9C is the C1
// String Terminator, so the parser ends the title early and the rest of it —
// " Claude Code" — is printed into the screen at the cursor (usually parked in
// the prompt bar). When the leak lands on the bottom row it wraps and scrolls
// the alternate screen, after which the child's diff-based repaints land one
// row off — the ghost text and broken chrome bug. A real terminal in UTF-8
// mode never interprets lone C1 bytes, so only the mirror corrupted.
//
// Replacing 0x80–0x9F with '?' inside string data keeps the sequence intact
// (it terminates where the child intended) at the cost of mangling non-ASCII
// title/path text, which amux never displays anyway. Ground-state text is
// untouched: there the parser collects UTF-8 runes before consulting C1 rules.
//
// State persists across chunks (escape sequences split arbitrarily at read
// boundaries); the zero value starts in ground state.
type scrubber struct {
	st  scrubState
	osc bool // current string sequence is an OSC, which BEL may terminate
}

type scrubState byte

const (
	scrubGround scrubState = iota
	scrubEsc               // after ESC: the next byte selects the sequence kind
	scrubString            // inside a string sequence's data
)

// scrub rewrites b in place. The caller owns b (pump's read buffer, the feed
// backlog), so mutation is safe.
func (s *scrubber) scrub(b []byte) {
	for i, c := range b {
		switch s.st {
		case scrubGround:
			if c == 0x1b {
				s.st = scrubEsc
			}
		case scrubEsc:
			switch c {
			case ']':
				s.st, s.osc = scrubString, true
			case 'P', 'X', '^', '_':
				s.st, s.osc = scrubString, false
			case 0x1b: // ESC ESC: still deciding
			default:
				s.st = scrubGround
			}
		case scrubString:
			switch {
			case c == 0x1b:
				// ESC dispatches the string; the next byte is a fresh escape
				// (ST's '\', or anything else — including another string).
				s.st = scrubEsc
			case c == 0x07 && s.osc:
				s.st = scrubGround // BEL terminates an OSC
			case c == 0x18 || c == 0x1a:
				s.st = scrubGround // CAN/SUB cancel the sequence
			case c >= 0x80 && c <= 0x9f:
				b[i] = '?'
			}
		}
	}
}
