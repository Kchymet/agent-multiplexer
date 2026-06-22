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
			{ID: "ab12cd", Title: "my-app", Section: core.SectionWorkspaces, IsRoot: true, State: core.StateRunning, Status: "running · 1 agent"},
			{ID: "ef34gh", Title: "fix-bug", Section: core.SectionWorkspaces, RootID: "ab12cd", State: core.StateRunning, Status: "running · main"},
			{ID: "agent-multiplexer", Title: "Kchymet/agent-multiplexer", Section: core.SectionRepos, Kind: "repo"},
			{ID: "5d87c7eb", Title: "some-proj", Section: core.SectionDetached, Mode: "external", State: core.StateRunning, Status: "running · untracked"},
		},
	}
	out := m.viewRail()

	// Sections must appear top-to-bottom: console, WORKSPACES, REPOS, DETACHED.
	markers := []string{"amux console", "WORKSPACES", "my-app", "fix-bug", "REPOS", "Kchymet/agent-multiplexer", "DETACHED", "some-proj"}
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
