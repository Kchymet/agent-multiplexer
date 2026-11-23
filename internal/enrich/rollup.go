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
	PRs   []hp.SessionPR
}

// Input is everything a workgroup's rollup depends on.
type Input struct {
	// OwnState is the coordinator's own turn activity (a hp.State* value).
	OwnState string
	// Goal is the coordinator's goal status in goal mode ("" otherwise).
	Goal     string
	GoalNote string
	Members  []Member
	Now      time.Time
	// PRsObserved: the daemon watches PRs. Without it PRs neither count nor
	// drive a state, and the rollup carries no pull_request counts.
	PRsObserved bool
	// Stale reports one PR's freshness (nil: fresh, observed time unknown).
	Stale func(hp.SessionPR) Staleness
	// StaleReason is the watcher-wide failure (a hp.Stale* value, or ""); a
	// gh_unavailable one marks every rollup stale.
	StaleReason string
	// ObservedAt is the watcher's last fully successful poll (zero: none yet).
	ObservedAt time.Time
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

// maxReasonItems bounds how many members/PRs a reason names.
const maxReasonItems = 3

// Rollup derives a workgroup's state, first match wins:
//
//	waiting_user   someone is blocked on the user: the coordinator's own
//	               prompt or question, its goal needing the user while it is
//	               between turns, or a waiting member (NeedsYou names who)
//	running        the coordinator or a member is running (or live, unknown);
//	               a between-turns coordinator then is "supervising"
//	pr_attention   an open PR needs work: the coordinator's job, not the user's
//	waiting_review an open PR waits on review
//	waiting_merge  an open PR waits on a merge
//	ready / idle   otherwise
//
// It is a pure function of member states and PRs: whatever decides a member's
// state (e.g. honest waiting detection) feeds straight into it.
func Rollup(in Input) Result {
	own := in.OwnState
	if own == "" {
		own = hp.StateIdle
	}
	between := own == hp.StateReady || own == hp.StateIdle || own == hp.StateUnknown
	goalNeeds := between && goalNeedsUser(in.Goal)
	maxState := own
	var needs, busy, prItems []string
	if own == hp.StateWaiting || goalNeeds {
		needs = append(needs, CoordinatorLabel)
	}
	nRunning := 0
	inflight := false
	counts := hp.PullRequestCounts{}
	tier := map[string][]string{} // phase -> reason items
	stale := false
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
		if !in.PRsObserved {
			continue
		}
		for _, pr := range Retain(m.PRs, in.Now) {
			switch pr.State {
			case "merged":
				counts.Merged++
				continue
			case "closed":
				counts.Closed++
				continue
			}
			if !IsOpen(pr) {
				continue
			}
			counts.Open++
			if pr.Draft {
				counts.Draft++
			}
			if CI(pr).Status == hp.CIPending {
				counts.CIPending++
			}
			phase, _ := Phase(pr)
			switch phase {
			case hp.PRPhaseAttention:
				counts.Attention++
			case hp.PRPhaseReview:
				counts.WaitingReview++
			case hp.PRPhaseMerge:
				counts.WaitingMerge++
			}
			item := prItem(m.Label, pr)
			prItems = append(prItems, item)
			tier[phase] = append(tier[phase], item)
			if in.Stale != nil && in.Stale(pr).Stale {
				stale = true
			}
		}
	}

	r := Result{}
	ro := hp.Rollup{ObservedAt: rfc3339(in.ObservedAt)}
	if in.PRsObserved {
		c := counts
		ro.Enrichments.PullRequest = &c
		switch {
		case in.StaleReason == hp.StaleGHUnavailable:
			ro.Stale, ro.StaleReason = true, hp.StaleGHUnavailable
		case stale:
			ro.Stale, ro.StaleReason = true, in.StaleReason
			if ro.StaleReason == "" {
				ro.StaleReason = hp.StaleError
			}
		}
	}
	var reason []string
	stopped := own == hp.StateIdle
	supervising := between && inflight
	supervise := func() {
		if !supervising {
			return
		}
		r.SubState = hp.SubStateSupervising
		if items := append(append([]string(nil), busy...), prItems...); len(items) > 0 {
			reason = append(reason, "supervising "+list(items))
		}
		if stopped {
			reason = append(reason, "coordinator stopped: wake-ups wait until it starts")
		}
	}
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
	case len(tier[hp.PRPhaseAttention]) > 0:
		n := len(tier[hp.PRPhaseAttention])
		ro.State, ro.Count, ro.Label = hp.RollupPRAttention, n, "PR attention · "+prs(n)
		r.State, r.SubState = hp.StateRunning, hp.SubStatePRAttention
		reason = append(reason, list(tier[hp.PRPhaseAttention]))
		if stopped {
			r.State = hp.StateReady
			reason = append(reason, "coordinator stopped")
		}
	case len(tier[hp.PRPhaseReview]) > 0:
		n := len(tier[hp.PRPhaseReview])
		ro.State, ro.Count, ro.Label = hp.RollupWaitingReview, n, "waiting for review · "+prs(n)
		if m := len(tier[hp.PRPhaseMerge]); m > 0 {
			ro.Label += fmt.Sprintf(" · %d ready to merge", m)
		}
		r.State, r.SubState = hp.StateReady, hp.SubStateWaitingReview
		reason = append(reason, list(append(tier[hp.PRPhaseReview], tier[hp.PRPhaseMerge]...)))
	case len(tier[hp.PRPhaseMerge]) > 0:
		n := len(tier[hp.PRPhaseMerge])
		ro.State, ro.Count, ro.Label = hp.RollupWaitingMerge, n, "waiting for merge · "+prs(n)
		r.State, r.SubState = hp.StateReady, hp.SubStateWaitingMerge
		reason = append(reason, list(tier[hp.PRPhaseMerge]))
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

// prItem is one PR in a reason, e.g. "m1 PR #12 CI failed".
func prItem(member string, pr hp.SessionPR) string {
	what := "open"
	switch {
	case pr.Draft:
		what = "draft"
	case pr.Mergeable == "conflicting":
		what = "conflict"
	case pr.Checks == "fail":
		what = "CI failed"
	case pr.ReviewDecision == "changes_requested":
		what = "changes requested"
	case pr.MergeState == "behind":
		what = "behind"
	case pr.Checks == "pending":
		what = "CI running"
	case pr.ReviewDecision == "approved":
		what = "approved"
	case pr.ReviewDecision == "review_required":
		what = "awaiting review"
	case pr.Checks == "pass":
		what = "ready"
	}
	return fmt.Sprintf("%s PR #%d %s", member, pr.Number, what)
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
