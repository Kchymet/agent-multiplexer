package nativetui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"amux/internal/core"
)

func cachedModel(t *testing.T) *model {
	t.Helper()
	m := &model{w: 120, h: 30, sessions: []core.Session{
		{ID: "wg1", Title: "payments", IsRoot: true, Section: core.SectionWorkgroups},
		{ID: "ag1", Title: "idempotency", RootID: "wg1", Section: core.SectionWorkgroups},
	}}
	m.frames.enabled = true
	m.refreshSessions(m.sessions)
	return m
}

// Pane bytes alone never re-render; any other message does.
func TestFrameCacheSkipsDataOnlyFrames(t *testing.T) {
	m := cachedModel(t)
	first := m.View()
	m.sessions[1].Title = "renamed-agent"
	m.Update(paneOutput("ag1"))
	if got := m.View(); got != first {
		t.Fatal("a data-only frame re-rendered")
	}
	m.frames.shownData.Store(false)
	m.Update(termDataMsg{})
	if got := m.View(); got != first {
		t.Fatal("hidden-terminal data re-rendered")
	}
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	if got := m.View(); !strings.Contains(got, "renamed-agent") {
		t.Fatal("a non-data message did not re-render the rail")
	}
}

// Data parsed by the shown terminal re-renders the main pane but reuses the
// rendered rail.
func TestFrameCacheShownDataKeepsRail(t *testing.T) {
	m := cachedModel(t)
	_ = m.View()
	rail := m.frames.rail
	m.frames.shownData.Store(true)
	m.Update(termDataMsg{})
	if m.frames.viewValid || !m.frames.railValid {
		t.Fatalf("shown data: viewValid=%v railValid=%v, want frame invalid and rail kept", m.frames.viewValid, m.frames.railValid)
	}
	_ = m.View()
	if m.frames.rail != rail {
		t.Fatal("rail re-rendered for pane data")
	}
}

// Without Run's opt-in, every View renders from the current state.
func TestFrameCacheDisabledRendersEveryView(t *testing.T) {
	m := cachedModel(t)
	m.frames.enabled = false
	_ = m.View()
	m.sessions[1].Title = "renamed-agent"
	if !strings.Contains(m.View(), "renamed-agent") {
		t.Fatal("uncached View served a stale frame")
	}
}
