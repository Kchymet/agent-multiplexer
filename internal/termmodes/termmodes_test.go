package termmodes

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
)

func preamble(chunks ...string) string {
	var t Tracker
	for _, c := range chunks {
		t.Write([]byte(c))
	}
	return string(t.Preamble())
}

func TestPreamble(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{name: "nothing set", chunks: []string{"hello\x1b[1;1H\x1b[2J"}, want: ""},
		{name: "codex startup", chunks: []string{"\x1b[?2004h\x1b[?1004h\x1b[?25l\x1b[?1049h\x1b[?1007l\x1b[?1000h\x1b[?1002h\x1b[?1006h\x1b[?1003h"},
			want: "\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1002h\x1b[?1003h\x1b[?1004h\x1b[?1006h\x1b[?1007l\x1b[?2004h"},
		{name: "multi-parameter", chunks: []string{"\x1b[?1000;1006h"}, want: "\x1b[?1000h\x1b[?1006h"},
		{name: "split across writes", chunks: []string{"\x1b", "[?10", "49", "h\x1b[?100", "0h"}, want: "\x1b[?1049h\x1b[?1000h"},
		{name: "disable after enable", chunks: []string{"\x1b[?1000h\x1b[?1006h", "\x1b[?1000l"}, want: "\x1b[?1000l\x1b[?1006h"},
		{name: "leave alternate screen", chunks: []string{"\x1b[?1049h\x1b[?1000h", "\x1b[?1049l"}, want: "\x1b[?1000h"},
		{name: "re-enter alternate screen", chunks: []string{"\x1b[?1049h\x1b[?1049l\x1b[?1047h"}, want: "\x1b[?1047h"},
		{name: "1047 exits 1049", chunks: []string{"\x1b[?1049h\x1b[?1000h", "\x1b[?1047l"}, want: "\x1b[?1000h"},
		{name: "1049 exits 1047", chunks: []string{"\x1b[?1047h", "\x1b[?1049l\x1b[?1006h"}, want: "\x1b[?1006h"},
		{name: "47 does not switch screens in the emulator", chunks: []string{"\x1b[?1049h\x1b[?47l"}, want: "\x1b[?1049h"},
		{name: "47 alone is not restated", chunks: []string{"\x1b[?47h"}, want: ""},
		{name: "re-entering while on keeps one restatement", chunks: []string{"\x1b[?1049h\x1b[?1047h"}, want: "\x1b[?1047h"},
		{name: "RIS resets", chunks: []string{"\x1b[?1049h\x1b[?1000h\x1bc\x1b[?2004h"}, want: "\x1b[?2004h"},
		{name: "synchronized output is not restated", chunks: []string{"\x1b[?2026h"}, want: ""},
		{name: "ANSI modes and other CSIs ignored", chunks: []string{"\x1b[4h\x1b[1000h\x1b[>1000h\x1b[?1000$h"}, want: ""},
		{name: "OSC payload text ignored", chunks: []string{"\x1b]0;[?1000h title\x07\x1b[?1006h"}, want: "\x1b[?1006h"},
		{name: "ESC inside OSC ends it, as in a terminal", chunks: []string{"\x1b]0;x\x1b[?1000h"}, want: "\x1b[?1000h"},
		{name: "OSC payload split, ST terminated", chunks: []string{"\x1b]2;x[?1049", "h\x1b\\", "\x1b[?2004h"}, want: "\x1b[?2004h"},
		{name: "DCS payload ignored", chunks: []string{"\x1bP1$r[?1000h\x1b\\\x1b[?1004h"}, want: "\x1b[?1004h"},
		{name: "APC payload split", chunks: []string{"\x1b_Gq=2;[?10", "49h\x9c", "\x1b[?1049h"}, want: "\x1b[?1049h"},
		{name: "8-bit C1 CSI", chunks: []string{"\x9b?1000h"}, want: "\x1b[?1000h"},
		{name: "CAN aborts a sequence", chunks: []string{"\x1b[?1000\x18h\x1b[?1006h"}, want: "\x1b[?1006h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := preamble(tc.chunks...); got != tc.want {
				t.Fatalf("preamble = %q, want %q", got, tc.want)
			}
			// The client emulator must agree: restating the modes reproduces
			// the alternate screen and wheel encoding the whole stream leaves.
			var all []byte
			for _, c := range tc.chunks {
				all = append(all, c...)
			}
			fa, fw := replayWheel(t, all)
			pa, pw := replayWheel(t, []byte(tc.want))
			if fa != pa || fw != pw {
				t.Fatalf("emulator after stream alt=%v wheel=%q, after preamble alt=%v wheel=%q", fa, fw, pa, pw)
			}
		})
	}
}

