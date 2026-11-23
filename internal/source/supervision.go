package source

import (
	"fmt"
	"strings"

	"amux/internal/core"
	"amux/internal/enrich"
	"amux/internal/prwatch"
	"amux/internal/store"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// maxGoalNoteBytes bounds Supervision.GoalNote.
const maxGoalNoteBytes = 240

// memberView is one active member as the coordinator row sees it.
type memberView struct {
	session store.Session
	state   string
	prs     []harnessproto.SessionPR
	items   []harnessproto.Enrichment // its rendered enrichments, every kind
}

// coordinatorView is how a workgroup coordinator row presents: the state it
// publishes, its sub-state, the word(s) that lead its status line, its
// supervision detail and its rollup.
type coordinatorView struct {
	state       string
	subState    string
	lead        string
	supervision harnessproto.Supervision
	rollup      harnessproto.Rollup
}

// prContext is what the rollup needs to know about the enrichment sources,
// read once per snapshot.
type prContext struct {
	sources []enrich.SourceStatus // registered sources' freshness (none: not observed)
	probe   EnrichmentProbe
}

// enrichments is a row's Enrichments field (nil when there is none or no
// source is registered).
func (pc prContext) enrichments(id string) *harnessproto.Enrichments {
	if pc.probe == nil || len(pc.sources) == 0 {
		return nil
	}
	return pc.probe.Enrichments(id)
}

// superviseCoordinator decides how a coordinator row presents from its own
// activity, its goal and its members. The state itself is enrich.Rollup — the
// one shared rollup — whose precedence is waiting on the user > running >
// pr_attention > waiting_review / waiting_merge > ready / idle; this adds the
// supervision detail (the members still in flight).
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
//     coordinator that is not itself mid-turn (ready, idle or unknown) while a
//     member runs is "supervising" — state running, SubState supervising,
//     status "running · N agents". An idle coordinator with a running member
//     is running.
//   - With only open PRs outstanding the row is calm: pr_attention (state
//     running — the coordinator's job; ready when it is stopped), or
//     waiting_review / waiting_merge (state ready, not the attention badge).
//   - It is idle/ready only when it and all its members are.
func superviseCoordinator(ownState, goal, goalNote string, members []memberView, pc prContext) coordinatorView {
	in := enrich.Input{OwnState: ownState, Goal: goal, GoalNote: goalNote, Sources: pc.sources}
	sup := harnessproto.Supervision{OwnState: ownState, Goal: goal, GoalNote: boundNote(goalNote)}
	for _, m := range members {
		in.Members = append(in.Members, enrich.Member{ID: m.session.ID, Label: memberLabel(m.session), State: m.state, Enrichments: m.items})
		open := openPRs(m.prs)
		busy := m.state == core.StateRunning || m.state == core.StateWaiting || m.state == core.StateUnknown
		if !busy && len(open) == 0 {
			continue
		}
		sup.Members = append(sup.Members, harnessproto.SupervisedMember{
			ID: m.session.ID, Title: agentLabel(m.session), State: m.state, PRs: prwatch.Summary(open),
		})
	}
	r := enrich.Rollup(in)
	sup.NeedsYou = r.NeedsYou
	sup.Reason = r.Rollup.Reason
	return coordinatorView{state: r.State, subState: r.SubState, lead: r.Lead, supervision: sup, rollup: r.Rollup}
}

// CoordinatorLabel names the coordinator itself in Supervision.NeedsYou.
const CoordinatorLabel = enrich.CoordinatorLabel

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
