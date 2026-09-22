package daemon

import (
	"testing"

	"amux/internal/access"
	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/wsops"
)

// A restricted principal — the console, whose guide teaches these fields, or any
// mailbox caller — must be able to choose a workgroup's coordinator runtime and
// model. The canonical operation's field allowlist is what decides that, and an
// omission there rejects the exact command the guide hands the model.
func TestRestrictedCreationCarriesCoordinatorFields(t *testing.T) {
	for _, verb := range []string{core.ActionNewWorkgroup, core.ActionCreateWorkspace} {
		t.Run(verb, func(t *testing.T) {
			fields := map[string]string{
				"name":                      "payments",
				"prompt":                    "ship the release",
				wsops.FieldCoordinator:      "claude",
				wsops.FieldCoordinatorModel: "claude-opus-5",
			}
			op, err := canonicalSessionOperation(access.Request{
				Route: access.RouteAction, Verb: verb, Fields: fields,
			})
			if err != nil {
				t.Fatalf("restricted %s with a chosen coordinator: %v", verb, err)
			}
			// The canonical copy must carry them through unchanged, or the daemon
			// would create a default coordinator while the caller was told otherwise.
			for key, want := range map[string]string{
				wsops.FieldCoordinator:      "claude",
				wsops.FieldCoordinatorModel: "claude-opus-5",
				"prompt":                    "ship the release",
			} {
				if got := op.Fields[key]; got != want {
					t.Errorf("canonical %s dropped %s: got %q, want %q", verb, key, got, want)
				}
			}
			// Omitting them still means the default goal runtime.
			bare, err := canonicalSessionOperation(access.Request{
				Route: access.RouteAction, Verb: verb, Fields: map[string]string{"name": "payments"},
			})
			if err != nil {
				t.Fatalf("restricted %s without a coordinator: %v", verb, err)
			}
			if got := wsops.CoordinatorKind(bare.Fields[wsops.FieldCoordinator]); got != agent.GoalRuntime {
				t.Fatalf("default coordinator = %q, want %q", got, agent.GoalRuntime)
			}
			// The allowlist is still closed: an unknown field is refused.
			if _, err := canonicalSessionOperation(access.Request{
				Route: access.RouteAction, Verb: verb, Fields: map[string]string{"coordinator_role": "console"},
			}); err == nil {
				t.Fatal("the creation allowlist accepted an unknown field")
			}
		})
	}
}
