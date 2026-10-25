package nativetui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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
			_, cmd = m.handleMouse(tea.MouseMsg{X: 2, Y: 1 + m.paneRows() - len(m.accountOverviewLines()) - len(m.railHints()), Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
		} else {
			_, cmd = m.handleKey(key("s"))
		}
		if cmd == nil || m.accounts == nil {
			t.Fatal("Accounts activation did not open the account UI")
		}
		m.Update(cmd())
		view := plain(m.renderAccounts())
		if m.accounts.selected != "claude:central" || !strings.Contains(view, "New workloads: preserved legacy default") {
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
				if screen != "accounts" && (!strings.Contains(out, "Accounts") || !strings.Contains(out, "/ search archived")) {
					t.Fatal("pinned navigation not visible")
				}
				t.Logf("%s rendered at %dx%d:\n%s", screen, m.w, m.h, out)
			}
		})
	}
}

// Previously an observation older than the TTL, or any failed status refresh,
// hid the last-known budget behind "stale"/"budget unknown".
func TestAccountRailKeepsLastKnownBudgetWithAge(t *testing.T) {
	now := time.Now()
	observed := now.Add(-12 * time.Minute)
	in := hp.AccountSnapshot{Profiles: []hp.AccountProfile{
		{ID: "claude:central", Harness: "claude", Source: "central", State: "ready", DeliveryEpoch: 2, Identity: hp.AccountIdentity{State: "unknown", Evidence: "opaque-token"},
			Limits: hp.AccountLimits{State: "known", Source: "claude-usage", ProfileID: "claude:central", DeliveryEpoch: 2, ObservedAt: observed.UnixMilli(), RefreshError: "claude_usage_unavailable",
				Windows: []hp.AccountLimitWindow{{Name: "five_hour", UsedPercent: 58, WindowMinutes: 300}, {Name: "seven_day", UsedPercent: 20, WindowMinutes: 10080}}}},
		{ID: "codex:central", Harness: "codex", Source: "central", State: "ready", DeliveryEpoch: 1, Identity: hp.AccountIdentity{State: "known", AccountID: "a"},
			Limits: hp.AccountLimits{State: "known", Source: "codex-native", ProfileID: "codex:central", DeliveryEpoch: 1, ObservedAt: now.UnixMilli(),
				Windows: []hp.AccountLimitWindow{{Name: "codex:primary", UsedPercent: 10, WindowMinutes: 300}}}},
	}}
	snap := projectAccounts(in)
	claude := snap.profiles[0]
	if claude.limits[0].confidence != "provider report" || claude.limits[0].usedPercent == nil {
		t.Fatalf("claude usage report not attributed: %+v", claude.limits[0])
	}
	if got := accountRailBudget(claude, now, false); got != "5h 42% · 7d 80% · stale" {
		t.Fatalf("aged claude budget: %q", got)
	}
	detail := strings.Join(accountLimitLines(claude.limits[0], now, false), "\n")
	for _, want := range []string{"stale", "Used: 58%", "12m ago", "Last refresh failed: claude_usage_unavailable"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail missing %q: %s", want, detail)
		}
	}
	if got := ansi.Strip(accountUsageSummary(claude.limits[0], now, false)); !strings.Contains(got, "42% left") || !strings.Contains(got, "stale 12m ago") {
		t.Fatalf("summary: %q", got)
	}
	m := groupedRail()
	m.accountOverview = &snap
	rail := plain(m.renderSidebar())
	for _, want := range []string{"Claude · 12m ago", "5h 42% · 7d 80% · stale", "5h 90%"} {
		if !strings.Contains(rail, want) {
			t.Fatalf("rail missing %q:\n%s", want, rail)
		}
	}
	if strings.Contains(rail, "5h 90% · stale") {
		t.Fatalf("fresh codex budget marked stale:\n%s", rail)
	}
	// A failed status refresh keeps the last snapshot, now marked stale.
	m.accountOverviewStale = true
	rail = plain(m.renderSidebar())
	if !strings.Contains(rail, "5h 90% · stale") || !strings.Contains(rail, "5h 42% · 7d 80% · stale") {
		t.Fatalf("failed refresh hid last-known budget:\n%s", rail)
	}
}

