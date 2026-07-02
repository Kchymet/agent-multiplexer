package vterm

import "github.com/charmbracelet/x/ansi/parser"

// init patches the x/ansi parser transition table so string-sequence data
// (OSC, DCS, SOS, PM, APC) is treated as UTF-8 text, the way xterm behaves in
// UTF-8 mode: bytes 0x80–0x9F inside a string are data, not single-byte C1
// controls. Sequences still terminate on BEL (OSC) or ESC \ (ST) — the only
// forms a UTF-8 stream can carry.
//
// Without this, the parser is byte-oriented inside strings and honors C1
// controls there — but those byte values are also UTF-8 continuation bytes.
// Claude Code animates its window title with spinner glyphs (OSC 0
// "✳ Claude Code"): ✳ is E2 9C B3, and 0x9C is the C1 String Terminator, so
// the parser ended the title early and printed the rest — " Claude Code" —
// into the screen at the cursor, usually parked in the prompt bar. When the
// leak landed on the bottom row it wrapped and scrolled the alternate screen,
// after which the child's diff-based repaints landed one row off: the ghost
// text and broken chrome bug. A real terminal in UTF-8 mode never interprets
// lone C1 bytes, so only the mirror corrupted. The bug is unfixed upstream as
// of x/ansi v0.11.7 and master (June 2026).
//
// parser.Table is a package-level variable, so this patch is process-wide. The
// other in-process consumers (x/ansi width/wrap functions used by lipgloss and
// Bubble Tea) only see amux's own composed frames, which never contain string
// sequences, so the changed rows are unreachable there.
func init() {
	for _, st := range []parser.State{
		parser.DcsStringState,
		parser.OscStringState,
		parser.SosStringState,
		parser.PmStringState,
		parser.ApcStringState,
	} {
		parser.Table.AddRange(0x80, 0x9f, st, parser.PutAction, st)
	}
}
