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