// Reset counts render on the rail and in the popup only when the provider
// reports one, and a vendor-requested pause explains why a budget is stale.
func TestAccountResetsAndRetryRendering(t *testing.T) {
	now := time.Now()
	two, zero := int64(2), int64(0)
	in := hp.AccountSnapshot{Profiles: []hp.AccountProfile{
		{ID: "claude:central", Harness: "claude", Source: "central", State: "ready", DeliveryEpoch: 1, Identity: hp.AccountIdentity{State: "unknown", Evidence: "opaque-token"},
			Limits: hp.AccountLimits{State: "known", Source: "claude-usage", ProfileID: "claude:central", DeliveryEpoch: 1, ObservedAt: now.Add(-20 * time.Minute).UnixMilli(),
				RefreshError: "claude_usage_rate_limited", RetryAt: now.Add(37*time.Minute + 30*time.Second).UnixMilli(), ResetsAvailable: &zero,
				Windows: []hp.AccountLimitWindow{{Name: "five_hour", UsedPercent: 58, WindowMinutes: 300}, {Name: "seven_day", UsedPercent: 20, WindowMinutes: 10080}}}},
		{ID: "codex:central", Harness: "codex", Source: "central", State: "ready", DeliveryEpoch: 1, Identity: hp.AccountIdentity{State: "known", AccountID: "a"},
			Limits: hp.AccountLimits{State: "known", Source: "codex-native", ProfileID: "codex:central", DeliveryEpoch: 1, ObservedAt: now.UnixMilli(), ResetsAvailable: &two,
				Windows: []hp.AccountLimitWindow{{Name: "codex:primary", UsedPercent: 10, WindowMinutes: 300}}}},
	}}
	snap := projectAccounts(in)
	claude, codex := snap.profiles[0], snap.profiles[1]
	if got := accountRailBudget(claude, now, false) + " | " + accountRailDetail(claude, now); got != "5h 42% · 7d 80% · stale | 0 resets · retry in 38m" {
		t.Fatalf("claude: %q", got)
	}
	if got := accountRailBudget(codex, now, false) + " | " + accountRailDetail(codex, now); got != "5h 90% | 2 resets" {
		t.Fatalf("codex: %q", got)
	}
	m := groupedRail()
	m.accountOverview = &snap
	rail := plain(m.renderSidebar())
	for _, want := range []string{"0 resets · retry in 38m", "2 resets"} {
		if !strings.Contains(rail, want) {
			t.Fatalf("rail missing %q:\n%s", want, rail)
		}
	}
	detail := strings.Join(accountLimitLines(claude.limits[0], now, false), "\n")
	if !strings.Contains(detail, "Last refresh failed: Claude usage report rate limited (retry in 38m)") {
		t.Fatalf("detail: %s", detail)
	}
	// Absent counts and expired pauses render nothing.
	in.Profiles[0].Limits.ResetsAvailable, in.Profiles[0].Limits.RetryAt = nil, now.Add(-time.Minute).UnixMilli()
	snap = projectAccounts(in)
	if got := accountRailDetail(snap.profiles[0], now); got != "" {
		t.Fatalf("absent detail rendered: %q", got)
	}
	m.accountOverview = &snap
	if rail = plain(m.renderSidebar()); strings.Contains(rail, "resets ·") || strings.Contains(rail, "retry in") {
		t.Fatalf("absent detail rendered:\n%s", rail)
	}
}

