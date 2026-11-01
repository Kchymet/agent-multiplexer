package nativetui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"amux/internal/core"
	"amux/internal/daemon"
)

// railSessions is a rail with two workgroups, each holding one agent.
func railSessions() []core.Session {
	return []core.Session{
		{ID: "console", Title: "console"},
		{ID: "wg", Title: "payments", IsRoot: true, Section: core.SectionWorkgroups},
		{ID: "wg-agent", Title: "pay-agent", RootID: "wg", Section: core.SectionWorkgroups},
		{ID: "wg2", Title: "billing", IsRoot: true, Section: core.SectionWorkgroups},
		{ID: "wg2-agent", Title: "bill-agent", RootID: "wg2", Section: core.SectionWorkgroups},
		{ID: "repo", Title: "acme/api", Kind: "repo", Section: core.SectionRepos},
	}
}

// openDashboard simulates a dashboard start: load state, then the daemon's
// first snapshot arrives.
func openDashboard(t *testing.T, path string, sessions []core.Session) *model {
	t.Helper()
	m := &model{w: 120, h: 40}
	m.openRailState(path)
	m.Update(frameMsg{f: daemon.Frame{Snapshot: &core.Snapshot{Sessions: sessions}}})
	return m
}

func readState(t *testing.T, path string) railStateFile {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var st railStateFile
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestRailStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "tui-state.json")
	m := openDashboard(t, path, railSessions())
	m.selectByID("wg2")
	m.toggleRailGroup() // fold billing
	m.selectEntry(railEntry{section: core.SectionRepos})
	m.toggleRailGroup() // fold the repos section
	m.selectByID("wg-agent")
	if cmd := m.persistRailState(); cmd == nil {
		t.Fatal("a changed view should schedule a save")
	}
	m.flushRailState() // clean exit

	m = openDashboard(t, path, railSessions())
	if !m.collapsed[railGroup{core.SectionWorkgroups, "wg2"}] || !m.collapsed[railGroup{section: core.SectionRepos}] {
		t.Fatalf("folds not restored: %v", m.collapsed)
	}
	if m.collapsed[railGroup{core.SectionWorkgroups, "wg"}] || m.collapsed[railGroup{section: core.SectionWorkgroups}] {
		t.Fatalf("expanded groups must stay expanded: %v", m.collapsed)
	}
	if s := m.selected(); s == nil || s.ID != "wg-agent" {
		t.Fatalf("selection not restored: %+v", s)
	}
	if m.persistRailState() != nil {
		t.Fatal("an unchanged restored view must not rewrite the file")
	}
}

func TestRailStateSectionSelectionAndDebounce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tui-state.json")
	m := openDashboard(t, path, railSessions())
	m.selectEntry(railEntry{section: core.SectionRepos})
	m.persistRailState()
	stale := m.state.serial
	m.toggleRailGroup()
	if m.persistRailState() == nil {
		t.Fatal("second change should reschedule")
	}
	m.Update(railStateTickMsg{serial: stale})
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a superseded debounce tick must not write")
	}
	m.Update(railStateTickMsg{serial: m.state.serial})
	if st := readState(t, path); st.Selected.Section != core.SectionRepos {
		t.Fatalf("saved selection = %+v", st.Selected)
	}
	m = openDashboard(t, path, railSessions())
	if m.sectionCursor != core.SectionRepos {
		t.Fatalf("section selection not restored: %q", m.sectionCursor)
	}
}

func TestRailStatePrunesArchivedAndMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tui-state.json")
	m := openDashboard(t, path, railSessions())
	m.selectByID("wg2")
	m.toggleRailGroup()
	m.selectByID("wg2-agent")
	m.flushRailState()

	// While closed, billing was archived and its agent with it.
	sessions := railSessions()
	sessions[3].Section, sessions[3].Archived = core.SectionArchived, true
	sessions[4].Section, sessions[4].Archived = core.SectionArchived, true
	m = openDashboard(t, path, sessions)
	if len(m.collapsed) != 0 {
		t.Fatalf("stale folds kept: %v", m.collapsed)
	}
	if s := m.selected(); s != nil && archivedSession(*s) {
		t.Fatalf("restore resurrected an archived row: %+v", s)
	}
	for _, e := range m.railEntries() {
		if e.section == "" && archivedSession(m.sessions[e.index]) {
			t.Fatalf("archived row on the rail: %+v", m.sessions[e.index])
		}
	}
	m.flushRailState()
	st := readState(t, path)
	for _, g := range st.Groups {
		if g.ID == "wg2" {
			t.Fatal("archived workgroup not pruned from the saved state")
		}
	}
	if st.Selected.ID == "wg2-agent" {
		t.Fatal("archived selection not pruned")
	}

	// A deleted id simply disappears too.
	m = openDashboard(t, path, railSessions()[:3])
	if m.collapsed[railGroup{core.SectionWorkgroups, "wg2"}] {
		t.Fatal("restored archive must start from the default, not an old fold")
	}
}

func TestRailStateHiddenSelectionStaysFolded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tui-state.json")
	m := openDashboard(t, path, railSessions())
	m.selectByID("wg2")
	m.toggleRailGroup()
	m.flushRailState()
	// Hand-edit the saved selection to a row under the fold: restoring it must
	// select the visible parent, not unfold the group.
	st := readState(t, path)
	st.Selected = railStateSel{ID: "wg2-agent"}
	b, _ := json.Marshal(st)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	m = openDashboard(t, path, railSessions())
	if !m.collapsed[railGroup{core.SectionWorkgroups, "wg2"}] {
		t.Fatal("restoring a hidden selection unfolded its group")
	}
	if s := m.selected(); s == nil || s.ID != "wg2" {
		t.Fatalf("hidden selection should land on its parent, got %+v", s)
	}
}

func TestRailStateNewItemsGetDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tui-state.json")
	m := openDashboard(t, path, railSessions())
	m.selectByID("wg")
	m.toggleRailGroup()
	m.flushRailState()

	sessions := append(railSessions(),
		core.Session{ID: "wg3", Title: "fresh", IsRoot: true, Section: core.SectionWorkgroups},
		core.Session{ID: "wg3-agent", Title: "fresh-agent", RootID: "wg3", Section: core.SectionWorkgroups})
	m = openDashboard(t, path, sessions)
	if !m.collapsed[railGroup{core.SectionWorkgroups, "wg"}] {
		t.Fatal("known fold lost")
	}
	if m.collapsed[railGroup{core.SectionWorkgroups, "wg3"}] || m.collapsed[railGroup{core.SectionWorkgroups, "wg2"}] {
		t.Fatal("new and previously-open groups take the default (expanded)")
	}
}

func TestRailStateCorruptOrMissingFile(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"missing": "",
		"garbage": "{not json",
		"future":  `{"version":99,"groups":[{"section":"workgroups","collapsed":true}]}`,
		"wrong":   `[1,2,3]`,
	} {
		path := filepath.Join(dir, name+".json")
		if content != "" {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		m := openDashboard(t, path, railSessions())
		if len(m.collapsed) != 0 {
			t.Fatalf("%s: expected defaults, got %v", name, m.collapsed)
		}
		if s := m.selected(); s == nil || s.ID != "console" {
			t.Fatalf("%s: expected default selection, got %+v", name, s)
		}
		// The next save replaces the bad file with a valid one.
		m.selectByID("wg")
		m.toggleRailGroup()
		m.flushRailState()
		if st := readState(t, path); st.Version != railStateVersion {
			t.Fatalf("%s: rewritten state = %+v", name, st)
		}
	}
}

func TestRailStateNotSavedBeforeFirstSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tui-state.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"groups":[{"section":"workgroups","id":"wg","collapsed":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m := &model{w: 120, h: 40}
	m.openRailState(path)
	// Quitting before the daemon answers must not prune against an empty rail.
	m.flushRailState()
	if st := readState(t, path); len(st.Groups) != 1 {
		t.Fatalf("state clobbered before first snapshot: %+v", st)
	}
}
