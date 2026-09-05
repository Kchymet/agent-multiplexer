package source

import (
	"context"
	"path/filepath"
	"testing"

	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/store"
)

// Every container row is a default session: the console, a workgroup root (its
// coordinator), and a repo header (its home) carry a role, a runtime, steerable
// caps, and a sandbox — so a consumer can open and steer them like an agent.
// A bare single-member repo root still hosts nothing and stays hidden.
func TestPollDefaultSessions(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("HOME", t.TempDir())

	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []store.Session{
		{ID: "wg1", Name: "payments", Scope: store.ScopeWork, Agent: "claude", Mode: store.ModeInteractive, Dir: store.RootDir("wg1"), ClaudeID: "conv-wg1", Created: 1},
		{ID: "a1", RootID: "wg1", Agent: "claude", Mode: store.ModeTask, Created: 2},
		{ID: "legacy", Name: "old", Scope: store.ScopeWork, Created: 3}, // predates default sessions: no dir, no id
		{ID: "api", Scope: store.ScopeRepo, Repo: "api", Agent: "claude", Mode: store.ModeInteractive, Dir: store.RootDir("api"), ClaudeID: "conv-api", Created: 4},
		{ID: "hidden", Scope: store.ScopeRepo, Repo: "api", Created: 5}, // the wrapper around a one-off
		{ID: "one", RootID: "hidden", Agent: "claude", Repo: "api", Mode: store.ModeTask, Created: 6},
	} {
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.PutRepo(store.Repo{Name: "api", Source: "octo/api", GitDir: "/x/api.git"}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutRepo(store.Repo{Name: "web", Source: "octo/web", GitDir: "/x/web.git"}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	// The coordinator is live and mid-turn; the repo home is stopped.
	if err := core.WriteHookState("conv-wg1", core.StateRunning, ""); err != nil {
		t.Fatal(err)
	}
	w := NewWorkspace()
	w.SetLiveness(func() map[string]bool { return map[string]bool{"wg1": true} })

	rows, err := w.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]core.Session{}
	for _, r := range rows {
		byID[r.ID] = r
	}
	allOn := core.SessionCaps{Prompt: true, Interject: true, Cancel: true, Permission: true}
	check := func(id, role, runtime, cwd string) core.Session {
		t.Helper()
		r, ok := byID[id]
		if !ok {
			t.Fatalf("row %s missing", id)
		}
		if r.Role != role || r.Runtime != runtime || !r.CanAttach {
			t.Errorf("%s: role=%q runtime=%q canAttach=%v, want %q %q true", id, r.Role, r.Runtime, r.CanAttach, role, runtime)
		}
		if r.Caps == nil || *r.Caps != allOn {
			t.Errorf("%s: caps = %+v, want all-on", id, r.Caps)
		}
		if r.Cwd != cwd {
			t.Errorf("%s: cwd = %q, want %q", id, r.Cwd, cwd)
		}
		return r
	}
	check(console.ID, store.RoleConsole, "claude", console.Dir())
	wg := check("wg1", store.RoleCoordinator, "claude", store.RootDir("wg1"))
	if !wg.IsRoot || wg.Section != core.SectionWorkgroups || wg.State != core.StateRunning || wg.Kind != "claude" {
		t.Errorf("coordinator row = %+v", wg)
	}
	// A root predating default sessions is still a coordinator: it gets its
	// container dir on first open, so the rail advertises that dir now.
	check("legacy", store.RoleCoordinator, "claude", store.RootDir("legacy"))
	api := check("api", store.RoleRepo, "claude", store.RootDir("api"))
	if api.Kind != "repo" || api.Section != core.SectionRepos || api.State != core.StateIdle {
		t.Errorf("repo home row = %+v", api)
	}
	// A repo tracked before default sessions has no home row yet; its header is
	// still a repo home the daemon materializes on open.
	check("web", store.RoleRepo, "claude", store.RootDir("web"))
	if _, ok := byID["hidden"]; ok {
		t.Error("the hidden single-member repo root rendered as a row")
	}
	if one := byID["one"]; one.RootID != "api" || one.Section != core.SectionRepos || one.Role != "" {
		t.Errorf("one-off row = %+v, want nested under the repo with no role", one)
	}
	if a1 := byID["a1"]; a1.Role != "" || a1.RootID != "wg1" {
		t.Errorf("member row = %+v", a1)
	}
	// The home row must never be mistaken for a workgroup.
	for _, r := range rows {
		if r.ID == "api" && r.IsRoot {
			t.Error("repo home row flagged IsRoot")
		}
	}
}
