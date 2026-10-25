package nativetui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"amux/internal/core"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestArchivedSearchSeparateFromRail(t *testing.T) {
	m := groupedRail()
	spy := &reconnectClient{}
	m.client = spy
	m.sessions = append(m.sessions, core.Session{ID: "other", Title: "Old Unicode α", Archived: true, Section: core.SectionRepos})
	original := m.sessions[5]
	for _, entry := range m.railEntries() {
		if entry.section == core.SectionArchived || (entry.section == "" && archivedSession(m.sessions[entry.index])) {
			t.Fatal("archive remains navigable in the rail")
		}
	}
	rail := plain(m.renderSidebar())
	if strings.Contains(rail, "ARCHIVED") || strings.Contains(rail, original.Title) || !strings.Contains(rail, "/ search archived") {
		t.Fatalf("archive search is not a separate discoverable path:\n%s", rail)
	}
	_, cmd := m.handleKey(key("/"))
	if cmd != nil || m.archiveSearch == nil || m.attached != "" || m.confirm != nil {
		t.Fatal("opening search launched or restored a workload")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("archived-child")})
	if rows := m.archivedMatches(); len(rows) != 1 || rows[0].ID != original.ID {
		t.Fatalf("archive search lost the full snapshot: %+v", rows)
	}
	if m.attached != "" || m.sessions[5] != original {
		t.Fatal("typing mutated the archived workload")
	}
	cursor, section := m.cursor, m.sectionCursor
	_, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	// Execute any returned command against the actual daemonClient dispatch
	// spy. With no existing mirror, the former implementation sent PaneOpen.
	runReconnectCommands(cmd)
	if actions := spy.recorded(); len(actions) != 0 {
		t.Fatalf("archive details dispatched runtime/lifecycle actions: %+v", actions)
	}
	if m.attached != "" || len(m.terms) != 0 || m.confirm != nil || m.cursor != cursor || m.sectionCursor != section || m.sessions[5] != original {
		t.Fatal("archive details changed runtime, archive state, or rail selection")
	}
	if m.archiveSearch == nil || m.archiveSearch.details != original.ID || !strings.Contains(plain(m.renderArchiveSearch()), "Restore before reopening") {
		t.Fatal("fresh archived result did not open read-only details")
	}
	_, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	runReconnectCommands(cmd)
	if len(spy.recorded()) != 0 {
		t.Fatal("Enter in details dispatched a runtime action")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}) // back to results
	if m.archiveSearch == nil || m.archiveSearch.query != "archived-child" {
		t.Fatal("details lost the search filter")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}) // close search
	m.handleKey(key("/"))
	m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("other")})
	_, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if cmd != nil || m.confirm == nil || m.confirm.action.ID != "other" || m.confirm.action.Action != core.ActionArchive {
		t.Fatal("restore must require an explicit confirmation for the chosen ID")
	}
	m.sessions[len(m.sessions)-1].Archived = false
	_, cmd = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil || m.confirm != nil || !strings.Contains(m.status, "Session changed") {
		t.Fatal("restore confirmation archived a concurrently restored session")
	}
}

