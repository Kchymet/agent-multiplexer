// Package termmodes tracks the DEC private modes a program has set on its
// terminal, so a replay that starts mid-stream can restore them.
//
// The engine replays a bounded tail of each pane's output to a client that
// opens it. A full-screen program sets its modes once at startup — Codex sends
// ?1049h (alternate screen) and ?1000h/?1002h/?1003h/?1006h (mouse reporting)
// exactly once — so once its output outgrows the tail those bytes are gone. A
// client emulator rebuilt from the tail alone then believes the program is on
// the primary screen without mouse reporting, keeps wheel events local, and has
// no history to scroll: the wheel does nothing while keys still reach the
// program. Tracker follows the whole stream and Preamble restates the state.
package termmodes

import (
	"strconv"

	"github.com/charmbracelet/x/ansi"
)

// altScreenModes switch to the alternate screen, with the client emulator's
// semantics (charmbracelet/x/vt): either one enters it and either one's reset
// returns to the primary screen, whichever entered. The legacy ?47 does not
// switch screens there, so it is not tracked. At most one is restated.
var altScreenModes = []int{1049, 1047}

// tracked are the other modes restated, in order. They change how the client
// encodes input for the program (mouse reporting and its encoding, cursor-key
// form, bracketed paste, focus events, alternate scroll) or how it draws the
// program's cursor. Synchronized output (2026) is deliberately absent: a
// replay must not open a frame it cannot close.
var tracked = []int{1, 25, 1000, 1002, 1003, 1004, 1005, 1006, 1007, 1015, 1016, 2004}

// Tracker follows program output through the same VT parser the emulator
// uses (charmbracelet/x/ansi), so OSC, DCS, APC, PM and SOS string contents are
// never mistaken for sequences and sequences split across writes still parse.
// The zero value is ready. It is not safe for concurrent use.
type Tracker struct {
	p   *ansi.Parser
	set map[int]bool // last explicit h/l per tracked mode; absent = never set
	alt int          // alternate-screen mode currently on, 0 if none
}

// Write observes output bytes.
func (t *Tracker) Write(b []byte) {
	if t.p == nil {
		t.p = ansi.NewParser()
		// String payloads are skipped, not inspected: keep only a token buffer.
		t.p.SetDataSize(16)
		t.p.SetHandler(ansi.Handler{HandleCsi: t.csi, HandleEsc: t.esc})
	}
	t.p.Parse(b)
}

// esc handles RIS (ESC c), which resets every mode to its default.
func (t *Tracker) esc(cmd ansi.Cmd) {
	if cmd.Final() == 'c' && cmd.Intermediate() == 0 {
		t.set = nil
		t.alt = 0
	}
}

// csi applies DECSET/DECRST ("CSI ? Pm h" / "CSI ? Pm l"); other CSIs are ignored.
func (t *Tracker) csi(cmd ansi.Cmd, params ansi.Params) {
	final := cmd.Final()
	if cmd.Prefix() != '?' || cmd.Intermediate() != 0 || (final != 'h' && final != 'l') {
		return
	}
	on := final == 'h'
	params.ForEach(-1, func(_, mode int, _ bool) {
		if isAlt(mode) {
			if on {
				t.alt = mode
			} else {
				t.alt = 0
			}
			return
		}
		for _, m := range tracked {
			if m == mode {
				if t.set == nil {
					t.set = map[int]bool{}
				}
				t.set[mode] = on
			}
		}
	})
}

func isAlt(mode int) bool {
	for _, m := range altScreenModes {
		if m == mode {
			return true
		}
	}
	return false
}

// Preamble restates the tracked state as escape sequences, alternate screen
// first so per-screen state lands on the right screen. Replaying it before a
// tail of the stream leaves a client emulator in the state the program set:
// any mode the tail toggles again ends at the tail's last toggle, which is the
// current state. It is empty when the program has set nothing tracked.
func (t *Tracker) Preamble() []byte {
	var b []byte
	if t.alt != 0 {
		b = append(b, "\x1b[?"+strconv.Itoa(t.alt)+"h"...)
	}
	for _, m := range tracked {
		on, ok := t.set[m]
		if !ok {
			continue
		}
		f := byte('l')
		if on {
			f = 'h'
		}
		b = append(b, "\x1b[?"+strconv.Itoa(m)...)
		b = append(b, f)
	}
	return b
}
