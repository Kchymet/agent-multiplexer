package vterm

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestFeedLossless is the text-shadowing regression on the client side. Streamed
// output is a stateful byte stream: dropping any of it (e.g. an erase sequence)
// leaves the emulator with stale cells. Feed must never drop — a large burst of
// writes must all reach the screen, in order.
func TestFeedLossless(t *testing.T) {
	term := NewRemote(40, 6, func([]byte) {}, nil)
	defer term.Close()

	const n = 8000
	for i := 0; i < n; i++ {
		term.Feed([]byte(fmt.Sprintf("\x1b[H\x1b[2Kvalue=%d", i)))
	}
	want := fmt.Sprintf("value=%d", n-1)
	if !waitFor(2*time.Second, func() bool { return strings.Contains(term.Render(), want) }) {
		t.Fatalf("final value %q never rendered — output was dropped or reordered", want)
	}
}

// TestFeedResyncOnOverflow covers the safety valve: a backlog past feedCap (the
// feed goroutine wedged) must not grow without bound. Feed keeps only the recent
// tail and prepends a RIS so the emulator clears before the retained repaint.
// The feed goroutine is wedged by holding t.mu so its emu.Write can't proceed,
// forcing the backlog to accumulate in fbuf.
func TestFeedResyncOnOverflow(t *testing.T) {
	block := make(chan struct{})
	term := NewRemote(80, 24, func([]byte) {}, nil)
	defer term.Close()

	term.mu.Lock()
	go func() { <-block; term.mu.Unlock() }()
	defer close(block)

	chunk := make([]byte, 64<<10)
	for i := range chunk {
		chunk[i] = 'x'
	}
	for i := 0; ; i++ {
		term.Feed(chunk)
		term.fmu.Lock()
		hasRIS := len(term.fbuf) >= 2 && term.fbuf[0] == 0x1b && term.fbuf[1] == 'c'
		size := len(term.fbuf)
		term.fmu.Unlock()
		if hasRIS {
			if size != feedKeep+2 {
				t.Fatalf("after the feedCap trim fbuf is %d bytes, want RIS+tail (%d)", size, feedKeep+2)
			}
			break
		}
		if i > (feedCap/len(chunk))*2 {
			t.Fatal("backlog never triggered the feedCap trim")
		}
	}
}
