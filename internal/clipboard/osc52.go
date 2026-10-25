// Package clipboard carries a hosted agent's clipboard writes out of amux.
//
// Claude Code and Codex copy a mouse selection by emitting OSC 52
// (ESC ] 52 ; c ; <base64> BEL|ST) to their terminal. Inside amux that
// terminal is the vterm emulator mirroring the pane, not the user's terminal,
// so the sequence ends there unless amux re-emits it. Their other route — a
// clipboard tool such as clip.exe, wl-copy or xclip — runs inside the agent
// sandbox, which has neither the display variables nor the WSL interop those
// tools need. This package decodes the sequence (ParseOSC52), keeps it out of
// replayed scrollback (Stripper), and delivers the text from the unsandboxed
// UI process (Copier).
package clipboard

import (
	"bytes"
	"encoding/base64"
)

// ParseOSC52 decodes an OSC 52 payload as an emulator's OSC handler receives it
// ("52;Pc;Pd", without the introducer or terminator) and returns the text of a
// clipboard write. ok is false for anything amux must not act on: a query
// (Pd "?"), which would hand the user's clipboard to the agent; a write aimed
// only at the X primary selection or a cut buffer; an empty or malformed
// payload.
func ParseOSC52(data []byte) (text []byte, ok bool) {
	rest, found := bytes.CutPrefix(data, []byte("52;"))
	if !found {
		return nil, false
	}
	target, payload, found := bytes.Cut(rest, []byte{';'})
	if !found || len(payload) == 0 || bytes.Equal(payload, []byte("?")) {
		return nil, false
	}
	// An empty target means xterm's default ("s0"); 's' is the configurable
	// selection, which is the clipboard on every terminal Windows and macOS ship.
	if len(target) > 0 && !bytes.ContainsAny(target, "cs") {
		return nil, false
	}
	text, err := base64.StdEncoding.DecodeString(string(payload))
	if err != nil {
		if text, err = base64.RawStdEncoding.DecodeString(string(payload)); err != nil {
			return nil, false
		}
	}
	if len(text) == 0 {
		return nil, false
	}
	return text, true
}

// Sequence encodes text as an OSC 52 clipboard write, BEL-terminated (the form
// tmux itself emits and every OSC 52 terminal accepts). tmux wraps it in a DCS
// passthrough, doubling the inner ESC, so a tmux with allow-passthrough set
// relays it to the terminal outside.
func Sequence(text []byte, tmux bool) []byte {
	seq := append([]byte("\x1b]52;c;"), base64.StdEncoding.AppendEncode(nil, text)...)
	seq = append(seq, '\a')
	if !tmux {
		return seq
	}
	out := append([]byte("\x1bPtmux;\x1b"), seq...)
	return append(out, '\x1b', '\\')
}

// Stripper removes OSC 52 sequences from a byte stream, holding state across
// calls so a sequence split at any chunk boundary is still caught. The engine
// runs pane output through it into the replay buffer: a clipboard write is a
// one-time side effect, and replaying it on every attach would overwrite the
// user's clipboard with stale text whenever they switch back to an agent.
// Other sequences pass through byte-for-byte. The zero value is ready to use.
type Stripper struct {
	st   stripState
	pend []byte // ESC, ESC ], ESC ] digits: held until the OSC number is known
}

type stripState byte

const (
	stripGround  stripState = iota
	stripEsc                // pend holds ESC
	stripOscNum             // pend holds ESC ] and up to three digits
	stripDrop               // inside an OSC 52's data
	stripDropEsc            // ESC inside an OSC 52: ST, or the next escape
)

// Strip appends p, minus any OSC 52, to dst and returns the result. Bytes of a
// sequence still undecided at the end of p are held for the next call.
func (s *Stripper) Strip(dst, p []byte) []byte {
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch s.st {
		case stripGround:
			if c == 0x1b {
				s.st, s.pend = stripEsc, append(s.pend[:0], c)
				continue
			}
			dst = append(dst, c)
		case stripEsc:
			if c == ']' {
				s.st, s.pend = stripOscNum, append(s.pend, c)
				continue
			}
			dst, s.st = append(dst, s.pend...), stripGround
			i-- // c may itself start an escape
		case stripOscNum:
			num := s.pend[2:]
			switch {
			case c >= '0' && c <= '9' && len(num) < 3:
				s.pend = append(s.pend, c)
			case c == ';' && string(num) == "52":
				s.st, s.pend = stripDrop, s.pend[:0]
			default:
				dst, s.st = append(dst, s.pend...), stripGround
				i--
			}
		case stripDrop:
			switch c {
			case 0x07, 0x18, 0x1a: // BEL terminates; CAN/SUB cancel
				s.st = stripGround
			case 0x1b:
				s.st = stripDropEsc
			}
		case stripDropEsc:
			if c == '\\' {
				s.st = stripGround
				continue
			}
			// Any other ESC still ends the string (as the emulator's parser
			// treats it) and opens a new escape with c.
			s.st, s.pend = stripEsc, append(s.pend[:0], 0x1b)
			i--
		}
	}
	return dst
}
