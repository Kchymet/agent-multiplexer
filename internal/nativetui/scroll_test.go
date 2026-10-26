package nativetui

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"amux/internal/keymap"
	"amux/internal/vterm"
)

func waitPane(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
func feedPane(t *testing.T, term *vterm.Terminal, data string) {
	t.Helper()
	rev := term.Viewport().Revision
	term.Feed([]byte(data))
	waitPane(t, "pane output", func() bool { return term.Viewport().Revision > rev })
}
func scrollModel(t *testing.T, cols, rows int) (*model, *vterm.Terminal, func() string) {
	t.Helper()
	var mu sync.Mutex
	var input strings.Builder
	term := vterm.NewRemote(cols, rows, func(b []byte) { mu.Lock(); defer mu.Unlock(); input.Write(b) }, nil)
	t.Cleanup(func() { _ = term.Close() })
	m := railModel(3, rows+3)
	m.w = cols + sidebarWidth + 1
	m.attached = "ag00"
	m.focus = focusAgent
	m.terms = map[paneKey]*vterm.Terminal{{m.attached, tabAgent}: term}
	return m, term, func() string { mu.Lock(); defer mu.Unlock(); return input.String() }
}
func historyRows(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "history-%03d\r\n", i)
	}
	return b.String()
}
func paneWheel(button tea.MouseButton) tea.MouseMsg {
	return tea.MouseMsg{X: sidebarWidth + 3, Y: 3, Button: button, Action: tea.MouseActionPress}
}

func TestPaneWheelRouting(t *testing.T) {
	for _, tc := range []struct {
		name                         string
		tracked, shift, alt, forward bool
	}{
		{name: "local"}, {name: "alt local", alt: true},
		{name: "reporting", tracked: true, forward: true}, {name: "alt reporting", tracked: true, alt: true, forward: true},
		{name: "shift overrides reporting", tracked: true, shift: true}, {name: "shift overrides alt", tracked: true, shift: true, alt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, term, input := scrollModel(t, 60, 6)
			data := historyRows(20)
			if tc.tracked {
				data += "\x1b[?1000h\x1b[?1006h"
			}
			feedPane(t, term, data)
			ev := paneWheel(tea.MouseButtonWheelUp)
			ev.Shift = tc.shift
			ev.Alt = tc.alt
			m.handleMouse(ev)
			if tc.forward {
				want := "\x1b[<64;3;3M"
				if tc.alt {
					want = "\x1b[<72;3;3M"
				}
				waitPane(t, "forwarded wheel", func() bool { return input() == want })
				if term.Viewport().Offset != 0 {
					t.Fatal("forwarded wheel scrolled locally")
				}
			} else {
				if term.Viewport().Offset != 3 || input() != "" {
					t.Fatalf("offset=%d input=%q", term.Viewport().Offset, input())
				}
				if !strings.Contains(ansi.Strip(term.Render()), "history-013") {
					t.Fatalf("older content missing: %s", term.Render())
				}
				ev.Button = tea.MouseButtonWheelDown
				m.handleMouse(ev)
				if term.Viewport().Offset != 0 || input() != "" {
					t.Fatalf("down offset=%d input=%q", term.Viewport().Offset, input())
				}
			}
		})
	}
}

func TestWheelEdgesAndCoordinates(t *testing.T) {
	m, term, input := scrollModel(t, 60, 6)
	// Border, divider, negative, and outside coordinates must not reach a pane
	// or change rail navigation. Includes reporting-enabled children.
	feedPane(t, term, historyRows(20)+"\x1b[?1000h\x1b[?1006h")
	for _, pos := range [][2]int{{-1, 2}, {1, 0}, {1, 7}, {26, 2}, {27, 0}, {87, 2}, {30, 7}} {
		ev := paneWheel(tea.MouseButtonWheelUp)
		ev.X, ev.Y = pos[0], pos[1]
		m.handleMouse(ev)
		if term.Viewport().Offset != 0 || input() != "" || m.cursor != 0 || m.sectionCursor != "" {
			t.Fatalf("out-of-pane event %v routed", pos)
		}
	}
	ev := paneWheel(tea.MouseButtonWheelDown)
	ev.X = 1
	m.handleMouse(ev)
	if m.cursor == 0 && m.sectionCursor == "" {
		t.Fatal("rail wheel did not move selection")
	}
	if input() != "" {
		t.Fatal("rail wheel reached child")
	}
	feedPane(t, term, "\x1b[?1000l")
	for i := 0; i < 30; i++ {
		m.handleMouse(paneWheel(tea.MouseButtonWheelUp))
	}
	if v := term.Viewport(); v.Offset != v.HistoryLen {
		t.Fatalf("top not clamped: %+v", v)
	}
	for i := 0; i < 30; i++ {
		m.handleMouse(paneWheel(tea.MouseButtonWheelDown))
	}
	if term.Viewport().Offset != 0 || input() != "" {
		t.Fatalf("edge leaked input %q", input())
	}
	feedPane(t, term, "\x1bc\x1b[?1049h")
	m.handleMouse(paneWheel(tea.MouseButtonWheelUp))
	m.handleMouse(paneWheel(tea.MouseButtonWheelDown))
	if input() != "" {
		t.Fatalf("empty alternate history emitted keys: %q", input())
	}
}

