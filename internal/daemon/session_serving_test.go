package daemon

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/sessionrpc"
)

type servingFixture struct {
	authority *access.FileAuthority
	runtime   *sessionRuntime
	grants    map[string]access.SessionAccess
}

// newServingFixture opens one real mailbox server per id. dispatch decides how
// each subject's request is handled.
func newServingFixture(t *testing.T, dispatch map[string]func() sessionrpc.DispatchResult, ids ...string) *servingFixture {
	t.Helper()
	authority, err := access.Open(filepath.Join(t.TempDir(), "authority"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { authority.Close() })
	f := &servingFixture{
		authority: authority,
		runtime: &sessionRuntime{servers: map[string]*sessionrpc.Server{}, initialized: map[string]bool{},
			wakes: make(chan string, sessionWakeQueue)},
		grants: map[string]access.SessionAccess{},
	}
	for _, id := range ids {
		dir := filepath.Join(t.TempDir(), id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		grant, err := authority.EnsureSession(context.Background(), id, dir)
		if err != nil {
			t.Fatal(err)
		}
		handle := dispatch[id]
		server, err := sessionrpc.OpenServerMailbox(id, grant.MailboxHostDir, authority, authority,
			sessionrpc.Callbacks{
				Authorize: func(context.Context, access.Principal, sessionrpc.Call) error { return nil },
				Dispatch: func(context.Context, sessionrpc.DispatchRequest) (sessionrpc.DispatchResult, error) {
					return handle(), nil
				},
			}, sessionrpc.ServerOptions{RequestNotify: f.runtime.notifier(id), Slow: sessionCallSlow})
		if err != nil {
			t.Fatal(err)
		}
		f.runtime.servers[id] = server
		f.grants[id] = grant
	}
	t.Cleanup(func() {
		f.runtime.waitPasses()
		for id := range f.runtime.servers {
			f.runtime.closeServer(id)
		}
	})
	return f
}

// A request that takes a long time to dispatch for one subject (agent
// discovery over every transcript on the host, a large event page, a slow
// action) used to stall the single serving loop, so every other agent's
// requests sat unclaimed until their 35s client deadline. Each mailbox now has
// its own pass.
func TestSlowSubjectDispatchDoesNotDelayOtherMailboxes(t *testing.T) {
	release := make(chan struct{})
	var slowStarted, fastServed atomic.Int32
	f := newServingFixture(t, map[string]func() sessionrpc.DispatchResult{
		"a-slow": func() sessionrpc.DispatchResult {
			slowStarted.Add(1)
			<-release
			return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK}
		},
		"b-fast": func() sessionrpc.DispatchResult {
			fastServed.Add(1)
			return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK}
		},
	}, "a-slow", "b-fast")
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	publishDaemonBudgetCall(t, f.authority, f.grants["a-slow"])

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan struct{})
	go func() {
		defer close(ticks)
		for ctx.Err() == nil {
			if err := f.runtime.serveCurrent(ctx); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	waitFor(t, "slow dispatch to start", func() bool { return slowStarted.Load() == 1 })

	start := time.Now()
	publishDaemonBudgetCall(t, f.authority, f.grants["b-fast"])
	waitFor(t, "fast mailbox served while slow subject dispatches", func() bool { return fastServed.Load() == 1 })
	t.Logf("fast subject served in %v while another subject's dispatch was blocked", time.Since(start))
	if slowStarted.Load() != 1 {
		t.Fatalf("slow request dispatched %d times; a busy subject must not start a second pass", slowStarted.Load())
	}
	close(release)
	cancel()
	<-ticks
	f.runtime.waitPasses()
}

// Retiring a subject while its pass dispatches must not wait under the
// dispatch lock (the dispatch may need it), and the mailbox must not be
// reopened until the old server finished closing.
func TestClosingBusySubjectDefersCloseWithoutDeadlock(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int32
	f := newServingFixture(t, map[string]func() sessionrpc.DispatchResult{
		"busy": func() sessionrpc.DispatchResult {
			started.Add(1)
			<-release
			return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK}
		},
	}, "busy")
	publishDaemonBudgetCall(t, f.authority, f.grants["busy"])
	if err := f.runtime.serveCurrent(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "dispatch to start", func() bool { return started.Load() == 1 })

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		f.runtime.dispatchMu.Lock() // as reconcile holds it around closeAndRevoke
		defer f.runtime.dispatchMu.Unlock()
		f.runtime.closeServer("busy")
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closeServer blocked on an in-flight pass")
	}
	if _, ok := f.runtime.servers["busy"]; ok {
		t.Fatal("retired server still published")
	}
	if !f.runtime.retiring("busy") {
		t.Fatal("mailbox may be reopened while its old server still owns a claimed request")
	}
	close(release)
	f.runtime.waitPasses()
	if f.runtime.retiring("busy") {
		t.Fatal("retirement did not finish after the pass returned")
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
