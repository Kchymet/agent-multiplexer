package nativetui

import (
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"

	"amux/internal/core"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Session enrichments on the rail: a PR chip on each member row and on the
// workgroup header ("#79 ✓ approved", "#245 ✗ CI"), a PR list in the main
// pane (p), and a key to open a PR in the browser (o, or Enter in the list).

// maxHeaderChips bounds the PR chips on a workgroup header.
const maxHeaderChips = 2

// prPanelState is the PR list shown in the main pane.
type prPanelState struct {
	open   bool
	cursor int
}

// openURL opens a URL in the user's browser; tests replace it.
var openURL = func(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	cmd := exec.Command(name, url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// toneStyle colors an enrichment by its tone. Attention uses the danger color,
// never the attention chip: a PR that needs work is the coordinator's job.
func toneStyle(tone string) lipgloss.Style {
	switch tone {
	case hp.ToneAttention:
		return errStyle
	case hp.TonePending:
		return stateStyle(core.StateRunning)
	case hp.ToneOK:
		return stateStyle(core.StateReady)
	}
	return dimStyle
}

func toneRank(tone string) int {
	switch tone {
	case hp.ToneAttention:
		return 0
	case hp.TonePending:
		return 1
	case hp.ToneOK:
		return 2
	case hp.ToneDone:
		return 3
	}
	return 4
}

// enrichmentsOf is a row's own enrichment list.
func enrichmentsOf(s *core.Session) []hp.Enrichment {
	if s == nil || s.Enrichments == nil {
		return nil
	}
	return *s.Enrichments
}

// isOpenPR reports an open (or draft) pull request enrichment.
func isOpenPR(e hp.Enrichment) bool {
	return e.Kind == hp.EnrichmentPullRequest && e.PullRequest != nil &&
		(e.PullRequest.State == hp.PRStateOpen || e.PullRequest.State == hp.PRStateDraft)
}

// memberEnrichment is one enrichment with the member row that owns it.
type memberEnrichment struct {
	owner *core.Session
	e     hp.Enrichment
}

// prEntries lists the PRs a row stands for: a member's own, or for a
// workgroup header every active member's, each group in its row order.
func (m *model) prEntries(s *core.Session) []memberEnrichment {
	if s == nil {
		return nil
	}
	var out []memberEnrichment
	if !s.IsRoot {
		for _, e := range enrichmentsOf(s) {
			out = append(out, memberEnrichment{owner: s, e: e})
		}
		return out
	}
	for i := range m.sessions {
		c := &m.sessions[i]
		if c.RootID != s.ID || archivedSession(*c) {
			continue
		}
		for _, e := range enrichmentsOf(c) {
			out = append(out, memberEnrichment{owner: c, e: e})
		}
	}
	return out
}

// headerChips are a workgroup header's chips: its members' open PRs, the most
// demanding first, at most maxHeaderChips then "+N".
func (m *model) headerChips(s *core.Session) []segment {
	var open []hp.Enrichment
	for _, me := range m.prEntries(s) {
		if isOpenPR(me.e) {
			open = append(open, me.e)
		}
	}
	sort.SliceStable(open, func(i, j int) bool { return toneRank(open[i].Tone) < toneRank(open[j].Tone) })
	var segs []segment
	for i, e := range open {
		if i == maxHeaderChips {
			segs = append(segs, segment{fmt.Sprintf(" +%d", len(open)-maxHeaderChips), dimStyle})
			break
		}
		segs = append(segs, segment{" " + e.Label, toneStyle(e.Tone)})
	}
	return segs
}

// memberChips are a member row's chips: its open PRs, else its most recent
// merged/closed one, plus "PR data stale" when any is stale.
func memberChips(s *core.Session) []segment {
	var segs []segment
	stale := false
	es := enrichmentsOf(s)
	for _, e := range es {
		if isOpenPR(e) {
			segs = append(segs, segment{" " + e.Label, toneStyle(e.Tone)})
			stale = stale || e.Stale
		}
	}
	if len(segs) == 0 && len(es) > 0 {
		segs = append(segs, segment{" " + es[0].Label, toneStyle(es[0].Tone)})
	}
	if stale {
		segs = append(segs, segment{" · PR data stale", dimStyle})
	}
	return segs
}

// stripPRSuffix removes the plain-text " · PR …" suffix from a member's
// status when its chips show the same PRs.
func stripPRSuffix(status string) string {
	i := strings.Index(status, " · PR #")
	if i < 0 {
		return status
	}
	rest := status[i+len(" · PR #"):]
	if j := strings.Index(rest, " · "); j >= 0 {
		return status[:i] + rest[j:]
	}
	return status[:i]
}

// prTierStyle is the style of a calm rollup label (waiting_review,
// waiting_merge, pr_attention), and whether subState is one.
func prTierStyle(subState string) (lipgloss.Style, bool) {
	switch subState {
	case hp.SubStateWaitingReview, hp.SubStateWaitingMerge:
		return stateStyle(core.StateReady), true
	case hp.SubStatePRAttention:
		return stateStyle(core.StateRunning), true
	}
	return dimStyle, false
}

// sessionStatusSegments styles a row's status line: the rollup label of a
// calm workgroup (never the attention chip, though "waiting for merge" starts
// with "waiting"), else the state word (statusSegments), with the row's PR
// chips after the lead.
func (m *model) sessionStatusSegments(s core.Session) []segment {
	status := s.Status
	var chips []segment
	if s.IsRoot {
		chips = m.headerChips(&s)
	} else if s.Enrichments != nil {
		status = stripPRSuffix(status)
		chips = memberChips(&s)
	}
	var segs []segment
	if style, ok := prTierStyle(s.SubState); ok && s.Rollup != nil && s.Rollup.Label != "" && strings.HasPrefix(status, s.Rollup.Label) {
		segs = []segment{{s.Rollup.Label, style}}
		if rest := status[len(s.Rollup.Label):]; rest != "" {
			segs = append(segs, segment{rest, dimStyle})
		}
	} else {
		segs = statusSegments(status)
	}
	if len(chips) == 0 {
		return segs
	}
	out := append([]segment{}, segs[0])
	out = append(out, chips...)
	return append(out, segs[1:]...)
}

// togglePRs opens or closes the PR list for the selected row.
func (m *model) togglePRs() {
	if m.prPanel.open {
		m.prPanel.open = false
		return
	}
	if len(m.prEntries(m.selected())) == 0 {
		m.status = "no pull requests for this row"
		return
	}
	m.prPanel = prPanelState{open: true}
}

// openSelectedPR opens the selected row's most relevant PR in the browser.
func (m *model) openSelectedPR() {
	entries := m.prEntries(m.selected())
	if len(entries) == 0 {
		m.status = "no pull request to open"
		return
	}
	best := entries[0]
	for _, me := range entries[1:] {
		if isOpenPR(me.e) && (!isOpenPR(best.e) || toneRank(me.e.Tone) < toneRank(best.e.Tone)) {
			best = me
		}
	}
	m.openPR(best.e)
}

func (m *model) openPR(e hp.Enrichment) {
	if !strings.HasPrefix(e.URL, "https://") {
		m.status = "no link for " + e.Label
		return
	}
	if err := openURL(e.URL); err != nil {
		m.status = "open " + e.URL + ": " + err.Error()
		return
	}
	m.status = "opened " + e.URL
}

// handlePRKey drives the open PR list: it captures every key.
func (m *model) handlePRKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	entries := m.prEntries(m.selected())
	p := &m.prPanel
	switch k.String() {
	case "p", "esc", "q", "ctrl+c":
		p.open = false
	case "up", "k":
		p.cursor--
	case "down", "j":
		p.cursor++
	case "enter", "o":
		if p.cursor >= 0 && p.cursor < len(entries) {
			m.openPR(entries[p.cursor].e)
		}
	}
	p.cursor = max(0, min(p.cursor, len(entries)-1))
	if len(entries) == 0 {
		p.open = false
	}
	return m, nil
}

// renderPRs draws the PR list in the main pane.
func (m *model) renderPRs() string {
	w, h := m.mainWidth(), m.paneRows()
	return lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(strings.Join(m.prLines(w), "\n"))
}

func (m *model) prLines(w int) []string {
	s := m.selected()
	entries := m.prEntries(s)
	title := " Pull requests"
	if s != nil {
		title += " · " + s.Title
	}
	lines := []string{cellPad(titleStyle.Render(truncate(title, w/2))+dimStyle.Render(" · ")+keyStyle.Render("Enter")+
		dimStyle.Render(" open · ")+keyStyle.Render("p")+dimStyle.Render(" close"), w)}
	if s != nil && s.Rollup != nil {
		line := "  " + s.Rollup.Label
		if s.Rollup.Stale {
			line += " · PR data stale (" + strings.ReplaceAll(s.Rollup.StaleReason, "_", " ") + ")"
		}
		lines = append(lines, dimStyle.Render(truncate(line, w)))
	}
	lines = append(lines, "")
	var owner *core.Session
	for i, me := range entries {
		if s != nil && s.IsRoot && me.owner != owner {
			owner = me.owner
			lines = append(lines, dimStyle.Render(truncate("  "+owner.Title, w)))
		}
		mark := "  "
		if i == m.prPanel.cursor {
			mark = focusStyle.Render(glyphCursor) + " "
		}
		head := mark + toneStyle(me.e.Tone).Render(me.e.Label) + " " + truncate(me.e.Title, max(0, w-len(me.e.Label)-4))
		lines = append(lines, truncate(head, w))
		lines = append(lines, dimStyle.Render(truncate("    "+prDetail(me.e), w)))
	}
	return lines
}

// prDetail is one PR's second line: repo, state, CI, review, blockers.
func prDetail(e hp.Enrichment) string {
	pr := e.PullRequest
	if pr == nil {
		return e.URL
	}
	parts := []string{fmt.Sprintf("%s#%d", pr.Repo, pr.Number), pr.State}
	ci := "CI " + pr.CI.Status
	if pr.CI.FailingCheck != "" {
		ci += " (" + pr.CI.FailingCheck + ")"
	}
	parts = append(parts, ci)
	rv := "review " + strings.ReplaceAll(pr.Review.State, "_", " ")
	if len(pr.Review.By) > 0 {
		rv += " by " + strings.Join(pr.Review.By, ", ")
	}
	parts = append(parts, rv)
	if pr.Mergeable.Conflict {
		parts = append(parts, "conflict")
	}
	if pr.Mergeable.Behind {
		parts = append(parts, "behind")
	}
	if e.Stale {
		parts = append(parts, "stale")
	}
	return strings.Join(parts, " · ")
}
