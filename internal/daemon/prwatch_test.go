package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/claudegoal"
	"amux/internal/core"
	"amux/internal/prwatch"
	"amux/internal/store"
)

// A PR wake-up typed into a Claude goal coordinator must continue its goal,
// never replace the objective as a new task would.
func TestPRWakeContinuesClaudeGoal(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	goal := claudegoal.State{Objective: "ship PR B", Status: claudegoal.StatusActive, Note: "waiting on CI", Created: 1}
	text := prWakeText(true, []string{"member m1 PR o/r#7: CI failed (check build) — https://github.com/o/r/pull/7"})
	if !strings.HasPrefix(text, prWakeMarker) {
		t.Fatalf("wake %q lacks the goal marker", text)
	}
	next := claudegoal.OnPrompt(goal, text, now)
	if next.Objective != goal.Objective || next.Status != claudegoal.StatusActive {
		t.Fatalf("wake replaced the goal: %+v", next)
	}
	// With no goal, the wake-up establishes none.
	if got := claudegoal.OnPrompt(claudegoal.State{}, text, now); !got.None() {
		t.Fatalf("wake established a goal: %+v", got)
	}
	if plain := prWakeText(false, []string{"x"}); strings.HasPrefix(plain, prWakeMarker) {
		t.Fatalf("non-goal wake %q carries the goal marker", plain)
	}
}

func TestLastTurnEnded(t *testing.T) {
	dir := t.TempDir()
	write := func(lines ...string) string {
		p := filepath.Join(dir, "t.jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ended := write(
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}}`,
		`{"type":"system","subtype":"stop_hook_summary"}`,
	)
	if !lastTurnEnded(ended) {
		t.Fatal("finished turn not recognized as idle")
	}
	pending := write(
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"id":"m2","role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"AskUserQuestion","input":{}}],"stop_reason":"tool_use"}}`,
	)
	if lastTurnEnded(pending) {
		t.Fatal("pending tool call recognized as idle")
	}
	if lastTurnEnded(filepath.Join(dir, "missing")) {
		t.Fatal("missing transcript recognized as idle")
	}
}

