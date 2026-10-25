//go:build linux || darwin

package nativetui

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/termoutput"
	"amux/internal/vterm"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
)

// Drive production key/mouse decoding, Update, commands, and the actual Bubble
// Tea renderer through one PTY. The output wrapper must preserve terminal size
// detection and keep a clipboard escape outside rendered frame sequences.
func TestSelectionPTYRendererAndClipboard(t *testing.T) {
	const width, height = 70, 10
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Cols: width, Rows: height}); err != nil {
		t.Fatal(err)
	}
	m, term, input := scrollModel(t, width-sidebarWidth-1, height-3)
	feedPane(t, term, historyRows(20)+"\x1b[H\x1b[2JA界éZ\r\nsecond\x1b[?1002h\x1b[?1006h")
	output := termoutput.New(slave)
	m.copyText = output.Copy
	frames := make(chan accountsInputFrame, 512)
	fixture := &accountsInputModel{model: m, frames: frames}
	p := newProgram(fixture, tea.WithInput(slave), tea.WithOutput(output), tea.WithoutSignalHandler(), tea.WithEnvironment([]string{"TERM=xterm-256color"}))
	outer := vterm.NewRemote(width, height, nil, nil)
	var mu sync.Mutex
	var raw bytes.Buffer
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 8192)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				mu.Lock()
				raw.Write(buf[:n])
				mu.Unlock()
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
			t.Error("selection PTY did not stop")
		}
		slave.Close()
		master.Close()
		<-readDone
		outer.Close()
	})
	awaitFrame := func(predicate func(accountsInputFrame) bool) accountsInputFrame {
		t.Helper()
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		for {
			select {
			case f := <-frames:
				if predicate(f) {
					return f
				}
			case <-timer.C:
				t.Fatal("PTY input did not reach expected model frame")
			}
		}
	}
	send := func(raw string) {
		t.Helper()
		if _, err := master.Write([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	readRaw := func() string { mu.Lock(); defer mu.Unlock(); return raw.String() }
	save := func(name string) {
		t.Helper()
		if dir := os.Getenv("AMUX_SELECTION_EVIDENCE"); dir != "" {
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			frame := outer.Render()
			for suffix, content := range map[string]string{".ansi": frame, ".txt": ansi.Strip(frame)} {
				if err := os.WriteFile(filepath.Join(dir, name+suffix), []byte(content+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	awaitFrame(func(f accountsInputFrame) bool { return strings.Contains(f.view, "A界éZ") })
	waitPane(t, "actual terminal mouse modes", func() bool {
		return strings.Contains(readRaw(), "\x1b[?1002h") && strings.Contains(readRaw(), "\x1b[?1006h")
	})
	// Shift owns the press locally even though the child reports mouse.
	// Remove Shift on motion/release to verify capture survives raw decoding.
	send("\x1b[<4;29;2M\x1b[<32;31;2M")
	awaitFrame(func(f accountsInputFrame) bool { return f.events >= 2 })
	waitPane(t, "actual renderer wide-cell highlight", func() bool {
		s := outer.Snapshot()
		return s.Cells[1][28].Content == "界" && s.Cells[1][28].Style.Bg == ansi.IndexedColor(24) &&
			s.Cells[1][30].Content == "é" && s.Cells[1][30].Style.Bg == ansi.IndexedColor(24)
	})
	if strings.Contains(readRaw(), "\x1b]52;") {
		t.Fatal("clipboard write before explicit release")
	}
	save("selected")
	send("\x1b[<0;31;2m")
	awaitFrame(func(f accountsInputFrame) bool { return f.events >= 3 })
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("界é")) + "\a"
	waitPane(t, "OSC52 on the rendered PTY", func() bool { return strings.Contains(readRaw(), want) })
	if strings.Count(readRaw(), "\x1b]52;") != 1 || input() != "" {
		t.Fatalf("copy count or local routing incorrect: child=%q", input())
	}
	// Exercise actual key decoding and outer header rendering, not direct
	// calls to handleKey/View. The child must receive neither history chord.
	send("\x1b[5;3~")
	awaitFrame(func(f accountsInputFrame) bool { return f.event == "key:alt+pgup" })
	waitPane(t, "history header in outer renderer", func() bool {
		return term.Viewport().Offset > 0 && strings.Contains(outer.Snapshot().Text(0), "↑")
	})
	save("history")
	send("\x1b[1;3F")
	awaitFrame(func(f accountsInputFrame) bool { return f.event == "key:alt+end" })
	waitPane(t, "live header in outer renderer", func() bool {
		return term.Viewport().Offset == 0 && !strings.Contains(outer.Snapshot().Text(0), "↑")
	})
	save("live")
	if input() != "" {
		t.Fatalf("local history chords leaked to child: %q", input())
	}
	// Ask Bubble Tea to query the resized real output descriptor. WithOutput
	// must preserve term.File or this message never yields a WindowSizeMsg.
	if err := pty.Setsize(master, &pty.Winsize{Cols: 80, Rows: 12}); err != nil {
		t.Fatal(err)
	}
	p.Send(tea.WindowSize()())
	awaitFrame(func(f accountsInputFrame) bool {
		return len(strings.Split(f.view, "\n")) == 12 && ansi.StringWidth(strings.Split(f.view, "\n")[0]) == 80
	})
	if v := term.Viewport(); v.Cols != 53 || v.Rows != 9 {
		t.Fatalf("real descriptor resize lost: %+v", v)
	}
}
