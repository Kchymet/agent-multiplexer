package nativetui

import (
	"encoding/base64"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"amux/internal/clipboard"
	"amux/internal/core"
	"amux/internal/vterm"
	tea "github.com/charmbracelet/bubbletea"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

func selectionEvent(action tea.MouseAction, x, y int) tea.MouseMsg {
	return tea.MouseMsg{Action: action, Button: tea.MouseButtonLeft, X: sidebarWidth + 1 + x, Y: 1 + y}
}

func finishSelection(m *model, ev tea.MouseMsg) {
	_, cmd := m.handleMouse(ev)
	if cmd != nil {
		m.Update(cmd())
	}
}

func TestPaneSelectionCopiesWholeGlyphsAndHighlightsSameCells(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "forward"
		if reverse {
			name = "reversed"
		}
		t.Run(name, func(t *testing.T) {
			m, term, input := scrollModel(t, 12, 4)
			feedPane(t, term, "\x1b[7mA界éZ\x1b[0m\r\nsecond\r\nthird")
			var copied []string
			m.clip = func(s []byte) { copied = append(copied, string(s)) }
			a, b := selectionEvent(tea.MouseActionPress, 2, 0), selectionEvent(tea.MouseActionMotion, 2, 1)
			if reverse {
				a.Y, b.Y = b.Y, a.Y
			}
			m.handleMouse(a)
			m.handleMouse(b)
			if len(copied) != 0 {
				t.Fatal("copied before release")
			}
			view := m.renderMain()
			if ansi.Strip(view) != ansi.Strip(term.Render()) {
				t.Fatalf("highlight changed text: %q vs %q", view, term.Render())
			}
			// Decode the actual highlighted ANSI output in a second terminal;
			// assert which displayed cells are highlighted, including wide cells
			// and originally reverse-video content. No snapshot helper oracle.
			rendered := vt.NewEmulator(12, 4)
			defer rendered.Close()
			_, _ = rendered.WriteString(strings.ReplaceAll(view, "\n", "\r\n"))
			for _, p := range [][2]int{{1, 0}, {3, 0}, {11, 0}, {0, 1}, {2, 1}} {
				c := rendered.CellAt(p[0], p[1])
				if c.Style.Bg != ansi.IndexedColor(24) || c.Style.Fg != ansi.IndexedColor(231) || c.Style.Attrs&uv.AttrReverse != 0 {
					t.Fatalf("selected cell %v lacks visible highlight: %+v", p, c)
				}
			}
			for _, p := range [][2]int{{0, 0}, {3, 1}, {0, 2}} {
				if rendered.CellAt(p[0], p[1]).Style.Bg == ansi.IndexedColor(24) {
					t.Fatalf("unselected cell %v highlighted", p)
				}
			}
			b.Action = tea.MouseActionRelease
			finishSelection(m, b)
			if len(copied) != 1 || copied[0] != "界éZ\nsec" || input() != "" {
				t.Fatalf("copy=%q input=%q", copied, input())
			}
			if term.Snapshot().Cells[0][1].Style.Attrs&uv.AttrReverse == 0 {
				t.Fatal("highlight mutated source terminal styles")
			}
		})
	}
}

func TestHistoricalSelectionUsesDisplayedRows(t *testing.T) {
	m, term, input := scrollModel(t, 20, 4)
	feedPane(t, term, historyRows(12))
	term.ScrollToTop()
	var copied string
	m.clip = func(s []byte) { copied = string(s) }
	m.handleMouse(selectionEvent(tea.MouseActionPress, 0, 0))
	finishSelection(m, selectionEvent(tea.MouseActionRelease, 10, 1))
	if copied != "history-001\nhistory-002" || input() != "" || term.Viewport().Offset == 0 {
		t.Fatalf("historical copy=%q input=%q viewport=%+v", copied, input(), term.Viewport())
	}
}

