package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"amux/internal/claudegoal"
	"amux/internal/core"
	"amux/internal/prwatch"
	"amux/internal/store"
)

func TestMemberTransition(t *testing.T) {
	cases := []struct {
		prev, cur string
		wake      bool
		what      string
	}{
		{"", core.StateReady, false, ""}, // first sighting / daemon start
		{core.StateRunning, core.StateRunning, false, ""},
		{core.StateRunning, core.StateReady, true, "finished its turn"},
		{core.StateWaiting, core.StateReady, true, "finished its turn"},
		{core.StateRunning, core.StateWaiting, true, "waiting on a permission prompt"},
		{core.StateReady, core.StateRunning, false, ""}, // steered: the coordinator did that
		{core.StateWaiting, core.StateRunning, false, ""},
		{core.StateRunning, core.StateIdle, true, "stopped"},
		{core.StateIdle, core.StateReady, false, ""}, // (re)started
		{core.StateUnknown, core.StateReady, false, ""},
	}
	for _, tc := range cases {
		what, wake := memberTransition(tc.prev, tc.cur)
		if wake != tc.wake || !strings.Contains(what, tc.what) {
			t.Errorf("memberTransition(%q, %q) = %q, %v; want wake=%v containing %q", tc.prev, tc.cur, what, wake, tc.wake, tc.what)
		}
	}
}

func workgroupRows(root, member store.Session, coordinatorState, memberState string) []core.Session {
	return []core.Session{
		{ID: root.ID, IsRoot: true, Role: store.RoleCoordinator, Section: core.SectionWorkgroups, State: coordinatorState, Status: coordinatorState + " · 1 agent"},
		{ID: member.ID, Title: member.Name, RootID: root.ID, Section: core.SectionWorkgroups, State: memberState, Status: memberState},
	}
}

func (d *Daemon) setRows(rows []core.Session) {
	d.mu.Lock()
	d.sessions = rows
	d.mu.Unlock()
}

// A member finishing its turn reaches its goal coordinator as a prompt within
// the coalescing window — no polling loop on the coordinator's side — and the
// prompt continues the goal rather than replacing it.
func TestMemberFinishWakesCoordinator(t *testing.T) {
	d, in, root, member, _ := prWakeFixture(t)
	d.prs = prwatch.New(prwatch.Config{
		Run:     func(context.Context, ...string) ([]byte, error) { return nil, nil },
		Members: func() ([]prwatch.Member, error) { return nil, nil },
		Load:    func() (map[string]string, error) { return nil, nil },
		Save:    func(string, string) error { return nil },
		Emit:    func(prwatch.Change) {},
		Deliver: d.deliverPRWake,
		Window:  20 * time.Millisecond,
		NoPoll:  true,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.prs.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	running := workgroupRows(root, member, core.StateReady, core.StateRunning)
	d.setRows(running)
	d.observeWorkgroups(running) // baseline: nobody is woken for it
	finished := workgroupRows(root, member, core.StateReady, core.StateReady)
	d.setRows(finished)
	d.observeWorkgroups(finished)

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(in.written(), "member member finished its turn") {
		if time.Now().After(deadline) {
			t.Fatalf("coordinator was not woken; typed %q", in.written())
		}
		time.Sleep(10 * time.Millisecond)
	}
	text := in.written()
	if !strings.Contains(text, claudegoal.Marker+" Workgroup update") {
		t.Fatalf("wake %q lacks the goal marker / workgroup header", text)
	}
	goal := claudegoal.State{Objective: "ship", Status: claudegoal.StatusWaiting, Note: "m1", Created: 1}
	if next := claudegoal.OnPrompt(goal, strings.TrimSpace(text), time.Now()); next.Objective != "ship" || next.Status != claudegoal.StatusActive {
		t.Fatalf("wake did not continue the goal: %+v", next)
	}
}

// A member wake is dropped when the member moved on before delivery, and for a
// coordinator with no goal to continue.
func TestDeliverMemberWakeGates(t *testing.T) {
	d, in, root, member, _ := prWakeFixture(t)
	wake := prwatch.Wake{Kind: prwatch.KindMember, SessionID: member.ID, Workgroup: root.ID, State: core.StateReady, Text: "member member finished its turn"}

	d.setRows(workgroupRows(root, member, core.StateReady, core.StateRunning)) // steered again since
	if got := d.deliverPRWake(context.Background(), root.ID, []prwatch.Wake{wake}); len(got) != 1 || got[0] != prwatch.Dropped {
		t.Fatalf("stale member wake = %v, want Dropped", got)
	}

	d.setRows(workgroupRows(root, member, core.StateReady, core.StateReady))
	writeGoalFile(t, root, claudegoal.State{})
	if got := d.deliverPRWake(context.Background(), root.ID, []prwatch.Wake{wake}); len(got) != 1 || got[0] != prwatch.Dropped {
		t.Fatalf("member wake to a goal-less coordinator = %v, want Dropped", got)
	}
	if w := in.written(); w != "" {
		t.Fatalf("gated wake typed %q", w)
	}

	writeGoalFile(t, root, claudegoal.State{Objective: "ship", Status: claudegoal.StatusActive, Note: "m1", Created: 1})
	if got := d.deliverPRWake(context.Background(), root.ID, []prwatch.Wake{wake}); len(got) != 1 || got[0] != prwatch.Delivered {
		t.Fatalf("current member wake = %v, want Delivered", got)
	}
	if !strings.Contains(in.written(), wake.Text) {
		t.Fatalf("wake not typed: %q", in.written())
	}
}
