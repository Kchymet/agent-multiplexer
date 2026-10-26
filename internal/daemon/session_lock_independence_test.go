package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/credentialbroker"
	"amux/internal/engine"
	"amux/internal/panespec"
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
		// denied is set when the holder archives a1 itself, so the report that
		// waited for it is then refused.
		denied bool
	}{
		{name: "peer session report", hold: holdPeerReport, blocked: false},
		{name: "host action on another session", hold: holdHostAction, blocked: false},
		{name: "workgroup start, stalled resolving its members", hold: holdWorkgroupAction, blocked: false},
		{name: "archive of a3, stalled stopping its runtime", hold: holdArchive(core.ActionSetArchived, "a3"), blocked: false},
		{name: "one-key archive toggle of a3 (exclusive), stalled stopping its runtime", hold: holdArchive(core.ActionArchive, "a3"), blocked: true},
		{name: "archive of a1's workgroup, stalled stopping a runtime", hold: holdArchive(core.ActionSetArchived, "root"), blocked: true, denied: true},
		{name: "exclusive action queued behind a slow session action", hold: holdQueuedExclusive, blocked: false},
		{name: "host start of a3, stalled launching its runtime", hold: holdRuntimeStart, blocked: false},
		{name: "host agent-set-repos of a3, stalled in its git work", hold: holdDispatch(core.Action{Action: core.ActionAgentSetRepos, ID: "a3", Fields: map[string]string{"repos": ""}}), blocked: false},
		{name: "host add-repo, stalled cloning", hold: holdDispatch(core.Action{Action: core.ActionAddRepo, Fields: map[string]string{"source": "https://example.invalid/r.git"}}), blocked: false},
		{name: "peer session credential helper", hold: holdPeerCredential, blocked: false},
		{name: "workgroup creation, stalled launching its coordinator", hold: holdCreation, blocked: false},
		{name: "reconcile pass", hold: holdReconcile, blocked: false},
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
			want := sessionrpc.StatusOK
			if tc.denied {
				want = sessionrpc.StatusDenied
			}
			if result.Status != want {
				t.Fatalf("report = %+v, want %s", result, want)
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
	ghConfig := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", ghConfig)
	if err := os.WriteFile(filepath.Join(ghConfig, "hosts.yml"), []byte("github.com:\n  user: selected\n"), 0600); err != nil {
		t.Fatal(err)
	}
	const runtimeID = "33333333-3333-4333-8333-333333333333"
	sessions := []store.Session{
		{ID: "root", Agent: "claude"},
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

// holdWorkgroupAction is a host `start` of the workgroup root, which starts
// every member under the members' locks.
func holdWorkgroupAction(t *testing.T, f *independenceFixture, held func(), release <-chan struct{}) {
	f.d.agentsUnder = func(id string) ([]string, error) {
		held()
		<-release
		return nil, nil
	}
	f.d.handle(context.Background(), core.Action{Action: core.ActionStart, ID: "root"})
}

// holdQueuedExclusive is a slow host action on a3 with a workgroup action
// queued behind it for the exclusive gate. A writer parked in
// sync.RWMutex.Lock would stall every new shared holder; the queued one must
// not.
func holdQueuedExclusive(t *testing.T, f *independenceFixture, held func(), release <-chan struct{}) {
	sessionHeld := make(chan struct{})
	var once sync.Once
	f.d.agentsUnder = func(id string) ([]string, error) {
		once.Do(func() { close(sessionHeld) })
		<-release
		return nil, nil
	}
	actions := make(chan struct{}, 2)
	go func() {
		f.d.handle(context.Background(), core.Action{Action: core.ActionStart, ID: "a3"})
		actions <- struct{}{}
	}()
	<-sessionHeld
	go func() {
		f.d.handle(context.Background(), core.Action{Action: core.ActionStart, ID: "root"})
		actions <- struct{}{}
	}()
	// Give the workgroup action time to reach the gate and start waiting.
	time.Sleep(50 * time.Millisecond)
	held()
	<-actions
	<-actions
}

// holdRuntimeStart is a host `start` of a3 stalling inside the runtime
// launch itself (Engine.Ensure), where the permission gate publishes a3's new
// generation.
func holdRuntimeStart(t *testing.T, f *independenceFixture, held func(), release <-chan struct{}) {
	eng := f.d.engine.(*fakeEngine)
	eng.ensurePublished = func(engine.Instance) {
		held()
		<-release
	}
	f.d.launchSpec = testLaunchSpecResolver
	f.d.resolve = func(panespec.LaunchSpec, int) (string, []string, []string, error) {
		return "", nil, []string{"sh"}, nil
	}
	f.d.agentsUnder = func(id string) ([]string, error) { return []string{id}, nil }
	if r := f.d.handle(context.Background(), core.Action{Action: core.ActionStart, ID: "a3"}); !r.OK {
		t.Errorf("start = %+v", r)
	}
}

// holdDispatch runs a store-backed host action that stalls inside its own
// work (the wsops dispatch the daemon calls).
func holdDispatch(action core.Action) func(t *testing.T, f *independenceFixture, held func(), release <-chan struct{}) {
	return func(t *testing.T, f *independenceFixture, held func(), release <-chan struct{}) {
		f.d.dispatch = func(context.Context, core.Action, func(string)) (string, error) {
			held()
			<-release
			return "", nil
		}
		if r := f.d.handle(context.Background(), action); !r.OK {
			t.Errorf("%s = %+v", action.Action, r)
		}
	}
}

// holdPeerCredential is a2's GitHub credential read stalling inside its
// helper process.
func holdPeerCredential(t *testing.T, f *independenceFixture, held func(), release <-chan struct{}) {
	f.runtime.credentialOperation = func(context.Context, credentialbroker.Operation) (credentialbroker.Result, error) {
		held()
		<-release
		return credentialbroker.Result{Value: "synthetic"}, nil
	}
	o := credentialbroker.Operation{Verb: credentialbroker.Read, Service: "gh:github.com", Account: credentialbroker.GitHubAccount}
	call := sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: o.Verb, Fields: o.Fields()}
	if result, _ := f.runtime.dispatch(context.Background(), sessionrpc.DispatchRequest{Principal: f.principals["a2"], Call: call}); result.Status != sessionrpc.StatusOK {
		t.Errorf("peer credential = %+v", result)
	}
}

// holdCreation is a host `new-workgroup` whose coordinator launch stalls.
func holdCreation(t *testing.T, f *independenceFixture, held func(), release <-chan struct{}) {
	f.d.launchSpec = func(ctx context.Context, id string) (panespec.LaunchSpec, error) {
		held()
		<-release
		return testLaunchSpecResolver(ctx, id)
	}
	f.d.resolve = func(panespec.LaunchSpec, int) (string, []string, []string, error) {
		return "", nil, []string{"sh"}, nil
	}
	if r := f.d.handle(context.Background(), core.Action{Action: core.ActionNewWorkgroup, Fields: map[string]string{"name": "created"}}); !r.OK {
		t.Errorf("new workgroup = %+v", r)
	}
}

// holdArchive archives id with verb, stalling while it stops a3's runtime.
// An explicit archive runs under the archived sessions' locks; the one-key
// toggle, which may restore, still takes the exclusive gate.
func holdArchive(verb, id string) func(t *testing.T, f *independenceFixture, held func(), release <-chan struct{}) {
	return func(t *testing.T, f *independenceFixture, held func(), release <-chan struct{}) {
		eng := f.d.engine.(*fakeEngine)
		eng.running("a3")
		var once sync.Once
		eng.killObserved = func(engine.Instance) {
			once.Do(func() {
				held()
				<-release
			})
		}
		action := core.Action{Action: verb, ID: id}
		if verb == core.ActionSetArchived {
			action.Fields = map[string]string{"archived": "true"}
		}
		if r := f.d.handle(context.Background(), action); !r.OK {
			t.Errorf("archive = %+v", r)
		}
	}
}

// holdReconcile is a session RPC reconcile pass stalling while it opens a3's
// mailbox, inside a3's critical section.
func holdReconcile(t *testing.T, f *independenceFixture, held func(), release <-chan struct{}) {
	f.d.launchSpec = func(_ context.Context, id string) (panespec.LaunchSpec, error) {
		if id == "a3" {
			held()
			<-release
		}
		return panespec.LaunchSpec{}, errors.New("test: no launch spec")
	}
	t.Cleanup(f.runtime.close)
	if err := f.runtime.reconcile(context.Background()); err != nil {
		t.Errorf("reconcile: %v", err)
	}
}

// TestSessionHostActionExcludesThatSessionsReports: a host action on a1 runs
// under a1's lock, so a1's own report waits for it.
func TestSessionHostActionExcludesThatSessionsReports(t *testing.T) {
	f := newIndependenceFixture(t)
	release := make(chan struct{})
	held := make(chan struct{})
	f.d.agentsUnder = func(id string) ([]string, error) {
		close(held)
		<-release
		return nil, nil
	}
	actionDone := make(chan struct{})
	go func() {
		defer close(actionDone)
		f.d.handle(context.Background(), core.Action{Action: core.ActionStart, ID: "a1"})
	}()
	<-held
	reported := make(chan sessionrpc.DispatchResult, 1)
	go func() { reported <- f.report(t, "a1") }()
	select {
	case result := <-reported:
		t.Fatalf("a1 reported while a host action held a1: %+v", result)
	case <-time.After(250 * time.Millisecond):
	}
	close(release)
	<-actionDone
	if result := <-reported; result.Status != sessionrpc.StatusOK {
		t.Fatalf("report = %+v", result)
	}
}

// TestReconcileTickDefersOpensDuringCreation: a session row a creation is
// still building must not have its mailbox adopted mid-creation.
func TestReconcileTickDefersOpensDuringCreation(t *testing.T) {
	f := newIndependenceFixture(t)
	t.Cleanup(f.runtime.close)
	var mu sync.Mutex
	opened := map[string]bool{}
	f.d.launchSpec = func(_ context.Context, id string) (panespec.LaunchSpec, error) {
		mu.Lock()
		opened[id] = true
		mu.Unlock()
		return panespec.LaunchSpec{}, errors.New("test: no launch spec")
	}
	f.d.creationMu.Lock()
	if err := f.runtime.reconcileTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	during := len(opened)
	mu.Unlock()
	f.d.creationMu.Unlock()
	if during != 0 {
		t.Fatalf("reconcile opened %v during a creation", opened)
	}
	if err := f.runtime.reconcileTick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !opened["a1"] {
		t.Fatal("reconcile did not open a1 after the creation")
	}
}

// TestWorkgroupStartStartsOnlyItsLockedMembers: a member added by a
// concurrent creation after the start resolved its members is not started
// by it (outside that member's lock); its creation starts it.
func TestWorkgroupStartStartsOnlyItsLockedMembers(t *testing.T) {
	f := newIndependenceFixture(t)
	eng := f.d.engine.(*fakeEngine)
	f.d.launchSpec = testLaunchSpecResolver
	f.d.resolve = func(panespec.LaunchSpec, int) (string, []string, []string, error) {
		return "", nil, []string{"sh"}, nil
	}
	var mu sync.Mutex
	calls := 0
	f.d.agentsUnder = func(string) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return []string{"a3"}, nil
		}
		return []string{"a3", "added-later"}, nil
	}
	if r := f.d.handle(context.Background(), core.Action{Action: core.ActionStart, ID: "root"}); !r.OK {
		t.Fatalf("workgroup start = %+v", r)
	}
	if keys := eng.ensuredKeys(); len(keys) != 1 || keys[0].AgentID != "a3" {
		t.Fatalf("workgroup start launched %v, want only its locked member a3", keys)
	}
}