// Under a preserved host default the rail shows the host login's budget,
// which the daemon now refreshes, instead of the unused central profile.
func TestAccountLegacyDefaultShowsHostBudget(t *testing.T) {
	now := time.Now()
	one := int64(1)
	snap := projectAccounts(hp.AccountSnapshot{Defaults: map[string]string{"claude": "claude:host", "codex": "codex:host"}, Profiles: []hp.AccountProfile{
		{ID: "claude:central", Harness: "claude", Source: "central", State: "unconfigured"},
		{ID: "claude:host", Harness: "claude", Source: "host", State: "ready", DeliveryEpoch: 1, Identity: hp.AccountIdentity{State: "unknown", Evidence: "external"},
			Limits: hp.AccountLimits{State: "known", Source: "claude-usage", ProfileID: "claude:host", DeliveryEpoch: 1, ObservedAt: now.UnixMilli(),
				Windows: []hp.AccountLimitWindow{{Name: "five_hour", UsedPercent: 25, WindowMinutes: 300}}}},
		{ID: "codex:central", Harness: "codex", Source: "central", State: "unconfigured"},
		{ID: "codex:host", Harness: "codex", Source: "host", State: "ready", DeliveryEpoch: 1, Identity: hp.AccountIdentity{State: "unknown", Evidence: "external"},
			Limits: hp.AccountLimits{State: "known", Source: "codex-native", ProfileID: "codex:host", DeliveryEpoch: 1, ObservedAt: now.UnixMilli(), ResetsAvailable: &one,
				Windows: []hp.AccountLimitWindow{{Name: "codex:primary", UsedPercent: 60, WindowMinutes: 300}}}},
	}})
	if got := accountRailBudget(snap.profiles[0], now, false); got != "5h 75%" {
		t.Fatalf("claude host budget: %q", got)
	}
	if got := accountRailBudget(snap.profiles[1], now, false) + " | " + accountRailDetail(snap.profiles[1], now); got != "5h 40% | 1 reset" {
		t.Fatalf("codex host budget: %q", got)
	}
	if snap.profiles[0].id != "claude:central" || !strings.Contains(snap.profiles[0].limits[0].reason, "legacy default claude:host") {
		t.Fatalf("row identity or provenance: %+v", snap.profiles[0])
	}
}

// A quota probe runs every 15 minutes, so it stays current until the next
// one is due instead of turning stale after the default 5 minutes.
func TestAccountProbeFreshUntil(t *testing.T) {
	now := time.Now()
	observed := now.Add(-12 * time.Minute)
	limits := hp.AccountLimits{State: "known", Source: "claude-probe", ProfileID: "claude:central", DeliveryEpoch: 1, ObservedAt: observed.UnixMilli(),
		FreshUntil: observed.Add(17 * time.Minute).UnixMilli(),
		Windows:    []hp.AccountLimitWindow{{Name: "five_hour", UsedPercent: 1, WindowMinutes: 300}, {Name: "seven_day", UsedPercent: 14, WindowMinutes: 10080}}}
	profile := func(l hp.AccountLimits) accountProfile {
		return projectAccounts(hp.AccountSnapshot{Profiles: []hp.AccountProfile{{ID: "claude:central", Harness: "claude", Source: "central", State: "ready", DeliveryEpoch: 1,
			Identity: hp.AccountIdentity{State: "unknown", Evidence: "opaque-token"}, Limits: l}}}).profiles[0]
	}
	p := profile(limits)
	if got := accountRailBudget(p, now, false); got != "5h 99% · 7d 86%" {
		t.Fatalf("probe within its lifetime: %q", got)
	}
	if p.limits[0].confidence != "provider report" {
		t.Fatalf("probe not attributed: %+v", p.limits[0])
	}
	if got := accountRailBudget(p, now.Add(6*time.Minute), false); got != "5h 99% · 7d 86% · stale" {
		t.Fatalf("overdue probe: %q", got)
	}
	limits.FreshUntil = observed.Add(48 * time.Hour).UnixMilli()
	if got := accountRailBudget(profile(limits), now.Add(time.Hour), false); !strings.HasSuffix(got, " · stale") {
		t.Fatalf("unbounded lifetime: %q", got)
	}
}
