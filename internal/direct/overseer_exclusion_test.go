package direct

import (
	"testing"

	"amux/internal/core"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// The overseer is in scope for an all_sessions grant only, never for a
// workgroups grant, and never grantable as a workgroup; the console is never
// in scope (docs/direct-console.md §5).
func TestOverseerScopeRule(t *testing.T) {
	byID := map[string]core.Session{}
	overseer := core.Session{ID: OverseerID, Role: hp.RoleOverseer, Source: "workspace"}
	console := core.Session{ID: ConsoleID, Role: hp.RoleConsole, Source: "workspace"}
	all := Grant{Resource: ResourceScope{Kind: ResourceAllSessions}}
	wg := Grant{Resource: ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{OverseerID, "wg1"}}}
	if !inScope(all, overseer, byID) {
		t.Error("the overseer is not in an all_sessions scope")
	}
	if inScope(wg, overseer, byID) {
		t.Error("the overseer is in a workgroups scope")
	}
	if inScope(all, console, byID) || inScope(wg, console, byID) {
		t.Error("the console is in scope")
	}
	if _, err := ValidateResource(ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{OverseerID}}); err == nil {
		t.Error("the overseer can be granted as a workgroup")
	}
	if len(filtered(all, []core.Session{overseer})) != 0 || len(filteredWith(all, []core.Session{overseer}, true)) != 1 {
		t.Error("the overseer row is not opt-in")
	}
}
