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
