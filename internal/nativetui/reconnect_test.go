package nativetui

import (
	"io"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"amux/internal/core"
	"amux/internal/daemon"
)

type reconnectClient struct {
	mu      sync.Mutex
	actions []core.Action
	closed  bool
}

func (c *reconnectClient) Next() (daemon.Frame, error) { return daemon.Frame{}, io.EOF }
func (c *reconnectClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}
func (c *reconnectClient) Send(a core.Action) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return io.ErrClosedPipe
	}
	c.actions = append(c.actions, a)
	return nil
}
func (c *reconnectClient) PaneOpen(p, id string, tab, cols, rows int) error {
	return c.Send(core.Action{Action: core.ActionPaneOpen, PaneID: p, ID: id, Tab: tab, Cols: cols, Rows: rows})
}
func (c *reconnectClient) PaneInput(p string, b []byte) error {
	return c.Send(core.Action{Action: core.ActionPaneInput, PaneID: p, Data: append([]byte(nil), b...)})
}
func (c *reconnectClient) PaneResize(p string, cols, rows int) error {
	return c.Send(core.Action{Action: core.ActionPaneResize, PaneID: p, Cols: cols, Rows: rows})
}
func (c *reconnectClient) recorded() []core.Action {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]core.Action(nil), c.actions...)
}

// Run only commands that open panes. The read-loop command belongs to Bubble
// Tea's event pump and must remain pending while we inject the fresh snapshot.
func runReconnectCommands(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if batch, ok := cmd().(tea.BatchMsg); ok {
		for _, child := range batch {
			runReconnectCommands(child)
		}
	}
}

func TestReconnectReattachesOpenTabsAndRoutesInputToNewClient(t *testing.T) {
	old, next := &reconnectClient{}, &reconnectClient{}
	m := &model{client: old, w: 100, h: 30, dataCh: make(chan struct{}, 1)}
	defer func() {
		for _, term := range m.terms {
			term.Close()
		}
	}()
	keys := []paneKey{{"agent-a", tabAgent}, {"agent-a", tabTerminal}, {"agent-b", tabAgent}}
	for _, k := range keys {
		runReconnectCommands(m.launchPane(k.id, k.tab))
	}
	m.attached, m.tab, m.focus = "agent-a", tabTerminal, focusAgent
	// A real reconnect sees a new authenticated connection, then its inventory.
	m.Update(disconnectedMsg{})
	m.Update(connectedMsg{next})
	_, cmd := m.Update(frameMsg{daemon.Frame{Snapshot: &core.Snapshot{Sessions: []core.Session{
		{ID: "agent-a", RootID: "group"}, {ID: "agent-b", RootID: "group"},
	}}}})
	runReconnectCommands(cmd)
	opens := map[string]bool{}
	for _, a := range next.recorded() {
		if a.Action == core.ActionPaneOpen {
			opens[a.PaneID] = true
		}
	}
	for _, k := range keys {
		if !opens[paneIDOf(k.id, k.tab)] {
			t.Fatalf("reconnect did not reopen %v on the new connection: %v", k, next.recorded())
		}
	}
	if m.attached != "agent-a" || m.tab != tabTerminal || m.focus != focusAgent {
		t.Fatalf("reconnect moved the user's active tab/focus")
	}
	oldCount := len(old.recorded())
	for _, k := range keys {
		m.attached, m.tab = k.id, k.tab
		m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
		m.cur().Resize(90, 25)
	}
	inputs, resizes := map[string]bool{}, map[string]bool{}
	for _, a := range next.recorded() {
		if a.Action == core.ActionPaneInput && string(a.Data) == "x" {
			inputs[a.PaneID] = true
		}
		if a.Action == core.ActionPaneResize && a.Cols == 90 && a.Rows == 25 {
			resizes[a.PaneID] = true
		}
	}
	for _, k := range keys {
		p := paneIDOf(k.id, k.tab)
		if !inputs[p] || !resizes[p] {
			t.Errorf("pane %v did not route input/resize to the new connection", k)
		}
	}
	if len(old.recorded()) != oldCount {
		t.Fatal("reconnected panes still wrote to the old client")
	}
	if !old.closed {
		t.Fatal("disconnected client was not closed")
	}
}

func TestReconnectDoesNotReopenDeletedSessionsOrExitedTabs(t *testing.T) {
	m := &model{client: &reconnectClient{}, w: 100, h: 30, dataCh: make(chan struct{}, 1)}
	defer func() {
		for _, term := range m.terms {
			term.Close()
		}
	}()
	for _, id := range []string{"deleted", "exited", "live"} {
		runReconnectCommands(m.launchPane(id, tabAgent))
	}
	m.terms[paneKey{"exited", tabAgent}].MarkClosed()
	m.attached, m.tab, m.focus = "live", tabAgent, focusSidebar
	m.Update(disconnectedMsg{})
	next := &reconnectClient{}
	m.Update(connectedMsg{next})
	_, cmd := m.Update(frameMsg{daemon.Frame{Snapshot: &core.Snapshot{Sessions: []core.Session{
		{ID: "exited", RootID: "group"}, {ID: "live", RootID: "group"},
	}}}})
	runReconnectCommands(cmd)
	opens := 0
	for _, a := range next.recorded() {
		if a.Action == core.ActionPaneOpen {
			opens++
			if a.ID != "live" {
				t.Errorf("reopened unavailable pane: %v", a)
			}
		}
	}
	if opens != 1 {
		t.Fatalf("opened %d panes, want live only", opens)
	}
	if m.focus != focusSidebar {
		t.Fatal("reconnect stole sidebar focus")
	}
}
