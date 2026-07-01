package vterm

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
)

// waitFor polls f until it returns true or the deadline elapses.
func waitFor(d time.Duration, f func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return f()
}

// TestAltScrollWithoutMouseTracking proves the alternate-scroll fallback: a
// full-screen child on the alternate screen that hasn't armed mouse tracking
// still scrolls, because a vertical wheel is translated into cursor-key presses
// and delivered on the child's stdin. Without the fallback the emulator drops
// the wheel and nothing reaches the child — the reported bug.
func TestAltScrollWithoutMouseTracking(t *testing.T) {
	got := make(chan []byte, 8)
	term := NewRemote(80, 24, func(b []byte) { got <- append([]byte(nil), b...) }, nil)
	defer term.Close()

	// Child enters the alternate screen (DECSET 1049) but never enables mouse
	// tracking — the situation for less/vim/tmux-without-mouse.
	term.Feed([]byte("\x1b[?1049h"))
	if !waitFor(time.Second, term.emu.IsAltScreen) {
		t.Fatal("emulator did not enter alt screen")
	}

	term.MouseEvent(vt.MouseWheel{Button: vt.MouseWheelUp})
	term.MouseEvent(vt.MouseWheel{Button: vt.MouseWheelDown})

	up := readInput(t, got)
	if want := strings.Repeat("\x1b[A", altScrollLines); up != want {
		t.Fatalf("wheel-up sent %q, want %q (%d cursor-up presses)", up, want, altScrollLines)
	}
	down := readInput(t, got)
	if want := strings.Repeat("\x1b[B", altScrollLines); down != want {
		t.Fatalf("wheel-down sent %q, want %q", down, want)
	}
}

// TestWheelForwardedWhenMouseTracked proves that once the child enables mouse
// reporting (here SGR button-event tracking), the raw wheel event is encoded and
// forwarded instead of being turned into cursor keys.
func TestWheelForwardedWhenMouseTracked(t *testing.T) {
	got := make(chan []byte, 8)
	term := NewRemote(80, 24, func(b []byte) { got <- append([]byte(nil), b...) }, nil)
	defer term.Close()

	// Alt screen + button-event tracking (1002) + SGR encoding (1006): a
	// mouse-aware TUI. It wants the raw event, not alternate-scroll keys.
	term.Feed([]byte("\x1b[?1049h\x1b[?1002h\x1b[?1006h"))
	if !waitFor(time.Second, term.mouseTracked) {
		t.Fatal("mouse tracking not registered")
	}

	term.MouseEvent(vt.MouseWheel{Button: vt.MouseWheelUp, X: 4, Y: 2})

	// SGR wheel-up press at 1-based (5,3): CSI < 64 ; 5 ; 3 M.
	if in := readInput(t, got); in != "\x1b[<64;5;3M" {
		t.Fatalf("mouse-tracked wheel sent %q, want an SGR wheel-up report", in)
	}
}

func readInput(t *testing.T, ch <-chan []byte) string {
	t.Helper()
	select {
	case b := <-ch:
		return string(b)
	case <-time.After(2 * time.Second):
		t.Fatal("no input delivered to child")
		return ""
	}
}

// TestTerminalCapturesOutput proves the PTY → VT-emulator pipeline: a child's
// output reaches the rendered screen. This is the core mechanism the native TUI
// relies on to draw an embedded (tmux/agent) terminal.
func TestTerminalCapturesOutput(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	term := New(80, 24)
	if err := term.Start(exec.Command("bash", "-c", "printf 'HELLO_VTERM'; sleep 1")); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer term.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(term.Render(), "HELLO_VTERM") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected HELLO_VTERM on screen, got:\n%s", term.Render())
}

// TestForwardsQueryResponses proves the emulator's replies to terminal queries
// reach the child — the regression guard for the native-TUI attach hang. A child
// emits a Primary Device Attributes query (CSI c), blocks until it reads the
// reply on stdin, then prints a marker. tmux/Claude both send this query on
// startup; if vterm doesn't drain the emulator's response pipe, the parser
// blocks inside emu.Write while pump holds the render lock, freezing the UI. Here
// that manifests as the marker never appearing (Render would also deadlock).
func TestForwardsQueryResponses(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	term := New(80, 24)
	// Emit the DA1 query immediately followed by a marker. The emulator answers
	// DA1 by writing into its response pipe; if vterm doesn't drain that pipe the
	// parser blocks mid-Write (holding the render lock), so the bytes after the
	// query — the marker — are never parsed and Render deadlocks. With the pipe
	// drained, parsing continues and the marker reaches the screen.
	script := `printf '\033[cMARKER_AFTER_DA1'; sleep 2`
	if err := term.Start(exec.Command("bash", "-c", script)); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer term.Close()

	found := make(chan struct{})
	go func() {
		for {
			if strings.Contains(term.Render(), "MARKER_AFTER_DA1") {
				close(found)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	select {
	case <-found:
	case <-time.After(3 * time.Second):
		t.Fatal("bytes after the DA1 query never rendered (response pipe not drained → pump blocked, render deadlocked)")
	}
}

// TestResize keeps the emulator usable after a resize.
func TestResize(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	term := New(80, 24)
	if err := term.Start(exec.Command("bash", "-c", "sleep 1")); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer term.Close()
	term.Resize(100, 30)
	// Render must not panic and should reflect the new height.
	_ = term.Render()
}