func TestAlternateFrameHistorySelectionUsesDisplayedRows(t *testing.T) {
	m, term, input := scrollModel(t, 20, 4)
	frame := func(text string) string { return "\x1b[?2026h\x1b[H\x1b[2J" + text + "\x1b[?2026l" }
	feedPane(t, term, "\x1b[?1049h"+frame("old 界 row\r\nsecond old")+frame("new frame"))
	term.ScrollToTop()
	if !strings.Contains(ansi.Strip(m.renderMain()), "old 界 row") {
		t.Fatal("alternate history is not displayed")
	}
	var copied string
	m.clip = func(s []byte) { copied = string(s) }
	m.handleMouse(selectionEvent(tea.MouseActionPress, 4, 0))
	finishSelection(m, selectionEvent(tea.MouseActionRelease, 5, 1))
	if copied != "界 row\nsecond" || input() != "" {
		t.Fatalf("alternate history copy=%q input=%q", copied, input())
	}
}

func TestSelectionReleaseInRailStaysInPane(t *testing.T) {
	m, term, input := scrollModel(t, 12, 4)
	feedPane(t, term, "first\r\nsecond")
	var copied string
	m.clip = func(s []byte) { copied = string(s) }
	m.handleMouse(selectionEvent(tea.MouseActionPress, 2, 0))
	ev := selectionEvent(tea.MouseActionMotion, -100, 1)
	ev.Alt = true // ownership stays local after modifier change
	m.handleMouse(ev)
	ev.Action = tea.MouseActionRelease
	finishSelection(m, ev)
	if copied != "rst\ns" || m.cursor != 0 || m.accounts != nil || m.archiveSearch != nil || input() != "" {
		t.Fatalf("rail release copy=%q cursor=%d input=%q", copied, m.cursor, input())
	}
	if m.gesture != nil {
		t.Fatal("release did not finish capture")
	}
}

func TestSelectionMousePriorityAndGestureLatch(t *testing.T) {
	for _, tc := range []struct {
		name                string
		tracked, shift, alt bool
		local, child        bool
	}{
		{name: "untracked local", local: true},
		{name: "tracked app", tracked: true, child: true},
		{name: "alt app", tracked: true, alt: true, child: true},
		{name: "alt untracked passthrough", alt: true},
		{name: "shift local", tracked: true, shift: true, local: true},
		{name: "shift wins alt", tracked: true, shift: true, alt: true, local: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, term, input := scrollModel(t, 12, 4)
			data := "abcdef"
			if tc.tracked {
				data += "\x1b[?1002h\x1b[?1006h"
			}
			feedPane(t, term, data)
			copies := 0
			m.clip = func([]byte) { copies++ }
			press := selectionEvent(tea.MouseActionPress, 0, 0)
			press.Shift, press.Alt = tc.shift, tc.alt
			m.handleMouse(press)
			motion := selectionEvent(tea.MouseActionMotion, 2, 0)
			// Deliberately invert the modifier that would change ownership if
			// routing were reconsidered per event.
			motion.Shift, motion.Alt = !tc.shift, !tc.alt
			m.handleMouse(motion)
			release := motion
			release.Action = tea.MouseActionRelease
			finishSelection(m, release)
			if tc.child {
				pressCode := "0"
				motionCode, releaseCode := "44", "12" // Shift+Alt
				if tc.alt {
					pressCode, motionCode, releaseCode = "8", "36", "4"
				}
				want := "\x1b[<" + pressCode + ";1;1M\x1b[<" + motionCode + ";3;1M\x1b[<" + releaseCode + ";3;1m"
				waitPane(t, "complete app gesture with changed modifiers", func() bool { return input() == want })
			} else if input() != "" {
				t.Fatalf("local/unreported input leaked: %q", input())
			}
			if (copies == 1) != tc.local {
				t.Fatalf("copies=%d want local=%v", copies, tc.local)
			}
		})
	}
}

