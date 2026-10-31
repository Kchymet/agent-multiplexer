package daemon

import (
	"context"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

// cancelAfterPolicy answers the policy check, then ends the call's deadline:
// the deadline expires between admission and the self-completion lookup.
type cancelAfterPolicy struct {
	access.Resolver
	cancel context.CancelFunc
}

func (c cancelAfterPolicy) Lookup(ctx context.Context, id string) (access.Resource, bool, error) {
	defer c.cancel()
	return c.Resolver.Lookup(ctx, id)
}

// A callback deadline that expires while a dispatch waits for its admission
// is a known failure (nothing ran), never a policy denial or a claim that the
// caller's completion is not current.
func TestSessionRPCDeadlineBeforeEffectIsNotADenial(t *testing.T) {
	session := store.Session{
		ID: "a1", RootID: "root1", Agent: "claude",
		Dir: t.TempDir(), ClaudeID: "deadline-before-effect",
	}
	_, runtime, principals := sessionRuntimeFixture(t, session)
	t.Cleanup(runtime.completions.close)
	applied := false
	runtime.applyResult = func(context.Context, core.Action) (string, error) {
		applied = true
		return "", nil
	}
	request := sessionrpc.DispatchRequest{
		Principal: principals[session.ID],
		RequestID: "0123456789abcdef0123456789abcdef",
		Call: sessionrpc.Call{
			Kind: sessionrpc.CallOperation, Route: access.RouteAction,
			Verb: core.ActionSetArchived, ID: session.ID,
			Fields: map[string]string{"archived": "true"},
		},
	}

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if result, err := runtime.dispatch(expired, request); err != nil || result.Status != sessionrpc.StatusFailed || result.Code != "deadline_exceeded" {
		t.Fatalf("deadline expired before admission = %+v, err=%v; want failed deadline_exceeded", result, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	policy := runtime.policy
	runtime.policy = access.Policy{Resolver: cancelAfterPolicy{Resolver: runtime.resolver, cancel: cancel}}
	result, err := runtime.dispatch(ctx, request)
	runtime.policy = policy
	if err != nil || result.Status != sessionrpc.StatusFailed || result.Code != "deadline_exceeded" {
		t.Fatalf("deadline expired before the self-completion lookup = %+v, err=%v; want failed deadline_exceeded", result, err)
	}
	if applied {
		t.Fatal("a call whose deadline expired before its effect was applied")
	}
}
