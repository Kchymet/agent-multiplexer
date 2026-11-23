package source

import (
	"fmt"
	"strings"

	"amux/internal/core"
	"amux/internal/prwatch"
	"amux/internal/store"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// maxReasonItems bounds how many members/PRs a coordinator's reason names
// before it collapses the rest into "+N more".
const maxReasonItems = 3

// maxGoalNoteBytes bounds Supervision.GoalNote.
const maxGoalNoteBytes = 240

// memberView is one active member as the coordinator row sees it.
type memberView struct {
	session store.Session
	state   string
	prs     []harnessproto.SessionPR
}

// coordinatorView is how a workgroup coordinator row presents: the state it
// publishes, its sub-state, the word that leads its status line, and its
// supervision detail.
type coordinatorView struct {
	state       string
	subState    string
	lead        string
	supervision harnessproto.Supervision
}

// superviseCoordinator decides how a coordinator row presents from its own
// activity, its goal and its members.
//
// "waiting" is the attention chip and means one thing everywhere: someone in
// the workgroup is blocked on the user. Idle is never waiting. So:
//
//   - The row is waiting only when the coordinator itself is waiting (a
//     permission prompt or question of its own), its goal needs the user while
//     it is between turns — input (it asked a question), blocked,
//     budgetLimited — or a member is waiting. Supervision.NeedsYou names who,
//     and the status line leads "waiting · N agents need you".
//   - Otherwise it is running when the coordinator or any member is: a
//     coordinator that is not itself mid-turn (ready, idle or unknown) while
//     work it dispatched is in flight is "supervising" — state running,
//     SubState supervising, status "running · N agents". In flight means a
//     member running, waiting or unknown — or, while its goal is waiting on a
//     wake-up, a member owning an open PR (CI, review and merge are what it
//     waits on then). An idle coordinator with a running member is running.
//   - It is idle/ready only when it and all its members are.
func superviseCoordinator(ownState, goal, goalNote string, members []memberView) coordinatorView {
	v := coordinatorView{state: ownState, lead: ownState}
	v.supervision = harnessproto.Supervision{OwnState: ownState, Goal: goal, GoalNote: boundNote(goalNote)}

	between := ownState == core.StateReady || ownState == core.StateIdle || ownState == core.StateUnknown
	goalNeedsUser := between && (goal == "input" || goal == "blocked" || goal == "budgetLimited")
	if ownState == core.StateWaiting || goalNeedsUser {
		v.supervision.NeedsYou = append(v.supervision.NeedsYou, CoordinatorLabel)
	}

	var active, prItems []string
	inflight := false
	for _, m := range members {
		if stateRank(m.state) > stateRank(v.state) {
			v.state = m.state
		}
		open := openPRs(m.prs)
		busy := m.state == core.StateRunning || m.state == core.StateWaiting || m.state == core.StateUnknown
		if !busy && len(open) == 0 {
			continue
		}
		v.supervision.Members = append(v.supervision.Members, harnessproto.SupervisedMember{
			ID: m.session.ID, Title: agentLabel(m.session), State: m.state, PRs: prwatch.Summary(open),
		})
		label := memberLabel(m.session)
		if m.state == core.StateWaiting {
			v.supervision.NeedsYou = append(v.supervision.NeedsYou, label)
		}
		if busy {
			inflight = true
			if m.state != core.StateWaiting {
				active = append(active, fmt.Sprintf("%s %s", label, m.state))
			}
		}
		for _, pr := range open {
			prItems = append(prItems, prItem(label, pr))
		}
	}

	if between && (inflight || (goal == "waiting" && len(prItems) > 0)) {
		v.subState = harnessproto.SubStateSupervising
		if stateRank(v.state) < stateRank(core.StateRunning) {
			v.state = core.StateRunning
		}
		v.lead = core.StateRunning
	}
	if len(v.supervision.NeedsYou) > 0 {
		v.state = core.StateWaiting
		v.lead = core.StateWaiting
	}

	var reason []string
	if len(v.supervision.NeedsYou) > 0 {
		reason = append(reason, "needs you: "+listItems(v.supervision.NeedsYou))
	}
	if goalNeedsUser && goalNote != "" {
		reason = append(reason, "goal "+goal+": "+oneLine(goalNote))
	}
	if v.subState != "" {
		if items := append(active, prItems...); len(items) > 0 {
			reason = append(reason, "supervising "+listItems(items))
		}
		if ownState == core.StateIdle {
			reason = append(reason, "coordinator stopped: wake-ups wait until it starts")
		}
	}
	v.supervision.Reason = strings.Join(reason, "; ")
	return v
}

// CoordinatorLabel names the coordinator itself in Supervision.NeedsYou.
const CoordinatorLabel = "coordinator"

// coordinatorStatus is a coordinator row's status line: "waiting · 1 agent
// needs you" when someone is blocked on the user, else "<lead> · N agents"
// (running while any work is in flight, else ready/idle), then the goal
// suffix and the reason.
func coordinatorStatus(v coordinatorView, members int, goalSuffix string) string {
	var status string
	if n := len(v.supervision.NeedsYou); n > 0 {
		verb := "need"
		if n == 1 {
			verb = "needs"
		}
		status = fmt.Sprintf("%s · %d agent%s %s you", core.StateWaiting, n, plural(n), verb)
	} else {
		status = fmt.Sprintf("%s · %d agent%s", stateLabel(v.lead), members, plural(members))
	}
	status += goalSuffix
	if v.supervision.Reason != "" {
		status += " · " + v.supervision.Reason
	}
	return status
}

func openPRs(prs []harnessproto.SessionPR) []harnessproto.SessionPR {
	var out []harnessproto.SessionPR
	for _, pr := range prs {
		if pr.State == "open" {
			out = append(out, pr)
		}
	}
	return out
}

// memberLabel names a member compactly in a reason: its name, else its short id.
func memberLabel(s store.Session) string {
	if n := strings.TrimSpace(s.Name); n != "" {
		return n
	}
	return shortID(s.ID)
}

// prItem is one open PR in a reason, e.g. "m1 PR #12 CI pending".
func prItem(member string, pr harnessproto.SessionPR) string {
	what := "open"
	switch {
	case pr.Draft:
		what = "draft"
	case pr.Mergeable == "conflicting":
		what = "conflict"
	case pr.Checks == "fail":
		what = "CI failed"
	case pr.Checks == "pending":
		what = "CI pending"
	case pr.ReviewDecision == "changes_requested":
		what = "changes requested"
	case pr.ReviewDecision == "approved":
		what = "approved"
	case pr.Checks == "pass":
		what = "awaiting review"
	}
	return fmt.Sprintf("%s PR #%d %s", member, pr.Number, what)
}

func listItems(items []string) string {
	if len(items) <= maxReasonItems {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:maxReasonItems], ", ") + fmt.Sprintf(", +%d more", len(items)-maxReasonItems)
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func boundNote(s string) string {
	s = oneLine(s)
	if len(s) <= maxGoalNoteBytes {
		return s
	}
	cut := maxGoalNoteBytes
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
