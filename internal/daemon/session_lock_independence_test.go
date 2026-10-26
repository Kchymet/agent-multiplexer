package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/sessionreport"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

// TestSessionReportIndependence records which slow holders delay session A's
// report. docs/daemon-locking.md is the plan: each lock-split PR flips one row
// from blocked to independent, and this table is the proof. A row that starts
// passing without its PR is a lock change worth a look, so the expectation is
// exact rather than an upper bound.
func TestSessionReportIndependence(t *testing.T) {
	tests := []struct {
		name string
		// hold starts a slow holder that touches only resources other than
		// session A's, calls held once it is inside its critical section, and
		// returns when release is closed.
		hold    func(t *testing.T, f *independenceFixture, held func(), release <-chan struct{})
		blocked bool
	}{
		{name: "peer session report", hold: holdPeerReport, blocked: true},
		{name: "host action on another session", hold: holdHostAction, blocked: true},
		{name: "reconcile pass", hold: holdReconcile, blocked: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newIndependenceFixture(t)
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseAll()
			held := make(chan struct{})
			var heldOnce sync.Once
			holderDone := make(chan struct{})
			go func() {
				defer close(holderDone)
				tc.hold(t, f, func() { heldOnce.Do(func() { close(held) }) }, release)
			}()
			select {
			case <-held:
			case <-time.After(10 * time.Second):
				t.Fatal("holder never entered its critical section")
			}

			reported := make(chan sessionrpc.DispatchResult, 1)
			go func() { reported <- f.report(t, "a1") }()
			// An independent report finishes promptly even under -race on a slow
			// runner; a blocked one cannot finish until release, so a short
			// observation window is enough to tell them apart without flaking.
			window := 250 * time.Millisecond
			if !tc.blocked {
				window = 10 * time.Second
			}
			var result sessionrpc.DispatchResult
			blocked := false
			select {
			case result = <-reported:
			case <-time.After(window):
				blocked = true
			}
			releaseAll()
			if blocked {
				result = <-reported
			}
			<-holderDone
			if result.Status != sessionrpc.StatusOK {
				t.Fatalf("report = %+v", result)
			}
			if blocked != tc.blocked {
				t.Fatalf("report blocked by %s = %v, want %v (update docs/daemon-locking.md with this row)", tc.name, blocked, tc.blocked)
			}
		})
	}
}

type independenceFixture struct {
	d           *Daemon
	runtime     *sessionRuntime
	principals  map[string]access.Principal
	generations map[string]string
}

// newIndependenceFixture has three live agents: a1 reports, a2 is a peer that
// reports too, and a3 is the target of unrelated host actions.
func newIndependenceFixture(t *testing.T) *independenceFixture {
	t.Helper()
	const runtimeID = "33333333-3333-4333-8333-333333333333"
	sessions := []store.Session{
		{ID: "a1", RootID: "root", Agent: "claude", ClaudeID: runtimeID},
		{ID: "a2", RootID: "root", Agent: "claude", ClaudeID: runtimeID},
		{ID: "a3", RootID: "root", Agent: "claude", ClaudeID: runtimeID},
	}
	d, runtime, principals := sessionRuntimeFixture(t, sessions...)
	d.sessionRPC = runtime
	t.Cleanup(runtime.completions.close)
	d.engine = newFakeEngine()
	f := &independenceFixture{d: d, runtime: runtime, principals: principals, generations: map[string]string{}}
	for _, s := range sessions {
		f.generations[s.ID] = publishTestRuntime(t, d, s.ID)
	}
	return f
}

func (f *independenceFixture) report(t *testing.T, id string) sessionrpc.DispatchResult {
	result, err := f.runtime.dispatch(context.Background(), reportRequest(f.principals[id], sessionreport.Activity, map[string]string{
		sessionreport.FieldRuntimeGeneration: f.generations[id], sessionreport.FieldState: core.StateRunning,
	}))
	if err != nil {
		t.Errorf("report %s: %v", id, err)
	}
	return result
}

// holdPeerReport is session a2's report stalling inside its own write.
func holdPeerReport(t *testing.T, f *independenceFixture, held func(), release <-chan struct{}) {
	f.runtime.beforeReportEffect = func(id string) {
		if id == "a2" {
			held()
			<-release
		}
	}
	if result := f.report(t, "a2"); result.Status != sessionrpc.StatusOK {
		t.Errorf("peer report = %+v", result)
	}
}

// holdHostAction is a host `start` of a3 that stalls while resolving the
// agents to start, through the real host action admission path.
func holdHostAction(t *testing.T, f *independenceFixture, held func(), release <-chan struct{}) {
	f.d.agentsUnder = func(id string) ([]string, error) {
		held()
		<-release
		return nil, nil
	}
	f.d.handle(context.Background(), core.Action{Action: core.ActionStart, ID: "a3"})
}

// holdReconcile is a session RPC reconcile pass stalling partway through.
func holdReconcile(t *testing.T, f *independenceFixture, held func(), release <-chan struct{}) {
	var once sync.Once
	f.runtime.progress = func(done, total int) {
		once.Do(func() {
			held()
			<-release
		})
	}
	t.Cleanup(f.runtime.close)
	if err := f.runtime.reconcile(context.Background()); err != nil {
		t.Errorf("reconcile: %v", err)
	}
}
