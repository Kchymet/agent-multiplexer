//go:build linux || darwin

package nativetui

import (
	"strings"
	"testing"
	"time"

	"amux/internal/keymap"
	"amux/internal/vterm"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
)

// Raw C0 bytes go through production Bubble Tea decoding and rendering. The
// child input spy proves local history never becomes line-kill or EOF, while
// vacated bindings and bracketed paste still preserve those bytes.
func TestHalfPageKeysPTY(t *testing.T) {
	for _, rebound := range []bool{false, true} {
		name := "defaults"
		if rebound {
			name = "rebound"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			m, term, input := scrollModel(t, 43, 7)
			feedPane(t, term, historyRows(20))
			if rebound {
				for action, chord := range map[string]string{keymap.ScrollHalfUp: "f5", keymap.ScrollHalfDown: "f6"} {
					if _, err := keymap.Set(action, chord); err != nil {
						t.Fatal(err)
					}
				}
			}
			var err error
			m.keys, err = keymap.Load()
			if err != nil {
				t.Fatal(err)
			}
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			if err := pty.Setsize(master, &pty.Winsize{Cols: 70, Rows: 10}); err != nil {
				t.Fatal(err)
			}
			frames := make(chan accountsInputFrame, 512)
			outer := vterm.NewRemote(70, 10, nil, nil)
			p := newProgram(&accountsInputModel{model: m, frames: frames}, tea.WithInput(slave), tea.WithOutput(slave), tea.WithoutSignalHandler(), tea.WithEnvironment([]string{"TERM=xterm-256color"}))
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
			t.Cleanup(func() {
				p.Quit()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(3 * time.Second):
					p.Kill()
					t.Error("PTY did not stop")
				}
				slave.Close()
				master.Close()
				<-readDone
				outer.Close()
			})
			await := func(event string) {
				t.Helper()
				timer := time.NewTimer(3 * time.Second)
				defer timer.Stop()
				for {
					select {
					case f := <-frames:
						if f.event == event {
							return
						}
					case <-timer.C:
						t.Fatalf("missing decoded event %q", event)
					}
				}
			}
			send := func(raw, event string) {
				t.Helper()
				if _, err := master.Write([]byte(raw)); err != nil {
					t.Fatal(err)
				}
				await(event)
			}
			await("")
			waitPane(t, "initial outer rendered row", func() bool { return strings.Contains(outer.Snapshot().Text(1), "history-015") })
			expectedInput := ""
			if rebound {
				send("\x15", "key:ctrl+u")
				send("\x04", "key:ctrl+d")
				expectedInput = "\x15\x04"
				if input() != expectedInput || term.Viewport().Offset != 0 {
					t.Fatalf("vacated Ctrl keys: input=%q viewport=%+v", input(), term.Viewport())
				}
				send("\x1b[15~", "key:f5")
			} else {
				send("\x15", "key:ctrl+u")
			}
			if term.Viewport().Offset != 3 || input() != expectedInput {
				t.Fatalf("half up input=%q viewport=%+v", input(), term.Viewport())
			}
			waitPane(t, "older row in actual renderer", func() bool { return strings.Contains(outer.Snapshot().Text(1), "history-012") })
			if rebound {
				send("\x1b[17~", "key:f6")
			} else {
				send("\x04", "key:ctrl+d")
			}
			if term.Viewport().Offset != 0 || input() != expectedInput {
				t.Fatal("half down sent input or stayed in history")
			}
			waitPane(t, "live row in actual renderer", func() bool { return strings.Contains(outer.Snapshot().Text(1), "history-015") })
			// Literal shortcut names and actual control bytes in a bracketed paste
			// are text, not key bindings. Retain the existing plain-text forwarding.
			for _, text := range []string{"ctrl+u", "ctrl+d", "\x15\x04", "first\nctrl+u\nctrl+d"} {
				send("\x1b[200~"+text+"\x1b[201~", "key:["+text+"]")
				expectedInput += text
				if input() != expectedInput || term.Viewport().Offset != 0 {
					t.Fatalf("paste was routed as a shortcut: input=%q viewport=%+v", input(), term.Viewport())
				}
			}
		})
	}
}
