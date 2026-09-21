package nativetui

import (
	"amux/internal/core"
	"amux/internal/daemon"
	tea "github.com/charmbracelet/bubbletea"
	"testing"
)

func TestRestartShortcutTargetsFocusedAgentOrSelectedCoordinator(t *testing.T) {
	for _, focused := range []bool{false, true} {
		c := &reconnectClient{}
		m := &model{client: c, sessions: []core.Session{{ID: "coordinator", Title: "goal mode", IsRoot: true, Role: "coordinator"}, {ID: "other", RootID: "group"}}, attached: "other"}
		if focused {
			m.focus = focusAgent
		}
		m.handleKey(altKey('r'))
		want := "coordinator"
		if focused {
			want = "other"
		}
		if m.confirm == nil || m.confirm.action.Action != core.ActionRestart || m.confirm.action.ID != want {
			t.Fatalf("wrong restart target: %+v", m.confirm)
		}
		if len(c.recorded()) != 0 {
			t.Fatal("restart ran before confirmation")
		}
		_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
		cmd()
		if got := c.recorded(); len(got) != 1 || got[0].Action != core.ActionRestart || got[0].ID != want {
			t.Fatalf("sent %+v", got)
		}
	}
}

func TestRestartResultReattachesAgentAndPreservesTerminal(t *testing.T) {
	c := &reconnectClient{}
	m := &model{client: c, w: 100, h: 30, dataCh: make(chan struct{}, 1), sessions: []core.Session{{ID: "a1", RootID: "group"}}}
	defer func() {
		for _, term := range m.terms {
			term.Close()
		}
	}()
	runReconnectCommands(m.launchPane("a1", tabAgent))
	old := m.terms[paneKey{"a1", tabAgent}]
	runReconnectCommands(m.launchPane("a1", tabTerminal))
	shell := m.terms[paneKey{"a1", tabTerminal}]
	_, cmd := m.Update(frameMsg{daemon.Frame{Result: &core.Result{OK: true, RestartedID: "a1"}}})
	runReconnectCommands(cmd)
	if m.attached != "a1" || m.tab != tabAgent || m.focus != focusAgent || m.terms[paneKey{"a1", tabAgent}] == old {
		t.Fatal("did not attach replacement agent")
	}
	if m.terms[paneKey{"a1", tabTerminal}] != shell || shell.Closed() {
		t.Fatal("restart closed terminal")
	}
}

func TestRestartShortcutRejectsArchivedSelection(t *testing.T) {
	m := &model{sessions: []core.Session{{ID: "a1", RootID: "group", Archived: true}}}
	m.handleKey(altKey('r'))
	if m.confirm != nil {
		t.Fatal("offered restart for archived session")
	}
}
