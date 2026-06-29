package nativetui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/vt"
)

func TestKeyToBytesCtrl(t *testing.T) {
	cases := []struct {
		name string
		key  tea.KeyMsg
		want []byte
	}{
		{"ctrl+o", tea.KeyMsg{Type: tea.KeyCtrlO}, []byte{0x0f}},
		{"ctrl+t", tea.KeyMsg{Type: tea.KeyCtrlT}, []byte{0x14}},
		{"ctrl+c", tea.KeyMsg{Type: tea.KeyCtrlC}, []byte{0x03}},
		{"ctrl+alt+o", tea.KeyMsg{Type: tea.KeyCtrlO, Alt: true}, []byte{0x1b, 0x0f}},
		{"enter", tea.KeyMsg{Type: tea.KeyEnter}, []byte("\r")},
		{"rune", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}, []byte("a")},
		{"alt+rune", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("b"), Alt: true}, []byte{0x1b, 'b'}},
		{"up", tea.KeyMsg{Type: tea.KeyUp}, []byte("\x1b[A")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := keyToBytes(c.key); string(got) != string(c.want) {
				t.Fatalf("keyToBytes(%s) = %v, want %v", c.name, got, c.want)
			}
		})
	}
}

func TestMouseToVT(t *testing.T) {
	wheel := mouseToVT(tea.MouseMsg{Button: tea.MouseButtonWheelUp, X: 30, Y: 5}, 3, 4)
	w, ok := wheel.(vt.MouseWheel)
	if !ok {
		t.Fatalf("wheel event mapped to %T, want vt.MouseWheel", wheel)
	}
	if w.X != 3 || w.Y != 4 {
		t.Fatalf("wheel coords = (%d,%d), want (3,4)", w.X, w.Y)
	}
	if w.Button != vt.MouseWheelUp {
		t.Fatalf("wheel button = %v, want MouseWheelUp", w.Button)
	}

	click := mouseToVT(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}, 1, 1)
	if c, ok := click.(vt.MouseClick); !ok || c.Button != vt.MouseLeft {
		t.Fatalf("press mapped to %T (%v), want vt.MouseClick left", click, click)
	}

	rel := mouseToVT(tea.MouseMsg{Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease}, 1, 1)
	if _, ok := rel.(vt.MouseRelease); !ok {
		t.Fatalf("release mapped to %T, want vt.MouseRelease", rel)
	}
}
