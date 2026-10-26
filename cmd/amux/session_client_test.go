package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/sessionreport"
	"amux/internal/sessionrpc"
)

type fakeRestrictedRPC struct {
	actionCalls int
	queryCalls  int
	actions     []sessionrpc.Action
	queries     []sessionrpc.Query
	action      sessionrpc.Result
	actionErr   error
	query       sessionrpc.Result
	queryErr    error
	lastQuery   sessionrpc.Query
}

func (f *fakeRestrictedRPC) Action(_ context.Context, action sessionrpc.Action) (sessionrpc.Result, error) {
	f.actionCalls++
	f.actions = append(f.actions, action)
	return f.action, f.actionErr
}
func (f *fakeRestrictedRPC) Query(_ context.Context, query sessionrpc.Query) (sessionrpc.Result, error) {
	f.queryCalls++
	f.queries = append(f.queries, query)
	f.lastQuery = query
	return f.query, f.queryErr
}
func (*fakeRestrictedRPC) Close() error { return nil }

func installRestrictedRPC(t *testing.T, client restrictedSessionRPC) {
	t.Helper()
	oldRestricted := sessionContextRestricted
	oldOpen := openRestrictedSessionRPC
	oldCursorDir := eventCursorDir
	cursorDir := t.TempDir()
	eventCursorDir = func() string { return cursorDir }
	oldStatusLine := startStatusLineReport
	sessionContextRestricted = func() bool { return true }
	openRestrictedSessionRPC = func() (restrictedSessionRPC, error) { return client, nil }
	// Run handed-off status-line reports inline so tests observe them.
	startStatusLineReport = reportStatusLine
	t.Cleanup(func() {
		sessionContextRestricted = oldRestricted
		openRestrictedSessionRPC = oldOpen
		startStatusLineReport = oldStatusLine
		eventCursorDir = oldCursorDir
	})
}

func TestSendActionUsesFixedContextRPCWithoutHostDial(t *testing.T) {
	body, err := json.Marshal(core.Result{Type: "result", OK: true, NewID: "created"})
	if err != nil {
		t.Fatal(err)
	}
	rpc := &fakeRestrictedRPC{action: sessionrpc.Result{Status: sessionrpc.StatusOK, Body: body}}
	installRestrictedRPC(t, rpc)
	oldDial := dial
	dial = func() (*daemon.Client, error) {
		t.Fatal("restricted action attempted host socket dial")
		return nil, nil
	}
	t.Cleanup(func() { dial = oldDial })
	id, err := sendActionID(core.Action{Action: core.ActionAddAgent, ID: "root", Fields: map[string]string{"agent": "codex"}})
	if err != nil || id != "created" || rpc.actionCalls != 1 {
		t.Fatalf("restricted action id=%q calls=%d err=%v", id, rpc.actionCalls, err)
	}
}

func TestRestrictedUncertainMutationIsNotRetried(t *testing.T) {
	uncertain := errors.Join(sessionrpc.ErrIndeterminate, errors.New("response lost"))
	rpc := &fakeRestrictedRPC{actionErr: uncertain}
	installRestrictedRPC(t, rpc)
	if _, err := sendActionID(core.Action{Action: core.ActionRename, ID: "a1", Fields: map[string]string{"name": "new"}}); !errors.Is(err, sessionrpc.ErrIndeterminate) {
		t.Fatalf("uncertain result = %v", err)
	}
	if rpc.actionCalls != 1 {
		t.Fatalf("uncertain mutation calls = %d, want exactly 1", rpc.actionCalls)
	}
}

