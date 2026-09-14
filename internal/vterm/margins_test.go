package vterm

import "testing"

func TestReplayMarginsOutsideViewportDoNotPanic(t *testing.T) {
	for _, stream := range []string{
		"\x1b[1;25r\x1b[H\x1bM",  // reverse index with an old, taller viewport
		"\x1b[1;25r\x1b[H\x1b[L", // insert line uses the same region
	} {
		term := New(53, 21)
		func() {
			defer term.Close()
			term.mu.Lock()
			defer term.mu.Unlock()
			term.emu.WriteString(stream)
		}()
	}
}

func TestValidScrollMarginsStillApply(t *testing.T) {
	term := New(10, 4)
	defer term.Close()
	term.emu.WriteString("\x1b[1;1Htop\x1b[2;1Hmiddle\x1b[4;1Hbottom\x1b[2;3r\x1b[2;1H\x1bM")
	if term.emu.CellAt(0, 0).Content != "t" || term.emu.CellAt(0, 3).Content != "b" {
		t.Fatal("scroll changed rows outside valid margins")
	}
	if term.emu.CellAt(0, 2).Content != "m" {
		t.Fatal("valid scroll margins were ignored")
	}
}