func TestAccountsBarKeyboardMouseAndLegacyProjection(t *testing.T) {
	snapshot := projectAccounts(hp.AccountSnapshot{Defaults: map[string]string{"claude": "claude:host", "codex": "codex:central"}, Profiles: []hp.AccountProfile{
		{ID: "claude:central", Harness: "claude", Source: "central", State: "ready"},
		{ID: "claude:host", Harness: "claude", Source: "host", State: "ready"},
		{ID: "claude:shared", Harness: "claude", Source: "shared"},
		{ID: "claude:second", Harness: "claude", Source: "central"},
		{ID: "codex:central", Harness: "codex", Source: "central", State: "unconfigured"},
		{ID: "codex:environment", Harness: "codex", Source: "environment"},
	}})
	if len(snapshot.profiles) != 2 {
		t.Fatalf("expected one configuration per harness: %+v", snapshot.profiles)
	}
	for _, mouse := range []bool{false, true} {
		m := groupedRail()
		m.accountOverview = &snapshot
		bar := plain(strings.Join(m.accountOverviewLines(), "\n"))
		if !strings.Contains(strings.Split(bar, "\n")[0], "s") || !strings.Contains(bar, "Claude: legacy default") {
			t.Fatalf("untruthful account overview: %s", bar)
		}
		m.accountCall = func(_ context.Context, req accountRequest) (accountResponse, error) {
			if req.op != "status" {
				t.Fatalf("opening accounts mutated configuration: %+v", req)
			}
			return accountResponse{snapshot: &snapshot}, nil
		}
		var cmd tea.Cmd
		if mouse {
			_, cmd = m.handleMouse(tea.MouseMsg{X: 2, Y: 1 + m.paneRows() - 6, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
		} else {
			_, cmd = m.handleKey(key("s"))
		}
		if cmd == nil || m.accounts == nil {
			t.Fatal("Accounts activation did not open the account UI")
		}
		m.Update(cmd())
		view := plain(m.renderAccounts())
		if m.accounts.selected != "claude:central" || !strings.Contains(view, "[not default]") || !strings.Contains(view, "Existing default retained") {
			t.Fatalf("legacy default represented as central:\n%s", view)
		}
		for _, forbidden := range []string{"claude:host", "claude:shared", "codex:environment", "↑/↓: profile"} {
			if strings.Contains(view, forbidden) {
				t.Fatalf("legacy menu exposed %q", forbidden)
			}
		}
	}
}

func TestArchivedDetailsRestoreNeverOpensPane(t *testing.T) {
	m := groupedRail()
	spy := &reconnectClient{}
	m.client = spy
	for _, key := range []tea.KeyMsg{key("/"), {Type: tea.KeyEnter}, {Type: tea.KeyCtrlR}} {
		_, cmd := m.handleKey(key)
		runReconnectCommands(cmd)
	}
	if m.confirm == nil || len(spy.recorded()) != 0 {
		t.Fatal("details restore must wait for confirmation")
	}
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	runReconnectCommands(cmd)
	actions := spy.recorded()
	if len(actions) != 1 || actions[0].Action != core.ActionArchive || actions[0].ID != "archived" {
		t.Fatalf("restore dispatched anything other than the reviewed lifecycle action: %+v", actions)
	}
	// The restored row arrives in the active inventory; it must still await a
	// separate user action in the rail before any runtime is opened.
	sessions := append([]core.Session(nil), m.sessions...)
	sessions[5].Section = core.SectionWorkgroups
	sessions[5].Archived = false
	m.refreshSessions(sessions)
	runReconnectCommands(m.tryPendingAttach())
	if m.attached != "" || m.pending != "" || len(m.terms) != 0 || len(spy.recorded()) != 1 {
		t.Fatal("restoring a search result opened or queued a runtime")
	}
}

func TestArchivedDetailsCannotRetargetAfterInventoryChange(t *testing.T) {
	m := groupedRail()
	spy := &reconnectClient{}
	m.client = spy
	m.handleKey(key("/"))
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	sessions := append([]core.Session(nil), m.sessions...)
	sessions[5].ID = "replacement"
	m.refreshSessions(sessions)
	_, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	runReconnectCommands(cmd)
	if m.confirm != nil || len(spy.recorded()) != 0 || !strings.Contains(plain(m.renderArchivedDetails()), "no longer in the archive") {
		t.Fatal("details silently retargeted a changed inventory row")
	}
}

func TestAccountOverviewFencesModalAndReconnectReplies(t *testing.T) {
	m := groupedRail()
	old := accountFixture()
	m.accountCall = func(_ context.Context, req accountRequest) (accountResponse, error) {
		if req.op != "status" {
			t.Fatal("overview made a mutation")
		}
		return accountResponse{snapshot: &old, attempt: &accountAttempt{url: "PRIVATE-URL"}}, nil
	}
	beforeModal := m.refreshAccountOverview()
	m.openAccounts()
	m.Update(beforeModal())
	if m.accountOverview != nil {
		t.Fatal("late overview overwrote modal-era identity")
	}
	m.accounts = nil
	beforeDisconnect := m.refreshAccountOverview()
	m.Update(disconnectedMsg{})
	m.Update(beforeDisconnect())
	if m.accountOverview != nil || !m.accountOverviewStale {
		t.Fatal("late overview overwrote disconnection")
	}
	m.Update(m.refreshAccountOverview()())
	if m.accountOverview == nil || strings.Contains(m.View(), "PRIVATE-URL") {
		t.Fatal("safe overview missing or ceremony copied into rail")
	}
	m.accountCall = func(context.Context, accountRequest) (accountResponse, error) {
		return accountResponse{}, errors.New("PRIVATE-SECRET")
	}
	reply := m.refreshAccountOverview()().(accountOverviewReplyMsg)
	if strings.Contains(reply.err.Error(), "PRIVATE-SECRET") {
		t.Fatal("overview retained raw error")
	}
	m.Update(reply)
	if !strings.Contains(plain(m.renderSidebar()), "stale") {
		t.Fatal("failed overview appeared fresh")
	}
}

func TestRailAccountRenderingEvidence(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {120, 32}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			m := groupedRail()
			m.w, m.h = size[0], size[1]
			snapshot := accountFixture()
			snapshot.legacyDefaults["claude"] = true
			snapshot.profiles[0].defaultProfile = false
			m.accountOverview = &snapshot
			for _, screen := range []string{"rail", "archive search", "archive details", "accounts"} {
				if screen == "archive search" {
					m.archiveSearch = &archiveSearchState{}
				}
				if screen == "archive details" {
					m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
				}
				if screen == "accounts" {
					m.archiveSearch = nil
					m.accounts = &accountsState{snapshot: snapshot, harness: "claude"}
					m.accounts.selectProfile()
				}
				out := plain(m.View())
				if lipgloss.Width(out) != m.w || lipgloss.Height(out) != m.h {
					t.Fatalf("%s exceeds terminal: %dx%d", screen, lipgloss.Width(out), lipgloss.Height(out))
				}
				if !strings.Contains(out, "Accounts") || !strings.Contains(out, "/ search archived") {
					t.Fatal("pinned navigation not visible")
				}
				t.Logf("%s rendered at %dx%d:\n%s", screen, m.w, m.h, out)
			}
		})
	}
}
