package vterm

import (
	"bytes"
	"os"
	"testing"
	"time"
)

// TestFeedResyncRestatesModes: when the feed backlog passes feedCap, Feed keeps
// the recent tail behind a RIS. Real Codex output sets the alternate screen and
// mouse reporting once, at startup, so a bare tail left the mirror on the primary
// screen without mouse reporting and the wheel did nothing. The trim restates
// the modes the stream had set after the RIS.
func TestFeedResyncRestatesModes(t *testing.T) {
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

	term := NewRemote(100, 28, func([]byte) {}, nil)
	defer term.Close()
	// Wedge the feed goroutine (as in TestFeedResyncOnOverflow) so the backlog
	// accumulates in fbuf and trims.
	term.mu.Lock()
	term.Feed(raw)
	for {
		term.Feed(repaints)
		term.fmu.Lock()
		trimmed := len(term.fbuf) >= 2 && term.fbuf[0] == 0x1b && term.fbuf[1] == 'c'
		term.fmu.Unlock()
		if trimmed {
			break
		}
	}
	term.mu.Unlock()
	term.Feed(repaints) // wake the feed goroutine
	if !waitFor(5*time.Second, func() bool {
		term.fmu.Lock()
		defer term.fmu.Unlock()
		return len(term.fbuf) == 0
	}) {
		t.Fatal("backlog never drained")
	}
	time.Sleep(50 * time.Millisecond) // the drained batch is still being parsed
	term.mu.Lock()
	term.mu.Unlock()
	if v := term.Viewport(); !v.AltScreen || !term.MouseTracked() {
		t.Fatalf("after the feedCap trim alt=%v mouse=%v; the trim dropped Codex's modes", v.AltScreen, term.MouseTracked())
	}
}
