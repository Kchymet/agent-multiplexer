package daemon

import (
	"context"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/sessionrpc"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func (d *Daemon) publishRows(rows []core.Session) {
	d.mu.Lock()
	d.sessions = rows
	d.mu.Unlock()
	d.broadcast()
}

func memberRow(state string, prs ...harnessproto.SessionPR) core.Session {
	row := core.Session{ID: "m1", RootID: "wg", Section: core.SectionWorkgroups, State: state, Status: state}
	if len(prs) > 0 {
		list := harnessproto.SessionPRs(prs)
		row.PRs = &list
	}
	return row
}

// A wait parks on the rail broadcast and answers the moment the member's
// state changes; a status-only change does not wake it.
func TestWaitSessionReturnsOnChange(t *testing.T) {
	d := New("", nil, time.Hour)
	d.publishRows([]core.Session{memberRow(core.StateRunning)})
	got := make(chan core.WaitResult, 1)
	go func() {
		res, err := d.waitSession(context.Background(), "m1", "", 10*time.Second)
		if err != nil {
			t.Error(err)
		}
		got <- res
	}()
	// Wait until the waiter has subscribed, then publish a cosmetic change.
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.subsMu.Lock()
		n := len(d.subs)
		d.subsMu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("waiter never subscribed")
		}
		time.Sleep(time.Millisecond)
	}
	cosmetic := memberRow(core.StateRunning)
	cosmetic.Status = "running · ⚙ 1 config edit"
	d.publishRows([]core.Session{cosmetic})
	select {
	case res := <-got:
		t.Fatalf("status-only change woke the waiter: %+v", res)
	case <-time.After(50 * time.Millisecond):
	}
	d.publishRows([]core.Session{memberRow(core.StateReady)})
	select {
	case res := <-got:
		if !res.Changed || res.State != core.StateReady || !res.Present || res.Token == "" {
			t.Fatalf("result = %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter not woken by a state change")
	}
	d.subsMu.Lock()
	defer d.subsMu.Unlock()
	if len(d.subs) != 0 {
		t.Fatalf("waiter left %d subscriptions behind", len(d.subs))
	}
}

// A token from an earlier call answers at once when the session moved since,
// so a caller that waits again never misses a change between calls; timeout 0
// reads the current view; a timeout reports no change; cancellation returns.
func TestWaitSessionTokenTimeoutAndCancel(t *testing.T) {
	d := New("", nil, time.Hour)
	d.publishRows([]core.Session{memberRow(core.StateRunning)})
	first, err := d.waitSession(context.Background(), "m1", "", 0)
	if err != nil || first.Changed || first.State != core.StateRunning {
		t.Fatalf("timeout 0 = %+v, %v", first, err)
	}
	pr := harnessproto.SessionPR{Repo: "o/r", Number: 3, State: "open", Checks: "fail"}
	d.publishRows([]core.Session{memberRow(core.StateRunning, pr)})
	moved, err := d.waitSession(context.Background(), "m1", first.Token, 10*time.Second)
	if err != nil || !moved.Changed || len(moved.PRs) != 1 {
		t.Fatalf("PR change since token = %+v, %v", moved, err)
	}
	start := time.Now()
	same, err := d.waitSession(context.Background(), "m1", moved.Token, time.Second)
	if err != nil || same.Changed || time.Since(start) < time.Second {
		t.Fatalf("timeout = %+v, %v after %v", same, err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, err := d.waitSession(ctx, "m1", moved.Token, 10*time.Second); err == nil {
		t.Fatal("cancelled wait returned no error")
	}
	gone, err := d.waitSession(context.Background(), "missing", "", 0)
	if err != nil || gone.Present {
		t.Fatalf("missing session = %+v, %v", gone, err)
	}
}

func TestValidateWaitQueryAndLane(t *testing.T) {
	ok := []access.Request{
		{Route: access.RouteQuery, Verb: core.QueryWait, ID: "m1"},
		{Route: access.RouteQuery, Verb: core.QueryWait, ID: "m1", Fields: map[string]string{core.WaitSinceField: "abc", core.WaitTimeoutField: "25"}},
		{Route: access.RouteQuery, Verb: core.QueryWait, ID: "m1", Fields: map[string]string{core.WaitTimeoutField: "0"}},
	}
	for _, req := range ok {
		if _, err := canonicalSessionOperation(req); err != nil {
			t.Errorf("%+v: %v", req, err)
		}
	}
	bad := []access.Request{
		{Route: access.RouteQuery, Verb: core.QueryWait},
		{Route: access.RouteQuery, Verb: core.QueryWait, ID: "m1", Fields: map[string]string{core.WaitTimeoutField: "26"}},
		{Route: access.RouteQuery, Verb: core.QueryWait, ID: "m1", Fields: map[string]string{core.WaitTimeoutField: "-1"}},
		{Route: access.RouteQuery, Verb: core.QueryWait, ID: "m1", Fields: map[string]string{core.WaitTimeoutField: "01"}},
		{Route: access.RouteQuery, Verb: core.QueryWait, ID: "m1", Fields: map[string]string{"other": "x"}},
		{Route: access.RouteQuery, Verb: core.QueryWait, ID: "m1", Target: "t"},
	}
	for _, req := range bad {
		if _, err := canonicalSessionOperation(req); err == nil {
			t.Errorf("%+v accepted", req)
		}
	}
	wait := sessionrpc.Query{Verb: core.QueryWait, ID: "m1"}
	if call := (sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: wait.Verb, ID: wait.ID}); !sessionCallParallel(call) {
		t.Fatal("wait is not on the parallel lane")
	}
	if sessionCallParallel(sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: core.QueryRuntimeEvents, ID: "m1"}) {
		t.Fatal("runtime events moved to the parallel lane")
	}
}
