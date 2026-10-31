package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/ghpr"
	"amux/internal/sessionrpc"
	"amux/internal/store"
	"amux/internal/wsops"
)

// An agent's `amux agent done` with an open PR is refused with a stable code
// and a body carrying the PR URL, and nothing is archived.
func TestSelfCompletionRefusedByOpenPR(t *testing.T) {
	session := store.Session{
		ID: "a1", RootID: "root1", Agent: "claude",
		Dir: t.TempDir(), ClaudeID: "pr-guard",
	}
	_, runtime, principals := sessionRuntimeFixture(t, session)
	t.Cleanup(runtime.completions.close)
	applied := false
	runtime.applyResult = func(context.Context, core.Action) (string, error) {
		applied = true
		return "", nil
	}
	runtime.guardOpenPRs = func(_ context.Context, a core.Action) error {
		return &wsops.OpenPRError{Verb: "archive", ID: a.ID, PRs: []wsops.OwnedPR{{
			SessionID: a.ID, PR: ghpr.PR{Number: 5, URL: "https://github.com/o/r/pull/5", HeadRefName: "amux/root1-a1"},
		}}}
	}
	result, err := runtime.dispatch(context.Background(), sessionrpc.DispatchRequest{
		Principal: principals[session.ID],
		RequestID: "0123456789abcdef0123456789abcdef",
		Call: sessionrpc.Call{
			Kind: sessionrpc.CallOperation, Route: access.RouteAction,
			Verb: core.ActionSetArchived, ID: session.ID,
			Fields: map[string]string{"archived": "true"},
		},
	})
	if err != nil || result.Status != sessionrpc.StatusFailed || result.Code != codeOpenPullRequests || result.Receipt != nil {
		t.Fatalf("result = %+v err=%v", result, err)
	}
	var body core.Result
	if err := json.Unmarshal(result.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.OK || !strings.Contains(body.Error, "https://github.com/o/r/pull/5") {
		t.Fatalf("body = %+v", body)
	}
	if applied {
		t.Fatal("refused completion still archived")
	}
	if runtime.completions.has(session.ID) {
		t.Fatal("refused completion began a completion")
	}
}

func TestSelfCompletionCannotForce(t *testing.T) {
	session := store.Session{
		ID: "a1", RootID: "root1", Agent: "claude",
		Dir: t.TempDir(), ClaudeID: "pr-guard-force",
	}
	_, runtime, principals := sessionRuntimeFixture(t, session)
	t.Cleanup(runtime.completions.close)
	runtime.applyResult = func(context.Context, core.Action) (string, error) {
		t.Fatal("forced self-completion reached the store")
		return "", nil
	}
	result, _ := runtime.dispatch(context.Background(), sessionrpc.DispatchRequest{
		Principal: principals[session.ID],
		RequestID: "0123456789abcdef0123456789abcdef",
		Call: sessionrpc.Call{
			Kind: sessionrpc.CallOperation, Route: access.RouteAction,
			Verb: core.ActionSetArchived, ID: session.ID,
			Fields: map[string]string{"archived": "true", wsops.FieldForce: "true"},
		},
	})
	if result.Status != sessionrpc.StatusDenied {
		t.Fatalf("forced self-completion = %+v", result)
	}
}
