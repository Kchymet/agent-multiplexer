package nativetui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"amux/internal/core"
	"amux/internal/daemon"
)

func groupedRail() *model {
	return &model{w: 120, h: 40, sessions: []core.Session{
		{ID: "console", Title: "console"},
		{ID: "wg", Title: "payments", IsRoot: true, Section: core.SectionWorkgroups},
		{ID: "wg-agent", Title: "workgroup-child", RootID: "wg", Section: core.SectionWorkgroups},
		{ID: "repo", Title: "acme/api", Kind: "repo", Section: core.SectionRepos},
		{ID: "repo-agent", Title: "repo-child", RootID: "repo", Section: core.SectionRepos},
		{ID: "archived", Title: "archived-child", RootID: "wg", Section: core.SectionArchived},
		{ID: "detached", Title: "external-session", Section: core.SectionDetached},
	}}
}

func TestFoldGroupsAndSections(t *testing.T) {
	for _, tc := range []struct {
		id, child, sibling string
	}{
		{"wg", "workgroup-child", "repo-child"},
		{"repo", "repo-child", "workgroup-child"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			m := groupedRail()
			m.selectByID(tc.id)
			m.attached = tc.id + "-agent"
			m.handleKey(tea.KeyMsg{Type: tea.KeySpace})
			out := plain(m.renderSidebar())
			if strings.Contains(out, tc.child) || !strings.Contains(out, tc.sibling) || !strings.Contains(out, "archived-child") {
				t.Fatalf("fold hid the wrong rows:\n%s", out)
			}
			if s := m.selected(); s == nil || s.ID != tc.id || m.attached != tc.id+"-agent" {
				t.Fatal("fold changed selection or detached the active agent")
			}
			m.handleKey(tea.KeyMsg{Type: tea.KeySpace})
			if !strings.Contains(plain(m.renderSidebar()), tc.child) {
				t.Fatal("unfold did not restore child")
			}
		})
	}
	for _, section := range []string{core.SectionWorkgroups, core.SectionRepos, core.SectionArchived, core.SectionDetached} {
		t.Run(section, func(t *testing.T) {
			m := groupedRail()
			m.selectEntry(railEntry{section: section})
			m.handleKey(key(" "))
			out := plain(m.renderSidebar())
			if !strings.Contains(out, "▸"+sectionLabel(section)) {
				t.Fatalf("missing collapsed header:\n%s", out)
			}
			for _, s := range m.sessions {
				if s.Section == section && strings.Contains(out, s.Title) {
					t.Fatalf("folded section still renders %q", s.Title)
				}
			}
			if m.selected() != nil {
				t.Fatal("section header must not target a daemon session")
			}
			m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
			if m.collapsed[railGroup{section: section}] {
				t.Fatal("Enter on a section should unfold it")
			}
		})
	}
}

func TestFoldNavigation(t *testing.T) {
	m := groupedRail()
	m.selectByID("wg")
	m.handleKey(tea.KeyMsg{Type: tea.KeyLeft})
	m.handleKey(key("j"))
	if m.sectionCursor != core.SectionRepos {
		t.Fatal("down must skip the folded workgroup child")
	}
	m.handleKey(key("k"))
	if m.selected().ID != "wg" {
		t.Fatal("up must skip the folded child")
	}
	m.handleKey(tea.KeyMsg{Type: tea.KeyRight})
	if m.selected().ID != "wg" || m.collapsed[railGroup{core.SectionWorkgroups, "wg"}] {
		t.Fatal("right should expand without moving the cursor")
	}
	m.handleKey(key("l"))
	if m.selected().ID != "wg-agent" {
		t.Fatal("right on an open group should enter its first child")
	}
	m.handleKey(key("h"))
	if m.selected().ID != "wg" {
		t.Fatal("left on a child should select its parent")
	}
	m.handleKey(key("h")) // collapse workgroup
	m.handleKey(key("h")) // parent section
	if m.sectionCursor != core.SectionWorkgroups {
		t.Fatal("left on a folded group should select its section")
	}
	m.handleKey(key(" ")) // collapse section
	m.handleKey(key(" ")) // reopen section; preserve the nested fold
	if !m.collapsed[railGroup{core.SectionWorkgroups, "wg"}] {
		t.Fatal("section fold lost the workgroup fold")
	}
	m.page(2)
	if m.sectionCursor != core.SectionRepos {
		t.Fatal("paging must count visible rows only")
	}
	m.railEdge(true)
	m.move(1)
	if m.selected().ID != "console" {
		t.Fatal("down at the end should wrap to the first visible row")
	}
}

func TestFoldSnapshotAndPendingAttach(t *testing.T) {
	m := groupedRail()
	m.selectByID("repo")
	m.toggleRailGroup()
	// Inserting a session before the selection must retain the selected ID.
	sessions := append([]core.Session{{ID: "new-console"}}, m.sessions...)
	m.Update(frameMsg{f: daemon.Frame{Snapshot: &core.Snapshot{Sessions: sessions}}})
	if m.selected().ID != "repo" || !m.collapsed[railGroup{core.SectionRepos, "repo"}] {
		t.Fatal("snapshot lost selection or fold")
	}
	m.selectEntry(railEntry{section: core.SectionRepos})
	m.toggleRailGroup()
	m.pending = "repo-agent"
	if m.tryPendingAttach() == nil || m.selected().ID != "repo-agent" {
		t.Fatal("pending attach must select the new agent")
	}
	if m.hidden(m.selected()) || !strings.Contains(plain(m.renderSidebar()), "repo-child") {
		t.Fatal("pending attach must reveal the section and parent")
	}
}

func TestRefreshMovesHiddenSelectionToAncestor(t *testing.T) {
	m := groupedRail()
	m.setRailCollapsed(railGroup{core.SectionRepos, "repo"}, true)
	m.selectByID("wg-agent")
	sessions := append([]core.Session(nil), m.sessions...)
	sessions[2].Section, sessions[2].RootID = core.SectionRepos, "repo"
	m.refreshSessions(sessions)
	if m.selected() == nil || m.selected().ID != "repo" {
		t.Fatal("moved agent should select its visible folded parent")
	}
	m.refreshSessions(nil)
	if m.sectionCursor != core.SectionWorkgroups {
		t.Fatal("empty snapshot should fall back to a visible section")
	}
}

func TestFoldHintsAndScroll(t *testing.T) {
	m := groupedRail()
	for name, out := range map[string]string{"rail": strings.Join(m.railHints(), "\n"), "footer": m.renderHelp()} {
		out = plain(out)
		if !strings.Contains(out, "Space") || !strings.Contains(out, "fold/unfold") {
			t.Fatalf("%s missing fold shortcut: %s", name, out)
		}
	}
	m.h = 12
	m.selectByID("detached")
	m.renderSidebar()
	m.handleKey(key("h")) // select detached section
	m.handleKey(key(" "))
	out := plain(m.renderSidebar())
	if !strings.Contains(out, "▸ DETACHED") || strings.Contains(out, "external-session") || !strings.Contains(out, "Space") {
		t.Fatalf("folded selection/hint lost in short rail:\n%s", out)
	}
}

func TestFoldFooterFitsNarrowTerminal(t *testing.T) {
	m := groupedRail()
	m.w = 80
	out := m.renderHelp()
	if lipgloss.Width(out) > m.w || !strings.Contains(plain(out), "Space fold/unfold") {
		t.Fatalf("footer should fit and retain the fold hint: %q", out)
	}
}
