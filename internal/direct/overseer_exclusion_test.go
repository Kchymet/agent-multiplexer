package direct

import (
	"testing"

	"amux/internal/core"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Until the overseer's direct exposure is signed off (docs/direct-console.md
// Phase B), it is excluded from direct access exactly like the console: never
// in an all_sessions scope and never grantable as a workgroup.
func TestOverseerIsNeverInDirectScope(t *testing.T) {
	g := Grant{Resource: ResourceScope{Kind: ResourceAllSessions}}
	byID := map[string]core.Session{}
	for _, row := range []core.Session{
		{ID: OverseerID, Role: hp.RoleOverseer, Source: "workspace"},
		{ID: ConsoleID, Role: hp.RoleConsole, Source: "workspace"},
	} {
		if inScope(g, row, byID) {
			t.Errorf("%s is in an all_sessions direct scope", row.ID)
		}
	}
	if _, err := ValidateResource(ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{OverseerID}}); err == nil {
		t.Error("the overseer can be granted as a workgroup")
	}
}
