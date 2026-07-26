package nativetui

import (
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"amux/internal/keymap"
)

func altKey(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: true}
}

// A zero-value model (no Load) navigates with the built-in Alt defaults, so
// every other test constructing &model{} keeps working.
func TestDefaultChordsRoute(t *testing.T) {
	m := railModel(2, 24)
	m.focus = focusAgent

	m.handleKey(altKey('h'))
	if m.focus != focusSidebar {
		t.Fatalf("alt+h: focus = %v, want sidebar", m.focus)
	}
	if _, cmd := m.handleKey(altKey('q')); cmd == nil {
		t.Fatal("alt+q: want a quit command, got nil")
	}
}

// A rebound chord routes the action and the vacated default goes dead.
func TestReboundChordsRoute(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "amux")
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(dir))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"keys": {"focus-rail": "ctrl+g"}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err := keymap.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	m := railModel(2, 24)
	m.keys = keys
	m.focus = focusAgent

	m.handleKey(altKey('h')) // the old default is unbound now
	if m.focus != focusAgent {
		t.Fatal("alt+h still routed after rebinding focus-rail")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlG})
	if m.focus != focusSidebar {
		t.Fatalf("ctrl+g: focus = %v, want sidebar", m.focus)
	}
}
