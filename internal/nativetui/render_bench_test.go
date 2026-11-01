package nativetui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/vterm"
)

// renderPopulation is the rail of the live install: 214 workgroups with two
// members each, plus the capped archived section, and one attached agent pane
// full of styled output.
func renderPopulation(b *testing.B) *model {
	b.Helper()
	var sessions []core.Session
	for i := 0; i < 214; i++ {
		root := fmt.Sprintf("wg%03d", i)
		sessions = append(sessions, core.Session{ID: root, Title: "workgroup " + root, IsRoot: true, Section: core.SectionWorkgroups, State: core.StateRunning, Status: "running · 2 agents"})
		for j := 0; j < 2; j++ {
			sessions = append(sessions, core.Session{ID: fmt.Sprintf("%s-a%d", root, j), Title: "agent", RootID: root, Section: core.SectionWorkgroups, State: core.StateReady})
		}
	}
	for i := 0; i < 8; i++ {
		sessions = append(sessions, core.Session{ID: fmt.Sprintf("old%d", i), Title: "archived", Section: core.SectionArchived, Archived: true})
	}
	m := &model{sessions: sessions, w: 200, h: 60, terms: map[paneKey]*vterm.Terminal{}, byPane: map[string]paneKey{}}
	m.refreshSessions(sessions)
	t := vterm.NewRemote(m.mainWidth(), m.paneRows(), func([]byte) {}, func(int, int) {})
	b.Cleanup(func() { _ = t.Close() })
	var out strings.Builder
	for row := 0; row < 80; row++ {
		fmt.Fprintf(&out, "\x1b[3%dmline %d \x1b[1mbold\x1b[0m %s\r\n", row%8, row, strings.Repeat("x", 120))
	}
	t.Feed([]byte(out.String()))
	m.terms[paneKey{"wg000-a0", tabAgent}] = t
	m.attached, m.tab, m.focus = "wg000-a0", tabAgent, focusAgent
	return m
}

// BenchmarkView is one full frame: Bubble Tea calls View after every message,
// including each streamed pane-output frame and each daemon snapshot.
func BenchmarkView(b *testing.B) {
	m := renderPopulation(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.View()
	}
}

// paneOutput is one streamed chunk of the attached agent's output.
func paneOutput(id string) tea.Msg {
	return frameMsg{daemon.Frame{Pane: &core.PaneFrame{Type: core.FramePaneOutput, PaneID: paneIDOf(id, tabAgent), Data: []byte("x")}}}
}

// BenchmarkUpdateViewPaneStream is the steady state of an open TUI: messages
// are mostly streamed pane output (a hidden agent's spinner, the shown
// agent's redraws), each followed by View, as Bubble Tea does.
func BenchmarkUpdateViewPaneStream(b *testing.B) {
	for _, cached := range []bool{false, true} {
		b.Run(map[bool]string{false: "uncached", true: "cached"}[cached], func(b *testing.B) {
			m := renderPopulation(b)
			m.frames.enabled = cached
			m.byPane[paneIDOf("wg000-a0", tabAgent)] = paneKey{"wg000-a0", tabAgent}
			_ = m.View()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Nine data chunks to every visible content change.
				if i%10 == 9 {
					m.frames.shownData.Store(true)
					m.Update(termDataMsg{})
				} else {
					m.Update(paneOutput("wg000-a0"))
				}
				_ = m.View()
			}
		})
	}
}
