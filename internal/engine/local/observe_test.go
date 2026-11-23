package local

import (
	"context"
	"strings"
	"testing"
	"time"

	"amux/internal/engine"
	"amux/internal/vterm"
)

// An attached client's emulator answers the child's startup queries through
// the same input path as keystrokes; those answers are not a human typing.
func TestTerminalReplyIsNotHumanInput(t *testing.T) {
	for _, tc := range []struct {
		in    string
		reply bool
	}{
		{"\x1b[12;40R", true},                      // cursor position report
		{"\x1b[?62;22c", true},                     // DA1
		{"\x1b[>1;10;0c", true},                    // DA2
		{"\x1b[0n", true},                          // DSR ok
		{"\x1b[?2004;1$y", true},                   // DECRPM
		{"\x1b[?1u", true},                         // kitty keyboard flags
		{"\x1b[I", true},                           // focus in
		{"\x1b]11;rgb:0000/0000/0000\x1b\\", true}, // background color reply
		{"\x1bP>|xterm(390)\x1b\\", true},          // XTVERSION
		{"\x1b[I\x1b[12;40R", true},                // several replies at once
		{"a", false},
		{"hello", false},
		{"\r", false},
		{"\x1b[A", false},         // arrow key
		{"\x1b[1;2R", false},      // shift+F3, not a CPR
		{"\x1b[<64;10;5M", false}, // mouse wheel in the pane: a person is there
		{"\x1b[200~pasted\x1b[201~", false},
		{"\x1b[I" + "x", false},
		{"\x1b", false},
	} {
		if got := terminalReply([]byte(tc.in)); got != tc.reply {
			t.Errorf("terminalReply(%q) = %v, want %v", tc.in, got, tc.reply)
		}
	}
}

// The instance mirrors its screen from the first byte and records only human
// input, so the daemon can read the composer and see whether someone is typing.
func TestInstanceObservesScreenAndHumanInput(t *testing.T) {
	eng := New()
	defer eng.Shutdown()
	key := engine.Key{AgentID: "obs", Tab: 0}
	inst, err := eng.Ensure(context.Background(), engine.Spec{
		Key: key, Argv: []string{"sh", "-c", "printf '\\033[2J\\033[H> composer here'; cat >/dev/null"}, Cols: 60, Rows: 10,
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	obs := inst.(interface {
		Screen() (vterm.Snapshot, bool)
		LastHumanInput() time.Time
		Started() time.Time
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		snap, ok := obs.Screen()
		if ok && strings.Contains(snap.Text(0), "composer here") {
			if snap.Cols != 60 || snap.Rows != 10 {
				t.Fatalf("mirror is %dx%d, want the pane's 60x10", snap.Cols, snap.Rows)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("screen never showed the output: %q", snap.Text(0))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if obs.Started().IsZero() || time.Since(obs.Started()) > time.Minute {
		t.Fatalf("Started = %v", obs.Started())
	}

	if !obs.LastHumanInput().IsZero() {
		t.Fatal("fresh instance reports human input")
	}
	inst.InputSequence([]engine.InputStep{{Bytes: []byte("amux text")}})
	inst.Input([]byte("\x1b[?62;22c"))
	if !obs.LastHumanInput().IsZero() {
		t.Fatal("amux's own input or a terminal reply counted as human input")
	}
	before := time.Now()
	inst.Input([]byte("k"))
	if got := obs.LastHumanInput(); got.Before(before) {
		t.Fatalf("keystroke not recorded: %v", got)
	}

	inst.Resize(40, 8)
	if snap, _ := obs.Screen(); snap.Cols != 40 || snap.Rows != 8 {
		t.Fatalf("mirror did not follow the resize: %dx%d", snap.Cols, snap.Rows)
	}
	eng.Kill(key)
	for deadline := time.Now().Add(3 * time.Second); inst.Alive() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := obs.Screen(); ok {
		t.Fatal("exited instance still reports a screen")
	}
}