// The committed capture is real Codex 0.157.1 output in owned (alternate
// screen) mode. It sets its modes once, in its first few hundred bytes; a tail
// without them leaves an emulator on the primary screen with no mouse
// reporting. Preamble+tail restores exactly the state the whole stream left.
func TestPreambleRestoresCodexCaptureState(t *testing.T) {
	raw, err := os.ReadFile("../../docs/evidence/embedded-scroll/always-pty.raw")
	if err != nil {
		t.Fatal(err)
	}
	cut := bytes.Index(raw, []byte("\x1b[?1003h"))
	cut += bytes.Index(raw[cut:], []byte("\x1b[?2026h"))
	tail := raw[cut:]
	if bytes.Contains(tail, []byte("\x1b[?1000h")) || bytes.Contains(tail, []byte("\x1b[?1049h")) {
		t.Fatal("capture re-enables modes later; the tail would not show their loss")
	}
	var tr Tracker
	tr.Write(raw)
	tailAlt, tailWheel := replayWheel(t, tail)
	if tailAlt || tailWheel != "" {
		t.Fatalf("tail alone: alt=%v wheel=%q; capture is not a loss case", tailAlt, tailWheel)
	}
	const sgrWheelUp = "\x1b[<64;4;3M"
	for name, b := range map[string][]byte{"full stream": raw, "preamble+tail": append(tr.Preamble(), tail...)} {
		if alt, wheel := replayWheel(t, b); !alt || wheel != sgrWheelUp {
			t.Errorf("%s: alt=%v wheel=%q, want alt and %q", name, alt, wheel, sgrWheelUp)
		}
	}
}

// replayWheel feeds b to a fresh emulator, then reports whether it is on the
// alternate screen and what a wheel-up at (3,2) would send the program. Each
// step ends with a mode query no program sends; the reader stops after the
// second answer, before the emulator is closed, and every wait is bounded.
func replayWheel(t *testing.T, b []byte) (alt bool, wheel string) {
	t.Helper()
	const (
		sentinel = "\x1b[?12345$p"
		answer   = "\x1b[?12345;"
		steps    = 2
	)
	e := vt.NewEmulator(100, 28)
	defer e.Close()
	out := make(chan string, 64)
	go func() {
		defer close(out)
		buf := make([]byte, 256)
		var acc string
		for seen := 0; seen < steps; {
			n, err := e.Read(buf)
			if n > 0 {
				out <- string(buf[:n])
				acc += string(buf[:n])
				if k := strings.Index(acc, answer); k >= 0 && strings.Contains(acc[k:], "$y") {
					seen++
					acc = acc[k+len(answer):]
				}
			}
			if err != nil {
				return
			}
		}
	}()
	timeout := time.After(5 * time.Second)
	// The emulator answers on an unbuffered pipe, so each write runs on its
	// own goroutine while this one collects replies up to the step's answer.
	step := func(f func()) string {
		t.Helper()
		done := make(chan struct{})
		go func() { defer close(done); f(); _, _ = e.Write([]byte(sentinel)) }()
		var got string
		for {
			select {
			case r, ok := <-out:
				got += r
				if k := strings.Index(got, answer); k >= 0 && strings.HasSuffix(got, "$y") {
					<-done
					return got[:k]
				}
				if !ok {
					t.Fatalf("emulator reply pipe ended early: %q", got)
				}
			case <-timeout:
				t.Fatalf("emulator did not answer the step sentinel: %q", got)
			}
		}
	}
	step(func() { _, _ = e.Write(b) })
	alt = e.IsAltScreen()
	wheel = step(func() { e.SendMouse(vt.MouseWheel{X: 3, Y: 2, Button: vt.MouseWheelUp}) })
	select { // the reader has stopped reading before Close runs
	case _, ok := <-out:
		if ok {
			t.Fatal("unexpected emulator reply after the final sentinel")
		}
	case <-timeout:
		t.Fatal("emulator reader did not stop")
	}
	return alt, wheel
}
