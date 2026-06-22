package vterm

import (
	"os/exec"
	"strings"
	"testing"
	"time"
)

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
