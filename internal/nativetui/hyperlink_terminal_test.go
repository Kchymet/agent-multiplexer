//go:build linux || darwin

package nativetui

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
)

// OSC 8 hyperlinks an agent prints (Claude Code and Codex link URLs and file
// paths) must reach the user's terminal intact through amux's mirror: the pane
// is parsed into an emulator and re-rendered, so the link is re-encoded rather
// than passed through. The pinned emulator once stored OSC 8's params and URI
// swapped, re-emitting "ESC ] 8 ; <uri> ; <params>" — an empty or "id=…" URI
// that iTerm2 hands to LaunchServices on Cmd-click ("The application can't be
// opened. -50"). This runs the real Bubble Tea program through a PTY and checks
// the raw bytes it writes to the outer terminal.
func TestHyperlinksReachOuterTerminalPTY(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	m, term, _ := scrollModel(t, 90, 6)
	feedPane(t, term, "see \x1b]8;;https://example.com/a?b=1&c=2#frag\x1b\\docs\x1b]8;;\x1b\\ now\r\n"+
		"\x1b]8;id=k1;file:///Users/me/src/main.go\x07main.go:12\x1b]8;;\x07\r\n"+
		"\x1b]8;;file:///Users/me/My%20Dir/a.txt\x1b\\a.txt\x1b]8;;\x1b\\")

	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Cols: uint16(m.w), Rows: 10}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var out bytes.Buffer
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				mu.Lock()
				out.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	frames := make(chan accountsInputFrame, 512)
	p := newProgram(&accountsInputModel{model: m, frames: frames}, tea.WithInput(slave), tea.WithOutput(slave), tea.WithoutSignalHandler(), tea.WithEnvironment([]string{"TERM=xterm-256color"}))
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	t.Cleanup(func() {
		p.Kill()
		<-done
		slave.Close()
		master.Close()
		<-readDone
	})

	// Each link must open with an empty-or-param field first and the URI
	// last, exactly as the agent wrote it, and be closed again.
	want := []string{
		"\x1b]8;;https://example.com/a?b=1&c=2#frag\x07docs",
		"\x1b]8;id=k1;file:///Users/me/src/main.go\x07main.go:12",
		"\x1b]8;;file:///Users/me/My%20Dir/a.txt\x07a.txt",
		"\x1b]8;;\x07",
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		got := out.String()
		mu.Unlock()
		missing := ""
		for _, w := range want {
			if !strings.Contains(got, w) {
				missing = w
				break
			}
		}
		if missing == "" {
			for _, bad := range []string{"\x1b]8;https:", "\x1b]8;file:"} {
				if strings.Contains(got, bad) {
					t.Fatalf("outer terminal got a link with the URI in the params field (%q):\n%q", bad, got)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("outer terminal never got %q; OSC 8 seen: %q", missing, osc8s(got))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// osc8s extracts the OSC 8 sequences from s, for a readable failure.
func osc8s(s string) []string {
	var seqs []string
	for {
		i := strings.Index(s, "\x1b]8;")
		if i < 0 {
			return seqs
		}
		s = s[i:]
		j := strings.IndexAny(s, "\x07\x1b\\")
		if k := strings.IndexByte(s[1:], '\x07'); k >= 0 {
			j = k + 2
		}
		if j <= 0 || j > len(s) {
			j = len(s)
		}
		seqs = append(seqs, s[:j])
		s = s[j:]
	}
}
