package nativetui

import (
	"strings"
	"testing"

	"amux/internal/core"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

func prEnrichment(n int, status, label, state string) hp.Enrichment {
	return hp.Enrichment{Kind: hp.EnrichmentPullRequest, ID: "o/r#" + label, Title: "PR title " + label,
		URL: "https://github.com/o/r/pull/1", Status: hp.EnrichmentStatus{State: status}, Tone: hp.ToneFor(status), Label: label,
		PullRequest: &hp.PullRequestEnrichment{Repo: "o/r", Number: n, State: state,
			CI: hp.PRCI{Status: hp.CIRed, FailingCheck: "test"}, Review: hp.PRReviewSummary{State: hp.ReviewApproved, By: []string{"octocat"}}}}
}

func prModel() *model {
	ok := hp.Enrichments{prEnrichment(79, hp.StatusAwaitingMerge, "#79 ✓ approved", hp.PRStateOpen)}
	bad := hp.Enrichments{prEnrichment(245, hp.StatusAttention, "#245 ✗ CI", hp.PRStateOpen), prEnrichment(9, hp.StatusDone, "#9 merged", hp.PRStateMerged)}
	return &model{w: 140, h: 40, sessions: []core.Session{
		{ID: "wg1", Title: "wg", IsRoot: true, Role: "coordinator", Section: core.SectionWorkgroups, State: core.StateRunning,
			SubState: hp.SubStatePRAttention, Status: "PR attention · 1 PR · 2 agents · b PR #245 CI failed",
			Rollup: &hp.Rollup{State: hp.RollupPRAttention, Count: 1, Label: "PR attention · 1 PR"}},
		{ID: "m1", Title: "a", RootID: "wg1", Section: core.SectionWorkgroups, State: core.StateReady,
			Status: "ready · PR #79 CI✓ approved · note", Enrichments: &ok},
		{ID: "m2", Title: "b", RootID: "wg1", Section: core.SectionWorkgroups, State: core.StateReady,
			Status: "ready · PR #245 CI✗", Enrichments: &bad},
	}}
}

// Member rows show tone-colored PR chips instead of the plain PR suffix; the
// workgroup header shows its members' open PRs, most demanding first.
func TestPRChipsOnRows(t *testing.T) {
	m := prModel()
	text := func(segs []segment) string {
		var b strings.Builder
		for _, sg := range segs {
			b.WriteString(sg.text)
		}
		return b.String()
	}
	member := text(m.sessionStatusSegments(m.sessions[1]))
	if member != "ready ⑂ #79 ✓ approved · note" {
		t.Fatalf("member status = %q", member)
	}
	if got := ansi.Strip(m.rowStatus(m.sessions[1], " ")); !strings.Contains(got, "#79") {
		t.Fatalf("member sub-line = %q", got)
	}
	header := text(m.sessionStatusSegments(m.sessions[0]))
	if !strings.HasPrefix(header, "PR attention · 1 PR ⑂ #245 ✗ CI ⑂ #79 ✓ approved · 2 agents") {
		t.Fatalf("header status = %q", header)
	}
	segs := m.sessionStatusSegments(m.sessions[0])
	if segs[1].style.GetForeground() != errStyle.GetForeground() || segs[2].style.GetForeground() != stateStyle(core.StateReady).GetForeground() {
		t.Fatalf("chip tones = %+v", segs[1:3])
	}
	if strings.Contains(header, "#9 merged") {
		t.Fatalf("header shows a merged PR: %q", header)
	}
}

// A calm workgroup ("waiting for merge" starts with "waiting") is never drawn
// as the attention chip.
func TestCalmRollupIsNotAttention(t *testing.T) {
	m := &model{}
	s := core.Session{ID: "wg1", Title: "wg", IsRoot: true, Role: "coordinator", State: core.StateReady,
		SubState: hp.SubStateWaitingMerge, Status: "waiting for merge · 2 PRs · 2 agents",
		Rollup: &hp.Rollup{State: hp.RollupWaitingMerge, Label: "waiting for merge · 2 PRs"}}
	segs := m.sessionStatusSegments(s)
	if segs[0].text != "waiting for merge · 2 PRs" || segs[0].style.GetForeground() != stateStyle(core.StateReady).GetForeground() {
		t.Fatalf("segments = %+v", segs)
	}
	for _, sg := range segs {
		if _, plain := sg.style.GetBackground().(lipgloss.NoColor); !plain {
			t.Fatalf("calm rollup drawn as the attention chip: %+v", segs)
		}
	}
	// Without the rollup it would have been: the word "waiting" is the chip.
	if old := statusSegments(s.Status); old[0].style.GetBackground() != attnStyle.GetBackground() {
		t.Fatalf("baseline changed: %+v", old)
	}
}

func TestMemberChipsStale(t *testing.T) {
	e := prEnrichment(1, hp.StatusAwaitingReview, "#1 CI…", hp.PRStateOpen)
	e.Stale = true
	enr := hp.Enrichments{e}
	got := ""
	for _, sg := range memberChips(&core.Session{Enrichments: &enr}) {
		got += sg.text
	}
	if got != " ⑂ #1 CI… · PR data stale" {
		t.Fatalf("chips = %q", got)
	}
}

func TestStripPRSuffix(t *testing.T) {
	for in, want := range map[string]string{
		"ready · PR #1 CI✓, #2 draft · note": "ready · note",
		"ready · PR #1 merged":               "ready",
		"ready · main":                       "ready · main",
	} {
		if got := stripPRSuffix(in); got != want {
			t.Errorf("stripPRSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

// p opens the selected row's PR list in the main pane (a workgroup lists every
// member's PRs), Enter/o opens the PR in the browser, p closes it.
func TestPRPanelAndOpen(t *testing.T) {
	var opened []string
	old := openURL
	openURL = func(u string) error { opened = append(opened, u); return nil }
	defer func() { openURL = old }()

	m := prModel()
	m.cursor = 0
	m.togglePRs()
	if !m.prPanel.open {
		t.Fatal("panel did not open")
	}
	out := ansi.Strip(m.renderPRs())
	for _, want := range []string{"Pull requests · wg", "PR attention · 1 PR", "#79 ✓ approved", "#245 ✗ CI", "#9 merged",
		"o/r#245 · open · CI red (test) · review approved by octocat"} {
		if !strings.Contains(out, want) {
			t.Errorf("panel lacks %q:\n%s", want, out)
		}
	}
	m.handlePRKey(tea.KeyMsg{Type: tea.KeyDown})
	m.handlePRKey(tea.KeyMsg{Type: tea.KeyEnter})
	if len(opened) != 1 || m.prPanel.cursor != 1 {
		t.Fatalf("opened %v cursor %d", opened, m.prPanel.cursor)
	}
	m.handlePRKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if m.prPanel.open {
		t.Fatal("p did not close the panel")
	}

	// o on the rail opens the most demanding open PR; a row without PRs says so.
	opened = nil
	m.cursor = 2
	m.openSelectedPR()
	if len(opened) != 1 || !strings.Contains(m.status, "opened") {
		t.Fatalf("o opened %v (status %q)", opened, m.status)
	}
	m.sessions = append(m.sessions, core.Session{ID: "m3", Title: "c", RootID: "wg1"})
	m.cursor = 3
	m.togglePRs()
	if m.prPanel.open || m.status != "no pull requests for this row" {
		t.Fatalf("empty row panel open=%v status=%q", m.prPanel.open, m.status)
	}
}

// Only https links are opened.
func TestOpenPRRefusesNonHTTPS(t *testing.T) {
	called := false
	old := openURL
	openURL = func(string) error { called = true; return nil }
	defer func() { openURL = old }()
	m := &model{}
	m.openPR(hp.Enrichment{Label: "#1", URL: "file:///etc/passwd"})
	if called {
		t.Fatal("opened a non-https URL")
	}
}

// Any kind renders through the same path: its icon, its label, a color from
// its normalized status; an unknown kind gets the generic icon and an unknown
// state the neutral color. A failed deploy outranks an approved PR.
func TestGenericKindChips(t *testing.T) {
	deploy := hp.Enrichment{Kind: "deploy", ID: "prod", Title: "deploy prod", Label: "prod failed",
		Status: hp.EnrichmentStatus{State: hp.StatusFailed, Detail: "rollout failed"}}
	odd := hp.Enrichment{Kind: "space_probe", ID: "x", Title: "probe x", Label: "x", Status: hp.EnrichmentStatus{State: "orbiting"}}
	pr := prEnrichment(79, hp.StatusAwaitingMerge, "#79 ✓ approved", hp.PRStateOpen)
	if c := chip(deploy); c.text != " ⇪ prod failed" || c.style.GetForeground() != errStyle.GetForeground() {
		t.Fatalf("deploy chip = %+v", c)
	}
	if c := chip(odd); c.text != " • x" || c.style.GetForeground() != dimStyle.GetForeground() {
		t.Fatalf("unknown kind/state chip = %+v", c)
	}
	enr := hp.Enrichments{pr, deploy}
	m := &model{sessions: []core.Session{
		{ID: "wg1", Title: "wg", IsRoot: true, Role: "coordinator", State: core.StateRunning, Status: "running · 1 agent"},
		{ID: "m1", Title: "a", RootID: "wg1", State: core.StateReady, Status: "ready", Enrichments: &enr},
	}}
	segs := m.headerChips(&m.sessions[0])
	if len(segs) != 2 || segs[0].text != " ⇪ prod failed" || segs[1].text != " ⑂ #79 ✓ approved" {
		t.Fatalf("header chips = %+v", segs)
	}
	if got := prDetail(deploy); got != "deploy · failed · rollout failed" {
		t.Fatalf("deploy detail = %q", got)
	}
}
