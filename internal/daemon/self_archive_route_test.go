package daemon

import (
	"testing"

	"amux/internal/access"
	"amux/internal/core"
)

// A coordinator wrapping up archives its own workgroup as its last tool call.
// That archive must take the self-completion path, which answers the caller
// before the runtime is stopped (and only once its turn has ended or a grace
// expires), never the ordinary dispatch, which stops the runtime before its
// response. Both spellings the guide names — `amux agent done` and
// `amux archive <own id>` — send this same action.
func TestCoordinatorSelfArchiveTakesCompletionPath(t *testing.T) {
	self := access.Principal{Kind: access.SubjectSession, SubjectID: "wg1"}
	archive := func(id string) core.Action {
		return core.Action{Action: core.ActionSetArchived, ID: id, Fields: map[string]string{"archived": "true"}}
	}
	if !selfCompletionShape(self, archive("wg1")) {
		t.Fatal("coordinator archiving its own workgroup is not routed as self-completion")
	}
	if selfCompletionShape(self, archive("a1")) {
		t.Error("archiving a member is routed as self-completion")
	}
	restore := core.Action{Action: core.ActionSetArchived, ID: "wg1", Fields: map[string]string{"archived": "false"}}
	if selfCompletionShape(self, restore) {
		t.Error("a restore is routed as self-completion")
	}
}
