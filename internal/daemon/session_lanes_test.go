package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/centralauth"
	"amux/internal/core"
	"amux/internal/credentialbroker"
	"amux/internal/sessionreport"
	"amux/internal/sessionrpc"
)

func TestSessionCallLanes(t *testing.T) {
	op := func(route access.Route, verb string) sessionrpc.Call {
		return sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: route, Verb: verb}
	}
	fast := []sessionrpc.Call{
		op(access.RouteQuery, sessionreport.Context),
		op(access.RouteAction, sessionreport.Activity),
		op(access.RouteAction, sessionreport.Model),
		op(access.RouteAction, sessionreport.Limits),
		op(access.RouteAction, sessionreport.PermissionRequest),
		op(access.RouteAction, sessionreport.PermissionResolved),
		op(access.RouteAction, sessionreport.PermissionClear),
		op(access.RouteQuery, core.QueryVersion),
		op(access.RouteQuery, core.QuerySessions),
		op(access.RouteQuery, core.QueryRepos),
		op(access.RouteQuery, core.QuerySnapshot),
		op(access.RouteQuery, centralauth.QueryVerb),
		op(access.RouteAction, credentialbroker.Read),
	}
	slow := []sessionrpc.Call{
		op(access.RouteAction, sessionreport.Capture),
		op(access.RouteQuery, core.QueryAgentSessions),
		op(access.RouteQuery, core.QueryRuntimeEvents),
		op(access.RouteAction, core.ActionAddAgent),
		op(access.RouteAction, core.ActionNewWorkgroup),
		op(access.RouteAction, core.ActionRestart),
		op(access.RouteAction, core.ActionStart),
		op(access.RouteAction, core.ActionSteer),
		op(access.RouteAction, core.ActionSetArchived),
		op(access.RouteAction, "some-future-verb"),
	}
	for _, call := range fast {
		if sessionCallSlow(call) {
			t.Errorf("%s %s is on the slow lane", call.Route, call.Verb)
		}
	}
	for _, call := range slow {
		if !sessionCallSlow(call) {
			t.Errorf("%s %s is on the fast lane", call.Route, call.Verb)
		}
	}
}

// A mailbox whose slow lane is dispatching must close on its own goroutine:
// Close joins the lane, whose dispatch may need the locks the closer holds.
func TestClosingSubjectWithBusySlowLaneDefersClose(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int32
	f := newServingFixture(t, map[string]func() sessionrpc.DispatchResult{
		"busy": func() sessionrpc.DispatchResult {
			started.Add(1)
			<-release
			return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK}
		},
	}, "busy")
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	publishDaemonCall(t, f.authority, f.grants["busy"], sessionrpc.Call{
		Kind: sessionrpc.CallOperation, Route: access.RouteAction, Verb: core.ActionAddAgent,
	})
	if err := f.runtime.serveCurrent(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "slow dispatch to start", func() bool { return started.Load() == 1 })
	f.runtime.waitPasses() // the pass handed the call to the lane and returned
	if !f.runtime.servers["busy"].Busy() {
		t.Fatal("slow lane idle while its dispatch runs")
	}
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
		t.Fatal("closeServer blocked on a busy slow lane")
	}
	if !f.runtime.retiring("busy") {
		t.Fatal("mailbox may be reopened while its slow lane still owns a claimed request")
	}
	close(release)
	f.runtime.waitPasses()
	if f.runtime.retiring("busy") {
		t.Fatal("retirement did not finish after the slow lane drained")
	}
}

// With the production lanes, a session's query is served while its own
// add-agent is still dispatching.
func TestSessionQueryServedWhileOwnActionRuns(t *testing.T) {
	release := make(chan struct{})
	var slowStarted, fastServed atomic.Int32
	f := newServingFixture(t, map[string]func() sessionrpc.DispatchResult{
		"a": func() sessionrpc.DispatchResult {
			if slowStarted.Load() == 0 {
				slowStarted.Add(1)
				<-release
			} else {
				fastServed.Add(1)
			}
			return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK}
		},
	}, "a")
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	serveOnWakesOnly(t, f)
	publishDaemonCall(t, f.authority, f.grants["a"], sessionrpc.Call{
		Kind: sessionrpc.CallOperation, Route: access.RouteAction, Verb: core.ActionAddAgent,
	})
	waitFor(t, "add-agent to start", func() bool { return slowStarted.Load() == 1 })
	start := time.Now()
	publishDaemonBudgetCall(t, f.authority, f.grants["a"])
	waitFor(t, "query served behind the running add-agent", func() bool { return fastServed.Load() == 1 })
	t.Logf("query served in %v while add-agent ran", time.Since(start))
	close(release)
}
