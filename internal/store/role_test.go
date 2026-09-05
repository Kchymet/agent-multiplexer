package store

import (
	"testing"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// The role vocabulary is restated in store (below core in the import graph);
// the wire's constants are the source of truth, so the two must agree.
func TestRoleConstantsMatchWire(t *testing.T) {
	if RoleConsole != harnessproto.RoleConsole || RoleCoordinator != harnessproto.RoleCoordinator || RoleRepo != harnessproto.RoleRepo {
		t.Fatalf("store roles (%q %q %q) drifted from harnessproto (%q %q %q)",
			RoleConsole, RoleCoordinator, RoleRepo,
			harnessproto.RoleConsole, harnessproto.RoleCoordinator, harnessproto.RoleRepo)
	}
}

func TestSessionRole(t *testing.T) {
	cases := []struct {
		name string
		s    Session
		want string
	}{
		{"console by mode", Session{ID: "console", Mode: ModeConsole}, RoleConsole},
		{"work root is the coordinator", Session{ID: "wg1", Scope: ScopeWork}, RoleCoordinator},
		{"root with blank scope is work", Session{ID: "wg2"}, RoleCoordinator},
		{"repo home: repo-scoped root whose id is the repo", Session{ID: "api", Scope: ScopeRepo, Repo: "api"}, RoleRepo},
		{"hidden single-member repo root hosts nothing", Session{ID: "3f7a9c", Scope: ScopeRepo, Repo: "api"}, RoleAgent},
		{"member agent", Session{ID: "a1", RootID: "wg1", Repo: "api"}, RoleAgent},
	}
	for _, c := range cases {
		if got := c.s.Role(); got != c.want {
			t.Errorf("%s: Role() = %q, want %q", c.name, got, c.want)
		}
	}
	if RepoHomeID("api") != "api" {
		t.Errorf("RepoHomeID(api) = %q", RepoHomeID("api"))
	}
}
