package nativetui

import (
	"errors"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/daemon"
)

// bootModel is a model as Run builds it, before any daemon message.
func bootModel() *model {
	return &model{w: 100, h: 24, load: loadConnecting, status: "connecting…", dataCh: make(chan struct{}, 1)}
}

func snapshotMsg(sessions ...core.Session) frameMsg {
	return frameMsg{daemon.Frame{Snapshot: &core.Snapshot{Sessions: sessions}}}
}

func assertContains(t *testing.T, where, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("%s missing %q:\n%s", where, w, out)
		}
	}
}

func assertNotContains(t *testing.T, where, out string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(out, w) {
			t.Errorf("%s should not show %q:\n%s", where, w, out)
		}
	}
}

// At boot, before the daemon answers, the rail and main pane say so instead of
// claiming there are no workgroups or repos.
func TestBootShowsConnecting(t *testing.T) {
	m := bootModel()
	assertContains(t, "rail", plain(m.renderSidebar()), "Connecting to daemon…")
	assertNotContains(t, "rail", plain(m.renderSidebar()), "no workgroups", "no repos")
	assertContains(t, "main", plain(m.renderMain()), "Connecting to daemon…")
	assertNotContains(t, "main", plain(m.renderMain()), "select an agent")
}

// Connected but still waiting on the first snapshot is "loading sessions".
func TestConnectedAwaitsFirstSnapshot(t *testing.T) {
	m := bootModel()
	m.Update(connectedMsg{&reconnectClient{}})
	if m.load != loadWaiting {
		t.Fatalf("load = %v, want loadWaiting", m.load)
	}
	assertContains(t, "rail", plain(m.renderSidebar()), "Loading sessions…")
	assertNotContains(t, "rail", plain(m.renderSidebar()), "no workgroups", "no repos")
	assertContains(t, "main", plain(m.renderMain()), "Loading sessions…")
}

// A genuinely empty snapshot renders the normal empty state, distinct from
// loading.
func TestEmptySnapshotShowsEmptyState(t *testing.T) {
	m := bootModel()
	m.Update(connectedMsg{&reconnectClient{}})
	m.Update(snapshotMsg())
	if m.load != loadReady {
		t.Fatalf("load = %v, want loadReady", m.load)
	}
	rail := plain(m.renderSidebar())
	assertContains(t, "rail", rail, "no workgroups", "no repos")
	assertNotContains(t, "rail", rail, "Loading sessions…", "Connecting")
	assertContains(t, "main", plain(m.renderMain()), "select an agent")
}

// A populated snapshot renders its rows.
func TestPopulatedSnapshotShowsSessions(t *testing.T) {
	m := bootModel()
	m.Update(connectedMsg{&reconnectClient{}})
	m.Update(snapshotMsg(core.Session{ID: "wg1", Title: "payments", IsRoot: true, Section: core.SectionWorkgroups}))
	rail := plain(m.renderSidebar())
	assertContains(t, "rail", rail, "payments", "no repos")
	assertNotContains(t, "rail", rail, "Loading sessions…", "no workgroups")
}

// An unreachable daemon before any snapshot shows the failure and its error
// rather than loading forever.
func TestUnreachableDaemonShowsError(t *testing.T) {
	m := bootModel()
	m.Update(disconnectedMsg{errors.New("dial unix amux.sock: connect: no such file or directory")})
	if m.load != loadOffline {
		t.Fatalf("load = %v, want loadOffline", m.load)
	}
	assertContains(t, "rail", plain(m.renderSidebar()), "Daemon unreachable", "dial unix")
	assertNotContains(t, "rail", plain(m.renderSidebar()), "no workgroups")
	assertContains(t, "main", plain(m.renderMain()), "Daemon unreachable — retrying…", "no such file or directory")

	// The retry connects: back to loading, error cleared.
	m.Update(connectedMsg{&reconnectClient{}})
	if m.load != loadWaiting || m.loadErr != "" {
		t.Fatalf("after reconnect: load = %v, err = %q", m.load, m.loadErr)
	}
}

// Losing the daemon after a snapshot keeps the last inventory on the rail and
// says it is reconnecting; the next snapshot returns to normal.
func TestReconnectKeepsStaleRail(t *testing.T) {
	m := bootModel()
	m.Update(connectedMsg{&reconnectClient{}})
	wg := core.Session{ID: "wg1", Title: "payments", IsRoot: true, Section: core.SectionWorkgroups}
	m.Update(snapshotMsg(wg))
	m.Update(disconnectedMsg{errors.New("EOF")})
	if m.load != loadStale {
		t.Fatalf("load = %v, want loadStale", m.load)
	}
	assertContains(t, "rail", plain(m.renderSidebar()), "payments")
	assertContains(t, "main", plain(m.renderMain()), "Daemon offline — reconnecting…")

	m.Update(connectedMsg{&reconnectClient{}})
	if m.load != loadStale {
		t.Fatalf("connected without a snapshot: load = %v, want loadStale", m.load)
	}
	m.Update(snapshotMsg(wg))
	if m.load != loadReady {
		t.Fatalf("load = %v, want loadReady", m.load)
	}
	assertContains(t, "main", plain(m.renderMain()), "select an agent")
}
