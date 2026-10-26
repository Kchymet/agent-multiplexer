package daemon

import (
	"context"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/sessionreport"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

func twoReportingSessions(t *testing.T) (*Daemon, *sessionRuntime, func(id string) sessionrpc.DispatchResult) {
	t.Helper()
	const runtimeID = "33333333-3333-4333-8333-333333333333"
	a := store.Session{ID: "a1", RootID: "root", Agent: "claude", ClaudeID: runtimeID}
	b := store.Session{ID: "a2", RootID: "root", Agent: "claude", ClaudeID: runtimeID}
	d, runtime, principals := sessionRuntimeFixture(t, a, b)
	generations := map[string]string{a.ID: publishTestRuntime(t, d, a.ID), b.ID: publishTestRuntime(t, d, b.ID)}
	report := func(id string) sessionrpc.DispatchResult {
		result, err := runtime.dispatch(context.Background(), reportRequest(principals[id], sessionreport.Activity, map[string]string{
			sessionreport.FieldRuntimeGeneration: generations[id], sessionreport.FieldState: core.StateRunning,
		}))
		if err != nil {
			t.Errorf("report %s: %v", id, err)
		}
		return result
	}
	return d, runtime, report
}

// within runs fn and fails if it does not return within limit.
func within(t *testing.T, limit time.Duration, what string, fn func() sessionrpc.DispatchResult) sessionrpc.DispatchResult {
	t.Helper()
	done := make(chan sessionrpc.DispatchResult, 1)
	go func() { done <- fn() }()
	select {
	case result := <-done:
		return result
	case <-time.After(limit):
		t.Fatalf("%s did not complete within %v", what, limit)
		return sessionrpc.DispatchResult{}
	}
}

// TestSessionReportsProceedInParallel pins the lock split: one session's slow
// report never delays another's, and a slow shared holder (a creation) plus a
// waiting authorization change do not stall either.
func TestSessionReportsProceedInParallel(t *testing.T) {
	d, runtime, report := twoReportingSessions(t)

	// A's report is mid-effect (its subject lock is held).
	unlockA := runtime.reportLocks.lock("a1")
	if result := within(t, 2*time.Second, "B's report while A's is in flight", func() sessionrpc.DispatchResult { return report("a2") }); result.Status != sessionrpc.StatusOK {
		t.Fatalf("B report = %+v", result)
	}
	unlockA()

	// A slow creation holds shared admission, and a revocation politely waits
	// for exclusive admission behind it.
	releaseCreation, _ := lockAdmission(context.Background(), &runtime.dispatchMu, &d.effectMu, admitShared)
	exclusive := make(chan func(), 1)
	go func() {
		release, _ := lockAdmission(context.Background(), &runtime.dispatchMu, &d.effectMu, admitExclusive)
		exclusive <- release
	}()
	time.Sleep(20 * time.Millisecond) // let the exclusive acquirer start retrying
	for _, id := range []string{"a1", "a2"} {
		if result := within(t, 2*time.Second, id+"'s report during a slow creation", func() sessionrpc.DispatchResult { return report(id) }); result.Status != sessionrpc.StatusOK {
			t.Fatalf("%s report = %+v", id, result)
		}
	}
	select {
	case <-exclusive:
		t.Fatal("exclusive admission overlapped a shared holder")
	default:
	}
	releaseCreation()
	release := <-exclusive

	// Exclusive admission still fences reports: they wait for it.
	done := make(chan struct{})
	go func() {
		report("a1")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("report overlapped an authorization change")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	<-done
}

// TestReconcileNeverBlocksReports pins that a steady-state reconcile tick takes
// no admission lock, and one with work skips a busy gate instead of queueing.
func TestReconcileNeverBlocksReports(t *testing.T) {
	d, runtime, _ := twoReportingSessions(t)
	if err := runtime.reconcile(context.Background()); err != nil { // open both mailboxes
		t.Fatal(err)
	}
	if needed, err := runtime.reconcileNeeded(context.Background()); err != nil || needed {
		t.Fatalf("steady state still has reconcile work: %v %v", needed, err)
	}
	releaseShared, _ := lockAdmission(context.Background(), &runtime.dispatchMu, &d.effectMu, admitShared)
	defer releaseShared()
	within(t, time.Second, "steady-state reconcile under a shared holder", func() sessionrpc.DispatchResult {
		if err := runtime.reconcile(context.Background()); err != nil {
			t.Error(err)
		}
		return sessionrpc.DispatchResult{}
	})
	// Work pending (a new session), gate busy: the tick is skipped, not queued.
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(store.Session{ID: "a3", RootID: "root", Agent: "claude", Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	within(t, time.Second, "reconcile with work under a busy gate", func() sessionrpc.DispatchResult {
		if err := runtime.reconcile(context.Background()); err != nil {
			t.Error(err)
		}
		return sessionrpc.DispatchResult{}
	})
	if _, ok := runtime.servers["a3"]; ok {
		t.Fatal("reconcile acted while the gate was held")
	}
}

func TestSharedAdmissionActions(t *testing.T) {
	for action, shared := range map[string]bool{
		core.ActionAddAgent: true, core.ActionNewWorkgroup: true, core.ActionStart: true, core.ActionSteer: true,
		core.ActionRefresh: true, core.ActionSetArchived: false, core.ActionDelete: false, core.ActionRestart: false,
		core.ActionAccountManage: false, "some-future-verb": false,
	} {
		if got := sharedAdmissionAction(core.Action{Action: action}); got != shared {
			t.Errorf("sharedAdmissionAction(%q) = %v, want %v", action, got, shared)
		}
	}
}
