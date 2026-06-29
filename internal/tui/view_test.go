package tui

import (
	"strings"
	"testing"

	"amux/internal/core"
)

func TestViewRailSectionOrder(t *testing.T) {
	m := &model{
		width: core.RailWidth,
		sessions: []core.Session{
			{ID: "console", Title: "amux console", Mode: "console", State: core.StateReady, Status: "ready · configure amux"},
			{ID: "ab12cd", Title: "my-app", Section: core.SectionWorkgroups, IsRoot: true, State: core.StateRunning, Status: "running · 1 agent"},
			{ID: "ef34gh", Title: "fix-bug", Section: core.SectionWorkgroups, RootID: "ab12cd", State: core.StateRunning, Status: "running · main"},
			{ID: "agent-multiplexer", Title: "Kchymet/agent-multiplexer", Section: core.SectionRepos, Kind: "repo"},
			{ID: "5d87c7eb", Title: "some-proj", Section: core.SectionDetached, Mode: "external", State: core.StateRunning, Status: "running · untracked"},
		},
	}
	out := m.viewRail()

	// Sections must appear top-to-bottom: console, WORKGROUPS, REPOS, DETACHED.
	markers := []string{"amux console", "WORKGROUPS", "my-app", "fix-bug", "REPOS", "Kchymet/agent-multiplexer", "DETACHED", "some-proj"}
	prev := -1
	for _, mk := range markers {
		i := strings.Index(out, mk)
		if i < 0 {
			t.Fatalf("rail missing %q\n%s", mk, out)
		}
		if i < prev {
			t.Fatalf("rail out of order at %q\n%s", mk, out)
		}
		prev = i
	}

	// A repo row is a single line: no status sub-line follows it.
	if strings.Contains(out, "agent-multiplexer") && strings.Contains(out, "· main\n   Kchymet") {
		t.Error("repo row should not render a status sub-line")
	}
}

func TestHighlightFollowsFocusedWindow(t *testing.T) {
	m := &model{
		windowID: "@7",
		sessions: []core.Session{
			{ID: "console", Title: "amux console"},
			{ID: "a1", Title: "alpha", Section: core.SectionWorkgroups, WindowID: "@3"},
			{ID: "a2", Title: "beta", Section: core.SectionWorkgroups, WindowID: "@7"},
		},
	}

	// Idle (not scrolling): highlight the agent whose window is this rail's own.
	if got := m.highlight(); got != 2 {
		t.Fatalf("idle highlight = %d, want 2 (the focused window)", got)
	}
	if s := m.selected(); s == nil || s.ID != "a2" {
		t.Fatalf("idle selected = %v, want a2", s)
	}

	// Scrolling the switcher: highlight follows the cursor instead, seeded at the
	// focused row so movement continues from there.
	m.beginScroll()
	if m.cursor != 2 {
		t.Fatalf("beginScroll cursor = %d, want 2 (seeded at focus)", m.cursor)
	}
	m.cursor = 0
	if got := m.highlight(); got != 0 {
		t.Fatalf("scrolling highlight = %d, want 0 (the cursor)", got)
	}

	// Leaving the switcher restores focus-following.
	m.scrolling = false
	if got := m.highlight(); got != 2 {
		t.Fatalf("post-scroll highlight = %d, want 2 (back to focus)", got)
	}
}

// When the rail isn't tied to a window (e.g. the full dashboard), the highlight
// falls back to the cursor — preserving the original behavior.
func TestHighlightFallsBackToCursor(t *testing.T) {
	m := &model{
		cursor: 1,
		sessions: []core.Session{
			{ID: "a1", Title: "alpha", WindowID: "@3"},
			{ID: "a2", Title: "beta", WindowID: "@7"},
		},
	}
	if got := m.highlight(); got != 1 {
		t.Fatalf("no-window highlight = %d, want 1 (the cursor)", got)
	}
}