func TestRestrictedQueryUsesNormalizedRPCBody(t *testing.T) {
	body := []byte(`[{"id":"a1","cwd":""}]`)
	rpc := &fakeRestrictedRPC{query: sessionrpc.Result{Status: sessionrpc.StatusOK, Body: body}}
	installRestrictedRPC(t, rpc)
	var rows []core.Session
	if err := queryRows(core.QuerySnapshot, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "a1" || rows[0].Cwd != "" || rpc.queryCalls != 1 {
		t.Fatalf("rows=%+v calls=%d", rows, rpc.queryCalls)
	}
}

func TestRestrictedReportQueriesAndEchoesCurrentGeneration(t *testing.T) {
	body, _ := json.Marshal(core.Result{Type: "result", OK: true})
	rpc := &fakeRestrictedRPC{
		query:  sessionrpc.Result{Status: sessionrpc.StatusOK, Body: []byte(`{"runtime_generation":"opaque-current"}`)},
		action: sessionrpc.Result{Status: sessionrpc.StatusOK, Body: body},
	}
	installRestrictedRPC(t, rpc)
	if err := restrictedReport(sessionreport.Activity, map[string]string{sessionreport.FieldState: core.StateReady}); err != nil {
		t.Fatal(err)
	}
	if len(rpc.queries) != 1 || rpc.queries[0].Verb != sessionreport.Context || len(rpc.actions) != 1 {
		t.Fatalf("report calls queries=%+v actions=%+v", rpc.queries, rpc.actions)
	}
	if got := rpc.actions[0].Fields[sessionreport.FieldRuntimeGeneration]; got != "opaque-current" {
		t.Fatalf("runtime generation = %q", got)
	}
}

func TestRepoShortcutCreatesAndStartsCoordinatorWorkgroup(t *testing.T) {
	body, _ := json.Marshal(core.Result{Type: "result", OK: true, NewID: "group"})
	rpc := &fakeRestrictedRPC{action: sessionrpc.Result{Status: sessionrpc.StatusOK, Body: body}}
	installRestrictedRPC(t, rpc)
	if err := cmdSession([]string{"repo", "api", "--name", "fix", "--prompt", "repair it", "--coordinator", "claude"}); err != nil {
		t.Fatal(err)
	}
	if len(rpc.actions) != 2 {
		t.Fatalf("actions: %+v", rpc.actions)
	}
	create, start := rpc.actions[0], rpc.actions[1]
	if create.Verb != core.ActionCreateWorkspace || create.ID != "" || create.Fields["repos"] != "api" || create.Fields["coordinator"] != "claude" || create.Fields["prompt"] != "repair it" || create.Fields["name"] != "fix" {
		t.Fatalf("repo shortcut did not use normal creation: %+v", create)
	}
	if start.Verb != core.ActionStart || start.ID != "group" {
		t.Fatalf("start targeted wrong session: %+v", start)
	}
}

// unservedThenServedRPC fails the report-context query as an unpublished
// mailbox (no service record) a few times, then answers normally.
type unservedThenServedRPC struct {
	fakeRestrictedRPC
	unserved int
}

func (u *unservedThenServedRPC) Query(ctx context.Context, query sessionrpc.Query) (sessionrpc.Result, error) {
	if u.unserved > 0 {
		u.unserved--
		u.queryCalls++
		return sessionrpc.Result{}, fmt.Errorf("read service: %w", os.ErrNotExist)
	}
	return u.fakeRestrictedRPC.Query(ctx, query)
}

// TestReportWaitsForUnservedMailbox pins that a self report made before the
// daemon serves a new session's mailbox is delivered once it is, instead of
// being dropped (which left fresh members "unknown" for their first turn).
func TestReportWaitsForUnservedMailbox(t *testing.T) {
	result, _ := json.Marshal(core.Result{Type: "result", OK: true})
	rpc := &unservedThenServedRPC{unserved: 2, fakeRestrictedRPC: fakeRestrictedRPC{
		query:  sessionrpc.Result{Status: sessionrpc.StatusOK, Body: []byte(`{"runtime_generation":"gen-1"}`)},
		action: sessionrpc.Result{Status: sessionrpc.StatusOK, Body: result},
	}}
	installRestrictedRPC(t, rpc)
	if err := restrictedReport(sessionreport.Activity, map[string]string{sessionreport.FieldState: core.StateRunning}); err != nil {
		t.Fatalf("report after mailbox publication = %v", err)
	}
	if rpc.queryCalls != 3 || len(rpc.actions) != 1 || rpc.actions[0].Fields[sessionreport.FieldState] != core.StateRunning {
		t.Fatalf("queries %d, actions %+v", rpc.queryCalls, rpc.actions)
	}
}