func TestSelectionInvalidationConsumesGestureTail(t *testing.T) {
	for _, reason := range []string{"parsed output", "queued output", "reset", "input", "resize", "tab round trip", "pane switch", "modal", "viewport", "closed", "disconnect"} {
		t.Run(reason, func(t *testing.T) {
			m, term, _ := scrollModel(t, 20, 4)
			feedPane(t, term, historyRows(10))
			copies := 0
			m.clip = func([]byte) { copies++ }
			m.handleMouse(selectionEvent(tea.MouseActionPress, 0, 0))
			m.handleMouse(selectionEvent(tea.MouseActionMotion, 4, 1))
			switch reason {
			case "parsed output":
				feedPane(t, term, "\x1b[Hchanged") // no termDataMsg delivered
			case "queued output", "reset":
				m.byPane = map[string]paneKey{"pane": {m.attached, m.tab}}
				kind := core.FramePaneOutput
				if reason == "reset" {
					kind = core.FramePaneReset
				}
				m.handlePaneFrame(&core.PaneFrame{PaneID: "pane", Type: kind, Data: []byte("new")})
			case "input":
				m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
			case "resize":
				m.Update(tea.WindowSizeMsg{Width: 70, Height: 12})
			case "tab round trip":
				m.terms[paneKey{m.attached, tabTerminal}] = term
				m.switchTab(tabTerminal)
				m.switchTab(tabAgent)
			case "pane switch":
				m.attached = "other"
			case "modal":
				m.confirm = &confirmState{message: "confirm"}
			case "viewport":
				term.ScrollLines(1)
			case "closed":
				term.MarkClosed()
			case "disconnect":
				m.Update(disconnectedMsg{})
			}
			// The tail now lands over the rail. It must be swallowed even after
			// invalidation, not start a new selection or activate a modal row.
			m.handleMouse(selectionEvent(tea.MouseActionMotion, -30, 1))
			finishSelection(m, selectionEvent(tea.MouseActionRelease, -30, 1))
			if m.selection != nil || m.gesture != nil || copies != 0 || m.cursor != 0 {
				t.Fatalf("stale selection survived %s: selected=%v gesture=%v copies=%d", reason, m.selection != nil, m.gesture != nil, copies)
			}
		})
	}
}

func TestOtherPaneOutputDoesNotCancelSelection(t *testing.T) {
	m, term, _ := scrollModel(t, 12, 4)
	feedPane(t, term, "abcdef")
	other := vterm.NewRemote(12, 4, nil, nil)
	defer other.Close()
	m.terms[paneKey{"other", tabAgent}] = other
	m.byPane = map[string]paneKey{"other": {"other", tabAgent}}
	var copy string
	m.clip = func(s []byte) { copy = string(s) }
	m.handleMouse(selectionEvent(tea.MouseActionPress, 0, 0))
	m.handlePaneFrame(&core.PaneFrame{PaneID: "other", Type: core.FramePaneOutput, Data: []byte("busy")})
	finishSelection(m, selectionEvent(tea.MouseActionRelease, 2, 0))
	if copy != "abc" {
		t.Fatalf("other pane cancelled selection: %q", copy)
	}
}

func TestSelectionClipboardThroughRealOutputWrapper(t *testing.T) {
	m, term, _ := scrollModel(t, 12, 4)
	f, err := os.CreateTemp(t.TempDir(), "terminal-output")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m.clip = isolatedClipboard(t, &clipboard.SerialFile{File: f})
	feedPane(t, term, "abcdef\x1b]52;c;c2VjcmV0\a\x1b]52;c;?\a")
	_ = m.View()
	// Without an app-copy callback, output/rendering and ordinary clicks do
	// not trigger local-selection copy. App-copy coexistence has PTY coverage.
	m.handleMouse(selectionEvent(tea.MouseActionPress, 1, 0))
	finishSelection(m, selectionEvent(tea.MouseActionRelease, 1, 0))
	if info, _ := f.Stat(); info.Size() != 0 {
		t.Fatal("clipboard exposed without a drag")
	}
	m.handleMouse(selectionEvent(tea.MouseActionPress, 0, 0))
	finishSelection(m, selectionEvent(tea.MouseActionRelease, 2, 0))
	// Duplicate releases cannot copy again.
	finishSelection(m, selectionEvent(tea.MouseActionRelease, 2, 0))
	waitPane(t, "queued selection delivery", func() bool { info, _ := f.Stat(); return info.Size() > 0 })
	_, _ = f.Seek(0, io.SeekStart)
	b, err := io.ReadAll(f)
	want := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte("abc")) + "\a"
	if err != nil || string(b) != want {
		t.Fatalf("clipboard output=%q, %v; want %q", b, err, want)
	}
}

