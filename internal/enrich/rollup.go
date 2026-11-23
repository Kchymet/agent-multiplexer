package enrich

import (
	"fmt"
	"strings"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Member is one active workgroup member as the rollup sees it.
type Member struct {
	ID    string
	Label string // compact display name for reasons
	State string // its activity (a hp.State* value), as the daemon presents it
	// Enrichments are its rendered enrichments of every kind.
	Enrichments []hp.Enrichment
}

// Input is everything a workgroup's rollup depends on.
type Input struct {
	// OwnState is the coordinator's own turn activity (a hp.State* value).
	OwnState string
	// Goal is the coordinator's goal status in goal mode ("" otherwise).
	Goal     string
	GoalNote string
	Members  []Member
	// Sources is each registered source's freshness. With none, enrichments
	// are not observed: items neither count nor drive a state.
	Sources []SourceStatus
}

// Result is a workgroup's rollup plus its legacy presentation.
type Result struct {
	Rollup hp.Rollup
	// State is Session.State: always in the attention ladder.
	State string
	// SubState mirrors the rollup for the new tiers, or is supervising.
	SubState string
	// Lead is the word(s) that open the row's status line.
	Lead string
	// NeedsYou names who is blocked on the user (Supervision.NeedsYou):
	// CoordinatorLabel for the coordinator itself, then member labels. State
	// is waiting exactly when it is non-empty.
	NeedsYou []string
}

// CoordinatorLabel names the coordinator itself in NeedsYou.
const CoordinatorLabel = "coordinator"

// Goal statuses that need the user.
func goalNeedsUser(goal string) bool {
	return goal == "input" || goal == "blocked" || goal == "budgetLimited"
}

func rank(state string) int {
	switch state {
	case hp.StateWaiting:
		return 4
	case hp.StateRunning:
		return 3
	case hp.StateUnknown:
		return 2
	case hp.StateReady:
		return 1
	}
	return 0
}

// maxReasonItems bounds how many members/items a reason names.
const maxReasonItems = 3

// Item tiers: which rollup state an item's normalized status drives. The
// rollup reads nothing else of an item, so any kind contributes by mapping its
// own states onto the normalized set.
const (
	tierNone      = ""
	tierAttention = "attention" // attention, failed → pr_attention
	tierReview    = "review"    // awaiting_review → waiting_review
	tierMerge     = "merge"     // awaiting_merge → waiting_merge
)

func tierOf(state string) string {
	switch state {
	case hp.StatusAttention, hp.StatusFailed:
		return tierAttention
	case hp.StatusAwaitingReview:
		return tierReview
	case hp.StatusAwaitingMerge:
		return tierMerge
	}
	return tierNone
}

// tierItems collects one tier's items for its label and reason.
type tierItems struct {
	items  []string
	allPRs bool
}

func (t *tierItems) add(kind, item string) {
	if len(t.items) == 0 {
		t.allPRs = true
	}
	t.items = append(t.items, item)
	t.allPRs = t.allPRs && kind == hp.EnrichmentPullRequest
}

// label is "<word> · N PRs" when only PRs contribute, else "<generic> · N".
func (t tierItems) label(prWord, generic string) string {
	n := len(t.items)
	if t.allPRs {
		return prWord + " · " + prs(n)
	}
	return fmt.Sprintf("%s · %d", generic, n)
}

// Rollup derives a workgroup's state, first match wins:
//
//	waiting_user   someone is blocked on the user: the coordinator's own
//	               prompt or question, its goal needing the user while it is
//	               between turns, or a waiting member (NeedsYou names who)
//	running        the coordinator or a member is running (or live, unknown);
//	               a between-turns coordinator then is "supervising"
//	pr_attention   an item needs work (status attention or failed): the
//	               coordinator's job, not the user's
//	waiting_review an item awaits review
//	waiting_merge  an item awaits a merge
//	ready / idle   otherwise
//
// It is a pure function of member states and the items' normalized statuses:
// whatever decides a member's state (e.g. honest waiting detection) feeds
// straight into it, and a new enrichment kind changes it only through the
// statuses its items carry.
func Rollup(in Input) Result {
	own := in.OwnState
	if own == "" {
		own = hp.StateIdle
	}
	between := own == hp.StateReady || own == hp.StateIdle || own == hp.StateUnknown
	goalNeeds := between && goalNeedsUser(in.Goal)
	maxState := own
	var needs, busy, openItems []string
	if own == hp.StateWaiting || goalNeeds {
		needs = append(needs, CoordinatorLabel)
	}
	nRunning := 0
	inflight := false
	observed := len(in.Sources) > 0
	kinds := map[string]bool{}
	for _, s := range in.Sources {
		kinds[s.Kind] = true
	}
	totals := zeroStates()
	other := map[string]map[string]int{}
	var prItems []hp.Enrichment
	tiers := map[string]*tierItems{tierAttention: {}, tierReview: {}, tierMerge: {}}
	staleItem := false
	for _, m := range in.Members {
		if rank(m.State) > rank(maxState) {
			maxState = m.State
		}
		switch m.State {
		case hp.StateWaiting:
			inflight = true
			needs = append(needs, m.Label)
		case hp.StateRunning, hp.StateUnknown:
			inflight = true
			nRunning++
			busy = append(busy, m.Label+" "+m.State)
		}
		if !observed {
			continue
		}
		for _, e := range m.Enrichments {
			if !kinds[e.Kind] {
				continue
			}
			state := normalizeStatus(e.Status.State)
			totals[state]++
			if e.Kind == hp.EnrichmentPullRequest {
				prItems = append(prItems, e)
			} else {
				if other[e.Kind] == nil {
					other[e.Kind] = zeroStates()
				}
				other[e.Kind][state]++
			}
			if state == hp.StatusDone {
				continue
			}
			text := itemText(m.Label, e)
			openItems = append(openItems, text)
			if t := tierOf(state); t != tierNone {
				tiers[t].add(e.Kind, text)
			}
			if e.Stale {
				staleItem = true
			}
		}
	}

	r := Result{}
	ro := hp.Rollup{Totals: totals, Sources: []hp.EnrichmentSource{}}
	if observed {
		if kinds[hp.EnrichmentPullRequest] {
			c := pullRequestCounts(prItems)
			ro.Enrichments.PullRequest = &c
		}
		if len(other) > 0 {
			ro.Enrichments.Other = other
		}
		var newest time.Time
		for _, s := range in.Sources {
			ro.Sources = append(ro.Sources, s.Wire())
			if s.LastOK.After(newest) {
				newest = s.LastOK
			}
			if s.Stale && !ro.Stale {
				ro.Stale, ro.StaleReason = true, s.StaleReason
			}
		}
		ro.ObservedAt = rfc3339(newest)
		if !ro.Stale && staleItem {
			ro.Stale, ro.StaleReason = true, hp.StaleError
			for _, s := range in.Sources {
				if s.LastReason != "" {
					ro.StaleReason = s.LastReason
					break
				}
			}
		}
	}

	var reason []string
	stopped := own == hp.StateIdle
	supervise := func() {
		if !(between && inflight) {
			return
		}
		r.SubState = hp.SubStateSupervising
		if items := append(append([]string(nil), busy...), openItems...); len(items) > 0 {
			reason = append(reason, "supervising "+list(items))
		}
		if stopped {
			reason = append(reason, "coordinator stopped: wake-ups wait until it starts")
		}
	}
	att, rev, mer := tiers[tierAttention], tiers[tierReview], tiers[tierMerge]
	switch {
	case len(needs) > 0:
		ro.State, ro.Count, ro.Label = hp.RollupWaitingUser, len(needs), "waiting on you"
		r.State, r.Lead, r.NeedsYou = hp.StateWaiting, hp.StateWaiting, needs
		reason = append(reason, "needs you: "+list(needs))
		if goalNeeds && strings.TrimSpace(in.GoalNote) != "" {
			reason = append(reason, "goal "+in.Goal+": "+oneLine(in.GoalNote))
		}
		supervise()
	case own == hp.StateRunning || nRunning > 0:
		ro.State, ro.Count = hp.RollupRunning, nRunning
		ro.Label = "running" + agents(nRunning)
		r.State, r.Lead = maxState, hp.StateRunning
		if rank(r.State) < rank(hp.StateRunning) {
			r.State = hp.StateRunning
		}
		supervise()
	case len(att.items) > 0:
		ro.State, ro.Count, ro.Label = hp.RollupPRAttention, len(att.items), att.label("PR attention", "needs attention")
		r.State, r.SubState = hp.StateRunning, hp.SubStatePRAttention
		reason = append(reason, list(att.items))
		if stopped {
			r.State = hp.StateReady
			reason = append(reason, "coordinator stopped")
		}
	case len(rev.items) > 0:
		ro.State, ro.Count, ro.Label = hp.RollupWaitingReview, len(rev.items), rev.label("waiting for review", "waiting for review")
		if m := len(mer.items); m > 0 {
			ro.Label += fmt.Sprintf(" · %d ready to merge", m)
		}
		r.State, r.SubState = hp.StateReady, hp.SubStateWaitingReview
		reason = append(reason, list(append(append([]string(nil), rev.items...), mer.items...)))
	case len(mer.items) > 0:
		ro.State, ro.Count, ro.Label = hp.RollupWaitingMerge, len(mer.items), mer.label("waiting for merge", "waiting for merge")
		r.State, r.SubState = hp.StateReady, hp.SubStateWaitingMerge
		reason = append(reason, list(mer.items))
	default:
		r.State, r.Lead = maxState, own
		ro.State, ro.Label = hp.RollupIdle, "idle"
		if maxState != hp.StateIdle {
			ro.State, ro.Label = hp.RollupReady, "ready"
		}
	}
	if r.Lead == "" {
		r.Lead = ro.Label
	}
	ro.Reason = strings.Join(reason, "; ")
	r.Rollup = ro
	return r
}

func zeroStates() map[string]int {
	m := make(map[string]int, len(hp.EnrichmentStates))
	for _, s := range hp.EnrichmentStates {
		m[s] = 0
	}
	return m
}

// normalizeStatus maps an item state onto the known set (unknown → unknown).
func normalizeStatus(state string) string {
	for _, s := range hp.EnrichmentStates {
		if s == state {
			return s
		}
	}
	return hp.StatusUnknown
}

// itemText is one item in a reason, e.g. "m1 PR #12 CI red: test" or
// "m1 deploy staging failed": the member, the item's reference and its
// status detail.
func itemText(member string, e hp.Enrichment) string {
	ref := e.Kind + " " + e.Title
	if e.Kind == hp.EnrichmentPullRequest {
		if i := strings.LastIndex(e.ID, "#"); i >= 0 {
			ref = "PR " + e.ID[i:]
		}
	}
	detail := e.Status.Detail
	if detail == "" {
		detail = strings.ReplaceAll(e.Status.State, "_", " ")
	}
	return fmt.Sprintf("%s %s %s", member, ref, detail)
}

// pullRequestCounts is the pull_request kind's v1 counts.
func pullRequestCounts(items []hp.Enrichment) hp.PullRequestCounts {
	var c hp.PullRequestCounts
	for _, e := range items {
		pr := e.PullRequest
		switch {
		case pr == nil:
			continue
		case pr.State == hp.PRStateMerged:
			c.Merged++
			continue
		case pr.State == hp.PRStateClosed:
			c.Closed++
			continue
		}
		c.Open++
		if pr.State == hp.PRStateDraft {
			c.Draft++
		}
		if pr.CI.Status == hp.CIPending {
			c.CIPending++
		}
		switch tierOf(e.Status.State) {
		case tierAttention:
			c.Attention++
		case tierReview:
			c.WaitingReview++
		case tierMerge:
			c.WaitingMerge++
		}
	}
	return c
}

func list(items []string) string {
	if len(items) <= maxReasonItems {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:maxReasonItems], ", ") + fmt.Sprintf(", +%d more", len(items)-maxReasonItems)
}

func prs(n int) string {
	if n == 1 {
		return "1 PR"
	}
	return fmt.Sprintf("%d PRs", n)
}

func agents(n int) string {
	switch n {
	case 0:
		return ""
	case 1:
		return " · 1 agent"
	}
	return fmt.Sprintf(" · %d agents", n)
}

// Normalize maps a rollup state from the wire onto the known set: an unknown
// value reads as ready (the compatibility rule consumers follow).
func Normalize(state string) string {
	switch state {
	case hp.RollupWaitingUser, hp.RollupRunning, hp.RollupPRAttention,
		hp.RollupWaitingReview, hp.RollupWaitingMerge, hp.RollupReady, hp.RollupIdle:
		return state
	}
	return hp.RollupReady
}