func prWakeFixture(t *testing.T) (*Daemon, *fakeInstance, store.Session, store.Session, prwatch.Wake) {
	t.Helper()
	isolateHome(t)
	root := putCoordinator(t, "wg", "claude")
	writeGoalFile(t, root, claudegoal.State{Objective: "ship", Status: claudegoal.StatusActive, Created: 1})
	member := store.Session{
		ID: "member", RootID: root.ID, Name: "member", Agent: "claude", Repo: "repo",
		Branch: "amux/wg-member", Dir: t.TempDir(), ClaudeID: convID("member"), Created: 2,
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRepo(store.Repo{Name: "repo", Source: "https://github.com/o/r.git", GitDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(member); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCoordinatorRepoGrants(root.ID, []string{"repo"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := core.WriteSessionHookState(root.ID, root.ClaudeID, core.StateReady, root.Dir); err != nil {
		t.Fatal(err)
	}
	d := New("", nil, time.Hour)
	eng := newFakeEngine()
	d.engine = eng
	in := eng.running(root.ID)
	wake := prwatch.Wake{
		SessionID: member.ID, Workgroup: root.ID, Repo: "o/r", Branch: member.Branch,
		PRBranch: member.Branch + "-topic", Text: "member member PR o/r#7: CI failed — https://github.com/o/r/pull/7",
	}
	return d, in, root, member, wake
}

func TestResolvePRWakesRechecksCurrentRelationship(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*testing.T, *store.DB, store.Session)
		eligible bool
	}{
		{name: "current", eligible: true},
		{name: "archived member remains readable", eligible: true, mutate: func(t *testing.T, db *store.DB, member store.Session) {
			if err := db.SetArchivedFlag(member.ID, true, 42); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "moved", mutate: func(t *testing.T, db *store.DB, member store.Session) {
			if err := db.SetRootID(member.ID, "other"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "deleted", mutate: func(t *testing.T, db *store.DB, member store.Session) {
			if err := db.DeleteSession(member.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "repo removed", mutate: func(t *testing.T, db *store.DB, member store.Session) {
			if err := db.SetRepoScope(member.ID, ""); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "repo rebound", mutate: func(t *testing.T, db *store.DB, _ store.Session) {
			if err := db.PutRepo(store.Repo{Name: "repo", Source: "https://github.com/other/r.git", GitDir: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "branch rebound", mutate: func(t *testing.T, db *store.DB, member store.Session) {
			member.Branch = "amux/other-member"
			if err := db.PutSession(member); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "repo grant removed", mutate: func(t *testing.T, db *store.DB, _ store.Session) {
			if err := db.SetCoordinatorRepoGrants("wg", nil); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, root, member, wake := prWakeFixture(t)
			db, err := store.Open()
			if err != nil {
				t.Fatal(err)
			}
			if tc.mutate != nil {
				tc.mutate(t, db, member)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			gotRoot, eligible, dropped, err := resolvePRWakes(root.ID, []prwatch.Wake{wake})
			if err != nil {
				t.Fatal(err)
			}
			if gotRoot.ID != root.ID {
				t.Fatalf("root = %+v, want %s", gotRoot, root.ID)
			}
			if tc.eligible {
				if len(eligible) != 1 || eligible[0] != 0 || len(dropped) != 0 {
					t.Fatalf("eligible=%v dropped=%v, want eligible", eligible, dropped)
				}
			} else if len(eligible) != 0 || len(dropped) != 1 || dropped[0] != 0 {
				t.Fatalf("eligible=%v dropped=%v, want dropped", eligible, dropped)
			}
		})
	}
}

func TestDeliverPRWakeDropsStaleItemFromMixedBatch(t *testing.T) {
	d, in, root, member, stale := prWakeFixture(t)
	current := member
	current.ID = "current"
	current.Name = "current"
	current.Branch = "amux/wg-current"
	current.Dir = t.TempDir()
	current.ClaudeID = convID(current.ID)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(current); err != nil {
		t.Fatal(err)
	}
	if err := db.SetArchivedFlag(current.ID, true, 42); err != nil {
		t.Fatal(err)
	}
	if err := db.SetRootID(member.ID, "other"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	valid := prwatch.Wake{
		SessionID: current.ID, Workgroup: root.ID, Repo: "o/r", Branch: current.Branch,
		PRBranch: current.Branch, Text: "member current PR o/r#8: CI green again — https://github.com/o/r/pull/8",
	}
	results := d.deliverPRWake(context.Background(), root.ID, []prwatch.Wake{stale, valid})
	if len(results) != 2 || results[0] != prwatch.Dropped || results[1] != prwatch.Delivered {
		t.Fatalf("results = %v, want [Dropped Delivered]", results)
	}
	written := in.written()
	if strings.Contains(written, stale.Text) || !strings.Contains(written, valid.Text) {
		t.Fatalf("prompt disclosed stale or lost valid wake: %q", written)
	}
}

func TestPRWakeAdmissionExcludesConcurrentPolicyMutation(t *testing.T) {
	d, _, root, member, wake := prWakeFixture(t)
	admitted := make(chan struct{})
	release := make(chan struct{})
	d.prWakeAdmission = func() {
		close(admitted)
		<-release
	}
	delivered := make(chan []prwatch.Delivery, 1)
	go func() { delivered <- d.deliverPRWake(context.Background(), root.ID, []prwatch.Wake{wake}) }()
	<-admitted

	mutationAdmitted := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		d.lockEffectExclusive()
		close(mutationAdmitted)
		db, err := store.Open()
		if err == nil {
			err = db.SetRootID(member.ID, "other")
			_ = db.Close()
		}
		d.effectMu.Unlock()
		mutationDone <- err
	}()
	select {
	case <-mutationAdmitted:
		t.Fatal("membership mutation crossed final wake admission")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if got := <-delivered; len(got) != 1 || got[0] != prwatch.Delivered {
		t.Fatalf("delivery = %v, want Delivered", got)
	}
	select {
	case <-mutationAdmitted:
	case <-time.After(time.Second):
		t.Fatal("membership mutation remained blocked after delivery")
	}
	if err := <-mutationDone; err != nil {
		t.Fatalf("mutate membership: %v", err)
	}
}

func TestCoordinatorWakeFailsClosedOnGoalReadinessAndPermission(t *testing.T) {
	t.Run("eligible waiting goal", func(t *testing.T) {
		d, _, root, _, _ := prWakeFixture(t)
		writeGoalFile(t, root, claudegoal.State{Objective: "ship", Status: claudegoal.StatusActive, Note: "waiting on CI"})
		if ready, why := d.coordinatorTakesWake(root); !ready {
			t.Fatalf("eligible waiting goal refused: %s", why)
		}
	})
	for _, status := range []string{
		claudegoal.StatusPaused, claudegoal.StatusBlocked, claudegoal.StatusComplete, claudegoal.StatusInput,
	} {
		t.Run(status, func(t *testing.T) {
			d, _, root, _, _ := prWakeFixture(t)
			writeGoalFile(t, root, claudegoal.State{Objective: "ship", Status: status})
			if ready, _ := d.coordinatorTakesWake(root); ready {
				t.Fatalf("goal %s accepted an automatic wake", status)
			}
		})
	}
	t.Run("unknown goal", func(t *testing.T) {
		d, _, root, _, _ := prWakeFixture(t)
		if err := os.WriteFile(claudegoal.StatePath(root.Dir), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if ready, why := d.coordinatorTakesWake(root); ready || !strings.Contains(why, "unknown") {
			t.Fatalf("unknown goal readiness = %v, %q", ready, why)
		}
	})
	t.Run("unknown readiness", func(t *testing.T) {
		d, _, root, _, _ := prWakeFixture(t)
		root.ClaudeID = "unobserved-runtime"
		if ready, why := d.coordinatorTakesWake(root); ready || !strings.Contains(why, "unknown") {
			t.Fatalf("unknown activity readiness = %v, %q", ready, why)
		}
	})
	t.Run("permission open", func(t *testing.T) {
		d, _, root, _, _ := prWakeFixture(t)
		if err := core.AppendPermission(root.ID, core.PermissionRecord{RequestID: "open", Tool: "Bash"}); err != nil {
			t.Fatal(err)
		}
		if ready, why := d.coordinatorTakesWake(root); ready || !strings.Contains(why, "permission") {
			t.Fatalf("open permission readiness = %v, %q", ready, why)
		}
	})
}

func TestStructuredPRWakeAdmitsTurnBeforeDeferredWait(t *testing.T) {
	d := New("", nil, time.Hour)
	steerer := &gatedPromptSteerer{started: make(chan struct{}), release: make(chan struct{})}
	if err := d.beginStructuredPRWake(context.Background(), "wg", steerer, "check PR"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-steerer.started:
	default:
		t.Fatal("structured wake returned before turn/start admission")
	}
	done := make(chan struct{})
	go func() {
		d.deferredWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("structured model-turn waiter did not remain deferred")
	case <-time.After(20 * time.Millisecond):
	}
	close(steerer.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("structured model-turn waiter did not finish")
	}
}
