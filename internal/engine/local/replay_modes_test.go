package local

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"

	"amux/internal/engine"
	"amux/internal/vterm"
)

// codexStartup is the mode setup real Codex 0.157.1 sends exactly once, in
// owned alternate-screen mode (docs/evidence/embedded-scroll/always-pty.raw).
const codexStartup = "\x1b[?2004h\x1b[?1004h\x1b[?25l\x1b[?1049h\x1b[?1007l\x1b[?1000h\x1b[?1002h\x1b[?1006h\x1b[?1003h"

// A pane reopened after its agent's output outgrew the replay ring must still
// know the agent is on the alternate screen with mouse reporting on, or the
// client keeps the wheel local (with no history to scroll) and the wheel does
// nothing while Page Up/Down keys still reach the agent. Modes the program
// turned off again, or reset, must stay off: its wheel stays amux's own.
func TestReplayAfterOverflowRestatesTerminalModes(t *testing.T) {
	for _, tc := range []struct {
		name, setup string
		tracked     bool
	}{
		{name: "codex startup", setup: codexStartup, tracked: true},
		{name: "tracked app disables", setup: "\x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[?1000l\x1b[?1006l\x1b[?1049l"},
		{name: "cross-alias screen exit", setup: "\x1b[?1049h\x1b[?1000h\x1b[?1000l\x1b[?1047l"},
		{name: "tracked app resets", setup: "\x1b[?1049h\x1b[?1002h\x1b[?1006h\x1bc"},
		{name: "re-enabled after disable", setup: "\x1b[?1000h\x1b[?1000l\x1b[?1049h\x1b[?1003h\x1b[?1006h", tracked: true},
	} {
		t.Run(tc.name, func(t *testing.T) { replayAfterOverflow(t, tc.setup, tc.tracked) })
	}
}

func replayAfterOverflow(t *testing.T, setup string, tracked bool) {
	dir := t.TempDir()
	got := filepath.Join(dir, "input")
	// Setup modes once, then well over the ring of synchronized, cursor-
	// addressed frames (what Codex repaints), then record raw input.
	script := `printf '` + strings.ReplaceAll(setup, "\x1b", `\033`) + `'
i=0; while [ $i -lt 1400 ]; do printf '\033[?2026h\033[H%0200d\033[?2026l' $i; i=$((i+1)); done
stty raw -echo; printf 'READY'; exec cat > "$1"`
	e := New()
	key := engine.Key{AgentID: "replay-modes"}
	in, err := e.Ensure(context.Background(), engine.Spec{Key: key, Cols: 100, Rows: 28, Argv: []string{"sh", "-c", script, "sh", got}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Kill(key)
	var mu sync.Mutex
	var live []byte
	cancel := in.Subscribe(engine.Sink{Output: func(b []byte) { mu.Lock(); live = append(live, b...); mu.Unlock() }})
	awaitCond(t, "child ready", func() bool { mu.Lock(); defer mu.Unlock(); return bytes.Contains(live, []byte("READY")) })
	cancel()

	local := in.(*instance)
	local.mu.Lock()
	trimmed, ring := local.ringTrimmed, append([]byte(nil), local.ring...)
	local.mu.Unlock()
	if !trimmed || bytes.Contains(ring, []byte("\x1b[?100")) || bytes.Contains(ring, []byte("\x1b[?1049")) || bytes.Contains(ring, []byte("\x1bc")) {
		t.Fatalf("ring trimmed=%v still holds the setup; overflow not exercised", trimmed)
	}

	// Reopen the pane exactly as the native TUI does: a remote mirror fed by
	// Subscribe, whose input goes back to the instance.
	term := vterm.NewRemote(100, 28, in.Input, nil)
	defer term.Close()
	in.Subscribe(engine.Sink{Output: term.Feed})()
	awaitCond(t, "replay parsed", func() bool { return strings.Contains(term.Render(), "READY") })
	if v := term.Viewport(); v.AltScreen != tracked || term.MouseTracked() != tracked {
		t.Fatalf("reopened mirror alt=%v mouse=%v, want both %v", v.AltScreen, term.MouseTracked(), tracked)
	}
	term.MouseEvent(vt.MouseWheel{X: 3, Y: 2, Button: vt.MouseWheelUp})
	_, _ = term.Write([]byte("Z")) // marks the end of whatever the wheel sent
	var input []byte
	awaitCond(t, "input at the agent", func() bool {
		input, _ = os.ReadFile(got)
		return bytes.HasSuffix(input, []byte("Z"))
	})
	if want := "\x1b[<64;4;3MZ"; tracked && string(input) != want {
		t.Fatalf("agent input %q, want %q", input, want)
	} else if !tracked && string(input) != "Z" {
		t.Fatalf("untracked wheel reached the agent: %q", input)
	}
}

func awaitCond(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
