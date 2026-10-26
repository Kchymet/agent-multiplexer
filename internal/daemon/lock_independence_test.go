package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/panespec"
	"amux/internal/sessionreport"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

// lockFixture is two live sessions, a and b, sharing one daemon. Each holder
// below starts a slow operation on b's behalf (or the daemon's), and the test
// measures whether a's ordinary activity report has to wait for it.
type lockFixture struct {
	d           *Daemon
	runtime     *sessionRuntime
	principals  map[string]access.Principal
	generations map[string]string
	// launch blocks d.launchSpec for session b until release is closed.
	launchEntered chan struct{}
	launchRelease chan struct{}
}

func newLockFixture(t *testing.T) *lockFixture {
	t.Helper()
	const runtimeID = "33333333-3333-4333-8333-333333333333"
	d, runtime, principals := sessionRuntimeFixture(t,
		store.Session{ID: "a", RootID: "root", Agent: "claude", ClaudeID: runtimeID},
		store.Session{ID: "b", RootID: "root", Agent: "claude", ClaudeID: runtimeID},
	)
	d.sessionRPC = runtime
	fx := &lockFixture{
		d: d, runtime: runtime, principals: principals, generations: map[string]string{},
		launchEntered: make(chan struct{}), launchRelease: make(chan struct{}),
	}
	for _, id := range []string{"a", "b"} {
		fx.generations[id] = publishTestRuntime(t, d, id)
	}
	var entered bool
	d.launchSpec = func(ctx context.Context, id string) (panespec.LaunchSpec, error) {
		if id != "b" {
			return panespec.LaunchSpec{}, errors.New("no launch in lock fixture")
		}
		if !entered {
			entered = true
			close(fx.launchEntered)
		}
		select {
		case <-fx.launchRelease:
		case <-ctx.Done():
		}
		return panespec.LaunchSpec{}, errors.New("launch released")
	}
	return fx
}

func (fx *lockFixture) report(id string) (sessionrpc.DispatchResult, error) {
	return fx.runtime.dispatch(context.Background(), reportRequest(fx.principals[id], sessionreport.Activity, map[string]string{
		sessionreport.FieldRuntimeGeneration: fx.generations[id], sessionreport.FieldState: core.StateRunning,
	}))
}

// lockHolder occupies one resource on b's (or the daemon's) behalf. start
// returns once the holder is inside its critical section; release lets it
// finish and waits for it.
type lockHolder struct {
	name  string
	start func(t *testing.T, fx *lockFixture) (release func())
	// blocksA records the current behaviour: whether a's report waits for this
	// holder. The lock-split work (docs/daemon-locking.md) flips each entry to
	// false as the resource moves out from under the daemon-wide gates.
	blocksA bool
}

var lockHolders = []lockHolder{
	{
		// Control: proves the harness can observe an unblocked report.
		name:    "nothing held",
		start:   func(*testing.T, *lockFixture) func() { return func() {} },
		blocksA: false,
	},
	{
		// Control: the account store's file lock is unrelated to activity.
		name: "account store lock alone",
		start: func(t *testing.T, fx *lockFixture) func() {
			return holdAccountStore(t)
		},
		blocksA: false,
	},
	{
		// A host restart of b holds dispatchMu+effectMu while it resolves b's
		// launch (and, in production, while it stops and respawns b).
		name: "host restart of b",
		start: func(t *testing.T, fx *lockFixture) func() {
			done := make(chan struct{})
			go func() {
				defer close(done)
				fx.d.handle(context.Background(), core.Action{Action: core.ActionRestart, ID: "b"})
			}()
			<-fx.launchEntered
			return func() { close(fx.launchRelease); <-done }
		},
		blocksA: true,
	},
	{
		// Reconcile visits every session under dispatchMu+effectMu; opening b's
		// mailbox resolves b's launch spec inside that one global pass.
		name: "reconcile opening b",
		start: func(t *testing.T, fx *lockFixture) func() {
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = fx.runtime.reconcile(context.Background())
			}()
			<-fx.launchEntered
			return func() { close(fx.launchRelease); <-done }
		},
		blocksA: true,
	},
	{
		// b's limits report waits on the account store while it holds the
		// daemon-wide gates, so a's unrelated report waits on b.
		name: "b's report waiting on the account store",
		start: func(t *testing.T, fx *lockFixture) func() {
			unlock := holdAccountStore(t)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = fx.runtime.dispatch(context.Background(), reportRequest(fx.principals["b"], sessionreport.Limits, map[string]string{
					sessionreport.FieldRuntimeGeneration: fx.generations["b"],
					sessionreport.FieldLimits:            `{"five_hour":{"used_percentage":10,"resets_at":1}}`,
				}))
			}()
			// Inside the gate once the effect lock is unavailable.
			waitFor(t, "b inside the effect gate", func() bool {
				if fx.d.effectMu.TryLock() {
					fx.d.effectMu.Unlock()
					return false
				}
				return true
			})
			return func() { unlock(); <-done }
		},
		blocksA: true,
	},
	{
		// Publishing b's replacement runtime holds the permission gate's single
		// mutex across runtime creation; every report reads its generation there.
		name: "b's runtime publication",
		start: func(t *testing.T, fx *lockFixture) func() {
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				_, _, _ = fx.d.publishPermissionRuntime("b", func() (any, error) {
					close(entered)
					<-release
					return &fakeInstance{}, nil
				})
			}()
			<-entered
			return func() { close(release); <-done }
		},
		blocksA: true,
	},
}

// TestSessionReportIndependence documents which daemon operations an
// unrelated session's report must wait for. It is the harness for the lock
// split: each PR that narrows a lock flips the matching blocksA entry.
func TestSessionReportIndependence(t *testing.T) {
	store.ShareHandles(true)
	defer store.ShareHandles(false)
	const blockedFor = 150 * time.Millisecond
	for _, holder := range lockHolders {
		t.Run(holder.name, func(t *testing.T) {
			fx := newLockFixture(t)
			release := holder.start(t, fx)
			released := false
			defer func() {
				if !released {
					release()
				}
			}()
			type outcome struct {
				result sessionrpc.DispatchResult
				err    error
			}
			reported := make(chan outcome, 1)
			go func() {
				result, err := fx.report("a")
				reported <- outcome{result, err}
			}()
			// Every holder holds until released, so an independent report finishes
			// while it is still held however slow the machine; a dependent one
			// must still be waiting after blockedFor.
			wait := 10 * time.Second
			if holder.blocksA {
				wait = blockedFor
			}
			var got outcome
			select {
			case got = <-reported:
				if holder.blocksA {
					t.Fatalf("a's report finished while %s was held; flip blocksA to false", holder.name)
				}
			case <-time.After(wait):
				if !holder.blocksA {
					t.Fatalf("a's report waited %v for %s", wait, holder.name)
				}
				release()
				released = true
				select {
				case got = <-reported:
				case <-time.After(10 * time.Second):
					t.Fatal("a's report never finished after release")
				}
			}
			if got.err != nil || got.result.Status != sessionrpc.StatusOK {
				t.Fatalf("a's report = %+v, %v", got.result, got.err)
			}
		})
	}
}

// holdAccountStore takes the account store's exclusive file lock, as a slow
// account update in another goroutine or process would.
func holdAccountStore(t *testing.T) (unlock func()) {
	t.Helper()
	dir := core.AuthDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "accounts.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	var unlocked bool
	unlock = func() {
		if unlocked {
			return
		}
		unlocked = true
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}
	t.Cleanup(unlock)
	return unlock
}
