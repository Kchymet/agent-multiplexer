package access

import (
	"testing"

	"amux/internal/core"
)

// TestAllowSteerNeverAdmitsAnswer pins that no restricted principal — a
// coordinator steering its member, an agent steering itself — may answer a
// question form for the user (harnessproto.VerbAnswer is host-only).
func TestAllowSteerNeverAdmitsAnswer(t *testing.T) {
	req := Request{Route: RouteAction, Verb: core.ActionSteer, ID: "a1", Fields: map[string]string{
		core.SteerVerb: core.SteerAnswer, core.SteerRequestID: "q-aaaaaaaaaa", core.SteerAnswerField: `{"version":2,"action":"cancel"}`,
	}}
	for _, self := range []bool{false, true} {
		if allowSteer(req, self, Resource{RootID: "wg1"}) {
			t.Fatalf("allowSteer admitted an answer (self=%v)", self)
		}
	}
}

// TestQuestionHoldSelfOnly pins that a session waits only on its own held
// question: a coordinator cannot read (or wait on) a member's answer, and the
// overseer holds no such rule.
func TestQuestionHoldSelfOnly(t *testing.T) {
	rule, ok := RuleFor(RouteQuery, core.QueryQuestionHold)
	if !ok || rule.Scope != ScopeWorkgroup || rule.Overseer || rule.Allow == nil {
		t.Fatalf("rule = %+v %v", rule, ok)
	}
	req := Request{Route: RouteQuery, Verb: core.QueryQuestionHold, ID: "a1"}
	if !rule.Allow(req, true, Resource{}) || rule.Allow(req, false, Resource{}) {
		t.Fatal("the hold must be self-only")
	}
	if r, ok := RuleFor(RouteAction, core.ActionQueueRelease); !ok || r.Scope != ScopeHost {
		t.Fatalf("queue-release rule = %+v %v", r, ok)
	}
}
