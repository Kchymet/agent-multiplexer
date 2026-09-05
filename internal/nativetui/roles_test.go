package nativetui

import (
	"testing"

	"amux/internal/core"
	"amux/internal/store"
)

// Container rows that are default sessions open themselves; bare containers
// (from a daemon predating default sessions) still fall through to their first
// agent. Neither the console nor a container session can be moved or re-scoped.
func TestAttachableRoles(t *testing.T) {
	coordinator := &core.Session{ID: "wg1", IsRoot: true, Role: store.RoleCoordinator, Section: core.SectionWorkgroups}
	bareRoot := &core.Session{ID: "wg0", IsRoot: true, Section: core.SectionWorkgroups}
	home := &core.Session{ID: "api", Kind: "repo", Role: store.RoleRepo, Section: core.SectionRepos}
	bareRepo := &core.Session{ID: "web", Kind: "repo", Section: core.SectionRepos}
	member := &core.Session{ID: "a1", RootID: "wg1", Kind: "claude", Section: core.SectionWorkgroups}
	consoleRow := &core.Session{ID: "console", Role: store.RoleConsole, Mode: store.ModeConsole}

	for _, c := range []struct {
		name string
		s    *core.Session
		want bool
	}{
		{"coordinator", coordinator, true}, {"bare root", bareRoot, false},
		{"repo home", home, true}, {"bare repo", bareRepo, false},
		{"member", member, true}, {"console", consoleRow, true},
	} {
		if got := attachable(c.s); got != c.want {
			t.Errorf("attachable(%s) = %v, want %v", c.name, got, c.want)
		}
	}
	for _, c := range []struct {
		name string
		s    *core.Session
		want bool
	}{
		{"member", member, true}, {"coordinator", coordinator, false},
		{"repo home", home, false}, {"console", consoleRow, false},
	} {
		if got := isAgent(c.s); got != c.want {
			t.Errorf("isAgent(%s) = %v, want %v", c.name, got, c.want)
		}
	}

	// Enter on a coordinator attaches the coordinator; on a bare root, its first
	// agent; on a bare repo with no agent, nothing (the new-agent form opens).
	m := &model{sessions: []core.Session{*coordinator, *member, *bareRoot, {ID: "a0", RootID: "wg0", Kind: "claude", Section: core.SectionWorkgroups}, *bareRepo}}
	m.cursor = 0
	m.attachSelected()
	if m.attached != "wg1" {
		t.Errorf("Enter on the coordinator attached %q, want wg1", m.attached)
	}
	m.cursor = 2
	m.attachSelected()
	if m.attached != "a0" {
		t.Errorf("Enter on a bare root attached %q, want its first agent a0", m.attached)
	}
}
