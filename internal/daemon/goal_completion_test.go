package daemon

import (
	"context"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/store"
)

type completedWorkgroup struct{ ready bool }

func (g completedWorkgroup) CompleteWorkgroup(commit func() error) (bool, error) {
	if !g.ready {
		return false, nil
	}
	err := commit()
	return err == nil, err
}

func TestGoalCompletionArchivesWorkgroupAndStopsItsMembers(t *testing.T) {
	isolateHome(t)
	d := New("", nil, time.Hour)
	d.engine = newFakeEngine()
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	root := store.Session{ID: "root", Agent: "codex", Dir: t.TempDir()}
	member := store.Session{ID: "member", RootID: root.ID, Agent: "claude", Dir: t.TempDir()}
	other := store.Session{ID: "other", Agent: "codex", Dir: t.TempDir()}
	for _, s := range []store.Session{root, member, other} {
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
		d.sessions = append(d.sessions, core.Session{ID: s.ID, RootID: s.RootID})
		for tab := 0; tab < 3; tab++ {
			if _, err := d.engine.Ensure(context.Background(), engine.Spec{Key: engine.Key{AgentID: s.ID, Tab: tab}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	d.completeWorkgroupGoal(context.Background(), root, completedWorkgroup{ready: false})
	if got, _, _ := db.GetSession(root.ID); got.Archived {
		t.Fatal("unfinished goal archived")
	}
	if len(d.engine.Live()) != 9 {
		t.Fatal("unfinished goal stopped a runtime")
	}
	d.completeWorkgroupGoal(context.Background(), root, completedWorkgroup{ready: true})
	got, _, err := db.GetSession(root.ID)
	if err != nil || !got.Archived || got.ArchivedAt == 0 {
		t.Fatalf("completion not persisted: %+v %v", got, err)
	}
	if live := d.engine.Live(); len(live) != 3 {
		t.Fatalf("live runtimes: %+v", live)
	}
	for _, key := range d.engine.Live() {
		if key.AgentID != other.ID {
			t.Fatalf("completed workgroup still running: %+v", key)
		}
	}
	// Archive keeps the records/worktrees, just as a host archive does.
	if _, found, err := db.GetSession(member.ID); err != nil || !found {
		t.Fatalf("member removed: %v", err)
	}
	select {
	case <-d.pollNow:
	default:
		t.Fatal("rail not refreshed")
	}
}

func TestGoalCompletionDoesNotArchiveOrdinarySessions(t *testing.T) {
	isolateHome(t)
	d := New("", nil, time.Hour)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range []store.Session{
		{ID: "member", RootID: "root", Agent: "codex"},
		{ID: "repo", Scope: store.ScopeRepo, Agent: "codex"},
		{ID: "claude", Agent: "claude"},
	} {
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
		d.completeWorkgroupGoal(context.Background(), s, completedWorkgroup{ready: true})
		if got, _, _ := db.GetSession(s.ID); got.Archived {
			t.Fatalf("ordinary session %s archived", s.ID)
		}
	}
}
