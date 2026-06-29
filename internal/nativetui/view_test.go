package nativetui

import (
	"strings"
	"testing"

	"amux/internal/core"
)

// Adding an agent to a workgroup row (root or child) opens the add-agent form
// targeting the workgroup root, with repos left blank (the daemon expands blank
// to the whole workgroup).
func TestAddAgentKeyOnWorkgroup(t *testing.T) {
	cases := []struct {
		name     string
		sessions []core.Session
		cursor   int
	}{
		{"on root", []core.Session{
			{ID: "wg1", Title: "payments", IsRoot: true, Section: core.SectionWorkgroups},
		}, 0},
		{"on child", []core.Session{
			{ID: "wg1", Title: "payments", IsRoot: true, Section: core.SectionWorkgroups},
			{ID: "ag1", Title: "idempotency", RootID: "wg1", Section: core.SectionWorkgroups},
		}, 1},
	}
	for _, tc := range cases {
		m := &model{sessions: tc.sessions, cursor: tc.cursor}
		m.handleKey(key("a"))
		if m.form == nil {
			t.Fatalf("%s: no form opened", tc.name)
		}
		if m.form.action != "add-agent" {
			t.Fatalf("%s: action %q, want add-agent", tc.name, m.form.action)
		}
		if m.form.id != "wg1" {
			t.Fatalf("%s: target id %q, want wg1", tc.name, m.form.id)
		}
		if v := m.form.values()["repos"]; v != "" {
			t.Fatalf("%s: repos %q, want blank", tc.name, v)
		}
	}
}

// `a` on a repo header still opens the repo-scoped new-agent form.
func TestAddAgentKeyOnRepo(t *testing.T) {
	m := &model{sessions: []core.Session{{ID: "acme/api", Title: "acme/api", Kind: "repo", Section: core.SectionRepos}}}
	m.handleKey(key("a"))
	if m.form == nil || m.form.action != "new-repo-agent" || m.form.id != "acme/api" {
		t.Fatalf("repo agent form not opened correctly: %+v", m.form)
	}
}

// `R` opens the track-repo form (a single source field, add-repo action).
func TestAddRepoKey(t *testing.T) {
	m := &model{}
	m.handleKey(key("R"))
	if m.form == nil || m.form.action != "add-repo" {
		t.Fatalf("add-repo form not opened: %+v", m.form)
	}
	if len(m.form.fields) != 1 || m.form.fields[0].key != "source" {
		t.Fatalf("add-repo form should have a single source field, got %+v", m.form.fields)
	}
}

// `D` asks to permanently delete the selected workgroup (delete action, guarded
// by a confirmation modal).
func TestDeleteKeyConfirms(t *testing.T) {
	m := &model{sessions: []core.Session{{ID: "wg1", Title: "payments", IsRoot: true, Section: core.SectionWorkgroups}}}
	m.handleKey(key("D"))
	if m.confirm == nil {
		t.Fatal("delete should open a confirmation modal")
	}
	if m.confirm.action.Action != "delete" || m.confirm.action.ID != "wg1" {
		t.Fatalf("confirm action: got %+v, want delete wg1", m.confirm.action)
	}
}

// Empty WORKGROUPS and REPOS sections are still drawn, with a hint pointing at
// the key that creates the first one — discoverability the old rail had.
func TestEmptySectionHints(t *testing.T) {
	m := &model{sessions: nil, h: 30}
	out := m.renderSidebar()
	for _, want := range []string{"no workgroups", "no repos"} {
		if !strings.Contains(out, want) {
			t.Errorf("sidebar missing empty hint %q:\n%s", want, out)
		}
	}
}

// An agent row carries a state-colored status sub-line; repos and containers do not.
func TestRowStatusSubline(t *testing.T) {
	m := &model{}
	agent := core.Session{ID: "ag1", Title: "idempotency", RootID: "wg1", Status: "ready · main", State: core.StateReady, Section: core.SectionWorkgroups}
	if got := m.rowStatus(agent, " "); got == "" {
		t.Fatal("agent row should have a status sub-line")
	}
	repo := core.Session{ID: "acme/api", Title: "acme/api", Kind: "repo", Status: "x", Section: core.SectionRepos}
	if got := m.rowStatus(repo, ""); got != "" {
		t.Fatalf("repo row should have no sub-line, got %q", got)
	}
}
