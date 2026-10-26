package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/sessionrpc"
)

// serveOnWakesOnly runs the serving loop with ticks that never fire, so only
// request wakes and pass ends can start a pass.
func serveOnWakesOnly(t *testing.T, f *servingFixture) {
	t.Helper()
	for id, server := range f.runtime.servers {
		if !server.Watched() {
			t.Skipf("platform cannot watch %s's mailbox", id)
		}
	}
	f.runtime.passDone = make(chan passEnd, sessionWakeQueue)
	if err := f.runtime.serveCurrent(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.runtime.awaitTick(ctx, nil, nil)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func TestRequestServedOnArrival(t *testing.T) {
	var served atomic.Int32
	f := newServingFixture(t, map[string]func() sessionrpc.DispatchResult{
		"a": func() sessionrpc.DispatchResult {
			served.Add(1)
			return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK}
		},
	}, "a")
	serveOnWakesOnly(t, f)
	for i := int32(1); i <= 3; i++ {
		start := time.Now()
		publishDaemonBudgetCall(t, f.authority, f.grants["a"])
		waitFor(t, "request served on its wake", func() bool { return served.Load() == i })
		t.Logf("request %d served %v after publication", i, time.Since(start))
	}
}

// A request that lands while its subject's pass is dispatching can be listed
// too late for that pass; the pass's end must serve it without a tick.
func TestRequestDuringPassServedWhenPassEnds(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int32
	f := newServingFixture(t, map[string]func() sessionrpc.DispatchResult{
		"a": func() sessionrpc.DispatchResult {
			if started.Add(1) == 1 {
				<-release
			}
			return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK}
		},
	}, "a")
	serveOnWakesOnly(t, f)
	publishDaemonBudgetCall(t, f.authority, f.grants["a"])
	waitFor(t, "first dispatch to start", func() bool { return started.Load() == 1 })
	publishDaemonBudgetCall(t, f.authority, f.grants["a"])
	time.Sleep(50 * time.Millisecond) // let the wake arrive mid-pass
	close(release)
	waitFor(t, "request published mid-pass served", func() bool { return started.Load() == 2 })
}