func TestPaneMouseRejectsTinyWindowOutsideCoordinates(t *testing.T) {
	m, term, input := scrollModel(t, 3, 24)
	m.h = 4
	feedPane(t, term, "\x1b[?1000h\x1b[?1006h")
	// paneRows historically falls back to 24 for this height. That fallback
	// cannot authorize mouse input below the actual one-row pane.
	for _, y := range []int{2, 3, 10} {
		ev := paneWheel(tea.MouseButtonWheelUp)
		ev.Y = y
		m.handleMouse(ev)
	}
	time.Sleep(20 * time.Millisecond) // allow the async mouse pipe to drain
	if input() != "" {
		t.Fatalf("tiny-window outside coordinates reached child: %q", input())
	}
}

func TestAppGestureReleaseRemainsOwnedAfterPaneSwitch(t *testing.T) {
	m, term, input := scrollModel(t, 12, 4)
	feedPane(t, term, "\x1b[?1002h\x1b[?1006h")
	_, other, otherInput := scrollModel(t, 12, 4)
	feedPane(t, other, "\x1b[?1002h\x1b[?1006h")
	m.terms[paneKey{m.attached, tabTerminal}] = other
	m.handleMouse(selectionEvent(tea.MouseActionPress, 2, 1))
	m.switchTab(tabTerminal)
	ev := selectionEvent(tea.MouseActionRelease, -100, 99)
	ev.Shift = true
	finishSelection(m, ev)
	want := "\x1b[<0;3;2M\x1b[<4;1;4m"
	waitPane(t, "release to original app after tab switch", func() bool { return input() == want })
	if otherInput() != "" {
		t.Fatalf("original gesture leaked to new pane: %q", otherInput())
	}
}

func TestAppGesturePreservesWheelAndMultipleButtons(t *testing.T) {
	m, term, input := scrollModel(t, 12, 4)
	feedPane(t, term, "\x1b[?1002h\x1b[?1006h")
	m.handleMouse(selectionEvent(tea.MouseActionPress, 0, 0))
	wheel := selectionEvent(tea.MouseActionPress, 0, 0)
	wheel.Button = tea.MouseButtonWheelUp
	m.handleMouse(wheel)
	right := selectionEvent(tea.MouseActionPress, 0, 0)
	right.Button = tea.MouseButtonRight
	m.handleMouse(right)
	m.handleMouse(selectionEvent(tea.MouseActionRelease, 0, 0))
	if m.gesture == nil {
		t.Fatal("capture ended while right button remained down")
	}
	right.Action = tea.MouseActionRelease
	m.handleMouse(right)
	want := "\x1b[<0;1;1M\x1b[<64;1;1M\x1b[<2;1;1M\x1b[<0;1;1m\x1b[<2;1;1m"
	waitPane(t, "app wheel and both releases", func() bool { return input() == want })
	if m.gesture != nil {
		t.Fatal("capture survived all releases")
	}
}

func TestShiftWheelRemainsLocalDuringAppGesture(t *testing.T) {
	m, term, input := scrollModel(t, 12, 4)
	feedPane(t, term, historyRows(12)+"\x1b[?1002h\x1b[?1006h")
	m.handleMouse(selectionEvent(tea.MouseActionPress, 0, 0))
	wheel := paneWheel(tea.MouseButtonWheelUp)
	wheel.Shift = true
	m.handleMouse(wheel)
	if term.Viewport().Offset != 3 {
		t.Fatal("Shift wheel lost local scrolling during app gesture")
	}
	// Resize before the outside release; it must use the new child's bounds.
	m.Update(tea.WindowSizeMsg{Width: sidebarWidth + 1 + 5, Height: 3 + 2})
	release := selectionEvent(tea.MouseActionRelease, 99, 99)
	release.Shift = true
	m.handleMouse(release)
	want := "\x1b[<0;1;1M\x1b[<4;5;2m"
	waitPane(t, "resized app release without local wheel bytes", func() bool { return input() == want })
}

// Exercise the real Copier without invoking the developer's host clipboard.
func isolatedClipboard(t *testing.T, out io.Writer) func([]byte) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	for _, key := range []string{"TMUX", "DISPLAY", "WAYLAND_DISPLAY", "WSL_DISTRO_NAME", "WSL_INTEROP"} {
		t.Setenv(key, "")
	}
	return clipboard.New(out).Copy
}
