//go:build linux || darwin

package nativetui

import (
	"testing"
	"time"

	"amux/internal/vterm"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
)

type redrawMsg struct{}

// The real Bubble Tea program, through a PTY into an emulated outer terminal:
// the child's bar cursor (vim INSERT mode) becomes the outer terminal's
// hardware cursor, as a bar, at the child's cell offset by the pane's
// position. It follows shape, movement and DECTCEM, hides when the rail takes
// focus, and on quit the user's terminal gets a visible default cursor back.
func TestHardwareCursorPTY(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, term, _ := scrollModel(t, 43, 7)
	feedPane(t, term, "\x1b[6 q> hi")
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Cols: 70, Rows: 10}); err != nil {
		t.Fatal(err)
	}
	frames := make(chan accountsInputFrame, 512)
	outer := vterm.NewRemote(70, 10, nil, nil)
	hw := newCursorOut(slave)
	m.hw = hw
	p := newProgram(&accountsInputModel{model: m, frames: frames}, tea.WithInput(slave), tea.WithOutput(hw), tea.WithoutSignalHandler(), tea.WithEnvironment([]string{"TERM=xterm-256color"}))
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				outer.Feed(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			p.Kill()
			<-done
		}
		slave.Close()
		master.Close()
		<-readDone
		outer.Close()
	})
	const paneX = sidebarWidth + 1
	expect := func(what string, want vterm.Cursor) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			got := outer.Cursor()
			if got == want || (!want.Visible && !got.Visible) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatalf("%s: outer cursor = %+v, want %+v", what, outer.Cursor(), want)
	}
	child := func(seq string) {
		t.Helper()
		feedPane(t, term, seq)
		p.Send(redrawMsg{}) // the daemon's data notification, in production
	}

	expect("INSERT bar after the prompt", vterm.Cursor{X: paneX + 4, Y: 1, Visible: true, Style: 6})
	// Cursor-only change: the frame is identical, the cursor moves and
	// becomes a block (vim NORMAL mode).
	child("\x1b[D\x1b[2 q")
	expect("NORMAL block one cell left", vterm.Cursor{X: paneX + 3, Y: 1, Visible: true, Style: 2})
	child("\x1b[?25l")
	expect("child hid its cursor", vterm.Cursor{})
	child("\x1b[?25h\x1b[5 q\r\nsecond")
	expect("blinking bar on the next row", vterm.Cursor{X: paneX + 6, Y: 2, Visible: true, Style: 5})
	if _, err := master.Write([]byte("\x1bh")); err != nil { // alt+h: focus the rail
		t.Fatal(err)
	}
	expect("rail focused", vterm.Cursor{})
	if _, err := master.Write([]byte("\x1ba")); err != nil { // alt+a: back to the agent
		t.Fatal(err)
	}
	expect("agent refocused", vterm.Cursor{X: paneX + 6, Y: 2, Visible: true, Style: 5})

	p.Quit()
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("program did not stop")
	}
	waitPane(t, "default visible cursor restored on exit", func() bool {
		c := outer.Cursor()
		return c.Visible && c.Style == 0
	})
}
