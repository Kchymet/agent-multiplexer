package nativetui

import (
	"strings"
	"testing"
)

// The agent pane draws the child's text cursor only while it has keyboard
// focus. Bubble Tea hides the real terminal cursor, so without this an agent
// in vim INSERT mode (bar cursor via DECSCUSR) or Codex's composer shows no
// cursor at all.
func TestFocusedAgentPaneDrawsCursor(t *testing.T) {
	m, term, _ := scrollModel(t, 12, 4)
	feedPane(t, term, "\x1b[6 q> hi") // INSERT mode: bar cursor after the text
	const reverse = "\x1b[7m"
	if got := m.renderMain(); !strings.Contains(got, reverse) {
		t.Fatalf("focused pane shows no cursor: %q", got)
	}
	m.focus = focusSidebar
	if got := m.renderMain(); strings.Contains(got, reverse) {
		t.Fatalf("unfocused pane drew a cursor: %q", got)
	}
}