func TestHistoryKeysAndInput(t *testing.T) {
	m, term, input := scrollModel(t, 60, 6)
	feedPane(t, term, historyRows(20))
	m.handleKey(tea.KeyMsg{Type: tea.KeyPgUp, Alt: true})
	if term.Viewport().Offset != 5 || input() != "" {
		t.Fatalf("page up offset=%d input=%q", term.Viewport().Offset, input())
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyPgDown, Alt: true})
	if term.Viewport().Offset != 0 {
		t.Fatal("page down did not return live")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyPgUp, Alt: true})
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnd, Alt: true})
	if term.Viewport().Offset != 0 || input() != "" {
		t.Fatal("live chord sent input or stayed scrolled")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyPgUp, Alt: true})
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("draft")})
	if term.Viewport().Offset != 0 || input() != "draft" {
		t.Fatalf("child input offset=%d bytes=%q", term.Viewport().Offset, input())
	}
	// The actual persisted keymap, not a hard-coded handler, controls navigation.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := keymap.Set(keymap.ScrollUp, "ctrl+g"); err != nil {
		t.Fatal(err)
	}
	keys, err := keymap.Load()
	if err != nil {
		t.Fatal(err)
	}
	m.keys = keys
	m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlG})
	if term.Viewport().Offset != 5 || input() != "draft" {
		t.Fatal("rebound scroll key not handled locally")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyPgUp, Alt: true})
	if term.Viewport().Offset != 0 || input() != "draft\x1b[5;3~" {
		t.Fatalf("vacated chord lost its modifier: offset=%d input=%q", term.Viewport().Offset, input())
	}
}

func TestHistoryIndicatorHeaderWidths(t *testing.T) {
	for _, width := range []int{1, 3, 8, 30, 80} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m, term, _ := scrollModel(t, width, 6)
			feedPane(t, term, historyRows(20))
			term.ScrollLines(3)
			before := term.Render()
			header := ansi.Strip(m.renderTopBorder())
			if ansi.StringWidth(header) != m.w || !strings.Contains(header, "↑") {
				t.Fatalf("width %d header %q", width, header)
			}
			if term.Render() != before {
				t.Fatal("header changed pane cells")
			}
			term.ReturnLive()
			if strings.Contains(ansi.Strip(m.renderTopBorder()), "↑") {
				t.Fatal("indicator remains live")
			}
		})
	}
}

func TestPaneClickModifierContract(t *testing.T) {
	for _, tc := range []struct {
		name                string
		tracked, shift, alt bool
		want                string
	}{
		{name: "untracked local"}, {name: "untracked alt offers passthrough", alt: true},
		{name: "reporting", tracked: true, want: "\x1b[<0;3;3M"},
		{name: "reporting alt", tracked: true, alt: true, want: "\x1b[<8;3;3M"},
		{name: "shift local", tracked: true, shift: true},
		{name: "shift wins over alt", tracked: true, shift: true, alt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, term, input := scrollModel(t, 60, 6)
			data := historyRows(20)
			if tc.tracked {
				data += "\x1b[?1000h\x1b[?1006h"
			}
			feedPane(t, term, data)
			term.ScrollLines(3)
			ev := paneWheel(tea.MouseButtonLeft)
			ev.Shift = tc.shift
			ev.Alt = tc.alt
			m.handleMouse(ev)
			if tc.want != "" {
				waitPane(t, "click forwarded", func() bool { return input() == tc.want })
				if term.Viewport().Offset != 0 {
					t.Fatal("child click did not return live")
				}
			} else if input() != "" || term.Viewport().Offset != 3 {
				t.Fatalf("local click changed viewport/input: %d %q", term.Viewport().Offset, input())
			}
		})
	}
}

func TestHalfPageKeyScopeAndSelection(t *testing.T) {
	for _, focused := range []focus{focusAgent, focusSidebar} {
		m, term, input := scrollModel(t, 40, 7)
		feedPane(t, term, historyRows(20))
		m.handleMouse(selectionEvent(tea.MouseActionPress, 0, 0))
		m.handleMouse(selectionEvent(tea.MouseActionMotion, 2, 0))
		m.focus = focused
		m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlU})
		wantOffset := 0
		if focused == focusAgent {
			wantOffset = 3
		}
		if term.Viewport().Offset != wantOffset || input() != "" || m.selection != nil || m.focus != focused {
			t.Fatal("half up scope/invalidation")
		}
		m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD})
		if term.Viewport().Offset != 0 || input() != "" {
			t.Fatal("half down scope")
		}
		m.confirm = &confirmState{message: "confirm"}
		m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlU})
		if term.Viewport().Offset != 0 || input() != "" {
			t.Fatal("modal leaked history shortcut")
		}
	}
}

func TestHalfPageRebindingInRail(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for action, chord := range map[string]string{keymap.ScrollHalfUp: "f5", keymap.ScrollHalfDown: "f6"} {
		if _, err := keymap.Set(action, chord); err != nil {
			t.Fatal(err)
		}
	}
	m := railModel(40, 23)
	var err error
	m.keys, err = keymap.Load()
	if err != nil {
		t.Fatal(err)
	}
	m.cursor = 20
	m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlU})
	m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD})
	if m.cursor != 20 {
		t.Fatal("vacated Ctrl keys still move rail")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyF5})
	if m.cursor != 10 {
		t.Fatal("rebound half-up did not page rail")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyF6})
	if m.cursor != 20 {
		t.Fatal("rebound half-down did not page rail")
	}
}
