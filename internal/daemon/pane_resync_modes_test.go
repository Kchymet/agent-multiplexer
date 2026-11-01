package daemon

import (
	"bytes"
	"os"
	"testing"
	"time"

	"amux/internal/vterm"
)

// resyncedMirror is a client emulator that has applied a pane.reset (RIS) and
// then data, once its feed goroutine has parsed all of it.
func resyncedMirror(t *testing.T, data []byte) *vterm.Terminal {
	t.Helper()
	m := vterm.NewRemote(100, 28, func([]byte) {}, nil)
	t.Cleanup(func() { _ = m.Close() })
	m.Reset()
	m.Feed(data)
	m.Feed([]byte("\x1b[999;1HSETTLED"))
	deadline := time.Now().Add(5 * time.Second)
	for !bytes.Contains([]byte(m.Render()), []byte("SETTLED")) {
		if time.Now().After(deadline) {
			t.Fatal("mirror never parsed the resync")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return m
}

// codexStream is real Codex 0.157.1 output (docs/evidence/embedded-scroll): its
// startup, which sets the alternate screen and mouse reporting exactly once,
// followed by its synchronized repaints repeated until the stream outgrows n.
func codexStream(t *testing.T, n int) []byte {
	t.Helper()
	raw, err := os.ReadFile("../../docs/evidence/embedded-scroll/always-pty.raw")
	if err != nil {
		t.Fatal(err)
	}
	setup := bytes.Index(raw, []byte("\x1b[?1003h"))
	frame := bytes.Index(raw[max(setup, 0):], []byte("\x1b[?2026h"))
	if setup < 0 || frame < 0 {
		t.Fatal("capture lacks Codex's startup modes or a synchronized frame")
	}
	repaints := raw[setup+frame:]
	if bytes.Contains(repaints, []byte("\x1b[?1000h")) || bytes.Contains(repaints, []byte("\x1b[?1049h")) {
		t.Fatal("capture re-sends Codex's startup modes; a tail would not lose them")
	}
	out := append([]byte(nil), raw...)
	for len(out) <= n {
		out = append(out, repaints...)
	}
	return out
}

// A client that falls more than paneOutCap behind is resynced: pane.reset (its
// emulator clears) and the recent tail. Codex sets the alternate screen and
// mouse reporting once at startup, so a bare tail left the resynced mirror on
// the primary screen without mouse reporting: the wheel stayed local, with no
// history to scroll, while keys still reached Codex. The resync restates them.
func TestPaneOutputResyncRestatesCodexModes(t *testing.T) {
	cl := &connState{obuf: map[string]*paneOut{}, wake: make(chan struct{}, 1), done: make(chan struct{})}
	stream := codexStream(t, paneOutCap+paneOutKeep)
	for len(stream) > 0 {
		n := min(len(stream), 32<<10) // engine pump reads
		cl.paneOutput("p", stream[:n])
		stream = stream[n:]
	}
	b := cl.obuf["p"]
	if !b.reset {
		t.Fatal("backlog past paneOutCap did not resync")
	}
	if bytes.Contains(b.data[len(b.data)-paneOutKeep:], []byte("\x1b[?1000h")) {
		t.Fatal("retained tail still holds Codex's startup modes; the test would not exercise their loss")
	}
	mirror := resyncedMirror(t, b.data)
	if v := mirror.Viewport(); !v.AltScreen || !mirror.MouseTracked() {
		t.Fatalf("resynced mirror alt=%v mouse=%v; the resync dropped Codex's modes", v.AltScreen, mirror.MouseTracked())
	}

	// Modes the program turned off stay off across a later resync.
	cl.paneOutput("p", []byte("\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1049l"))
	cl.paneOutput("p", bytes.Repeat([]byte("x"), paneOutCap))
	mirror = resyncedMirror(t, cl.obuf["p"].data)
	if v := mirror.Viewport(); v.AltScreen || mirror.MouseTracked() {
		t.Fatalf("resync restated modes Codex had reset: alt=%v mouse=%v", v.AltScreen, mirror.MouseTracked())
	}
}
