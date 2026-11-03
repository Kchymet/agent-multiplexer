package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/sessionrpc"
)

// Restricted session RPC never serves the host-only identity queries: its
// query allowlist denies them for every principal.
func TestRestrictedQueryDeniesIdentityQueries(t *testing.T) {
	r := &sessionRuntime{}
	for _, q := range []string{QueryHostCapabilities, QuerySessionIdentities} {
		_, err := r.restrictedQuery(context.Background(), access.Principal{Kind: access.SubjectSession, SubjectID: "a1"},
			core.Action{Action: core.ActionQuery, Query: q, Fields: map[string]string{"ids": "a1"}})
		if !errors.Is(err, access.ErrDenied) {
			t.Fatalf("restricted %s = %v, want denied", q, err)
		}
	}
}

// Neither core.Result (which session RPC serializes whole) nor its Call can
// carry an identity: their JSON has no identity or incarnation field.
func TestSessionRPCShapesCarryNoIdentity(t *testing.T) {
	for _, v := range []any{core.Result{Type: "result", OK: true, NewID: "x"}, sessionrpc.Call{Fields: map[string]string{}}, core.Action{}} {
		b, _ := json.Marshal(v)
		for _, f := range []string{"identity", "ncarnation", "expectRoot"} {
			if strings.Contains(string(b), f) {
				t.Fatalf("%T carries %q: %s", v, f, b)
			}
		}
	}
}
