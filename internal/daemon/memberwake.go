package daemon

import (
	"fmt"
	"sync"

	"amux/internal/core"
	"amux/internal/items"
	"amux/internal/prwatch"
	"amux/internal/store"
)

// This file pushes workgroup members' activity changes to their coordinator in
// real time, so a coordinator that dispatched work can end its turn (or run
// `amux agent goal wait`) and be woken when a member finishes, needs the user,
// or stops — instead of polling `amux agent events` in a background loop.
//
// The signal is the rail state the daemon already maintains: every activity
// hook report triggers an immediate rail poll (session_reports.go), and each
// poll hands its rows to observeWorkgroups, which diffs member states against
// the previous poll. A meaningful transition becomes a prwatch.KindMember wake
// on the PR watcher's per-coordinator queue, so it shares that queue's
// coalescing, revalidation, delivery gating (live coordinator, between turns,
// continuable goal) and `[amux goal]` marker. A change to a coordinator's own
// row kicks the queue, so a wake deferred while the coordinator was mid-turn
// is offered the moment that turn ends rather than at a timer.

// memberWatch is observeWorkgroups' memory between polls.
type memberWatch struct {
	mu sync.Mutex
	// members is each active member's last observed state, by session id.
	members map[string]string
	// roots is each active coordinator row's last observed state+status.
	roots map[string]string
	// goals is each active coordinator's last observed goal status; a move to
	// input, blocked or complete tells the overseer (overseerGoalUpdate).
	goals map[string]string
	// primed is false until the first poll: a daemon (re)start observes the
	// current states as a baseline and wakes nobody for them.
	primed bool
}

// memberTransition decides whether a member's move from prev to cur is worth
// waking its coordinator, and how to say it. prev is "" for a member the
// daemon has not seen before (new, or the daemon just started): never a wake.
func memberTransition(prev, cur string) (what string, wake bool) {
	if prev == "" || prev == cur {
		return "", false
	}
	switch cur {
	case core.StateReady:
		if prev == core.StateRunning || prev == core.StateWaiting {
			return "finished its turn", true
		}
	case core.StateWaiting:
		// Mid-turn: a permission prompt or a question for the user. (Claude's
		// idle reminder no longer reports waiting; see
		// claudecfg.NotificationNeedsUser.)
		return "is waiting on a permission prompt or question", true
	case core.StateStopped, core.StateIdle:
		return "stopped (its runtime exited)", true
	}
	return "", false
}

// observeWorkgroups diffs one poll's rows against the previous poll and hands
// member transitions to the wake queue.
func (d *Daemon) observeWorkgroups(rows []core.Session) {
	if d.prs == nil {
		return
	}
	w := &d.memberWatch
	w.mu.Lock()
	defer w.mu.Unlock()
	coordinators := map[string]bool{}
	roots := make(map[string]string, len(w.roots))
	goals := make(map[string]string, len(w.goals))
	for _, r := range rows {
		if r.IsRoot && r.Role == store.RoleCoordinator && r.Section == core.SectionWorkgroups && !r.Archived {
			coordinators[r.ID] = true
			roots[r.ID] = r.State + "|" + r.Status
			if sup := r.Supervision; sup != nil && sup.Goal != "" {
				goals[r.ID] = sup.Goal
				if w.primed {
					if what, wake := overseerGoalUpdate(w.goals[r.ID], sup.Goal, sup.GoalNote); wake {
						owner := items.GlobalInbox()
						d.prs.Notify(prwatch.Wake{Kind: prwatch.KindOverseer, Workgroup: owner, SessionID: r.ID,
							Text: fmt.Sprintf("coordinator %s %s", memberWakeLabel(r), what)})
					}
				}
			}
		}
	}
	members := make(map[string]string, len(w.members))
	kick := false
	for _, r := range rows {
		if r.RootID == "" || r.IsRoot || r.Archived || r.Section != core.SectionWorkgroups || !coordinators[r.RootID] {
			continue
		}
		members[r.ID] = r.State
		if !w.primed {
			continue
		}
		what, wake := memberTransition(w.members[r.ID], r.State)
		if !wake {
			continue
		}
		d.prs.Notify(prwatch.Wake{
			Kind: prwatch.KindMember, SessionID: r.ID, Workgroup: r.RootID, State: r.State,
			Text: fmt.Sprintf("member %s %s", memberWakeLabel(r), what),
		})
	}
	for id, v := range roots {
		if w.primed && w.roots[id] != v {
			kick = true
		}
	}
	// A coordinator that left the rail (archived, deleted) had its active
	// items carried over to the global inbox in that transaction; tell its owner.
	// The first poll after a (re)start offers whatever was left unnotified.
	offerItems := !w.primed
	for id := range w.roots {
		if _, still := roots[id]; !still {
			offerItems = true
		}
	}
	w.members, w.roots, w.goals, w.primed = members, roots, goals, true
	if offerItems {
		d.offerItemWakes()
	}
	if kick {
		d.prs.Kick()
	}
}

// maxWakeTitleRunes bounds the member title quoted in a wake line.
const maxWakeTitleRunes = 60

func memberWakeLabel(r core.Session) string {
	if r.Title == "" || r.Title == r.ID {
		return r.ID
	}
	title := []rune(r.Title)
	if len(title) > maxWakeTitleRunes {
		title = append(title[:maxWakeTitleRunes], '…')
	}
	return r.ID + " (" + string(title) + ")"
}

// sessionStateNow is a session's state in the latest rail poll ("" when it is
// not on the rail).
func (d *Daemon) sessionStateNow(id string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for _, s := range d.sessions {
		if s.ID == id {
			return s.State
		}
	}
	return ""
}
