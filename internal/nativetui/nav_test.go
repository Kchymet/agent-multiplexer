package nativetui

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"amux/internal/core"
)

func railModel(rows, height int) *model {
	var sessions []core.Session
	for i := 0; i < rows; i++ {
		sessions = append(sessions, core.Session{
			ID:      fmt.Sprintf("ag%02d", i),
			Title:   fmt.Sprintf("agent-%02d", i),
			Section: core.SectionWorkgroups,
		})
	}
	return &model{sessions: sessions, h: height}
}

// page moves by a signed row count and clamps at both ends instead of wrapping
// (unlike move), so paging past an edge stops there.
func TestPageClamps(t *testing.T) {
	m := railModel(10, 24)

	m.cursor = 8
	m.page(5) // would land at 13
	if m.sectionCursor != core.SectionRepos {
		t.Fatalf("page past the end: section = %q, want repos", m.sectionCursor)
	}

	m.cursor, m.sectionCursor = 2, ""
	m.page(-5) // would land at -3
	if m.sectionCursor != core.SectionWorkgroups {
		t.Fatalf("page past the start: section = %q, want workgroups", m.sectionCursor)
	}

	empty := &model{}
	empty.page(3) // must not panic on an empty rail
	if empty.cursor != 0 {
		t.Fatalf("page on empty rail: cursor = %d, want 0", empty.cursor)
	}
}

// The step helpers scale with the pane height and never drop below one row, so
// paging always makes progress even in a tiny window.
func TestPageStepSizes(t *testing.T) {
	// paneRows() == h-3, so h=23 gives a 20-row pane: half = 10, full = 19.
	m := railModel(1, 23)
	if got := m.halfPage(); got != 10 {
		t.Fatalf("halfPage = %d, want 10", got)
	}
	if got := m.pageStep(); got != 19 {
		t.Fatalf("pageStep = %d, want 19", got)
	}

	tiny := railModel(1, 5) // paneRows()==2, so both steps floor at 1
	if got := tiny.halfPage(); got != 1 {
		t.Fatalf("halfPage (tiny) = %d, want 1", got)
	}
	if got := tiny.pageStep(); got != 1 {
		t.Fatalf("pageStep (tiny) = %d, want 1", got)
	}
}

// The rail responds to the common pager navigation keys: ctrl-u/ctrl-d for half
// pages, PgUp/PgDn for full pages, and Home/End (and g/G) to jump to the ends.
func TestPagingKeys(t *testing.T) {
	half := railModel(1, 23).halfPage() // 10
	full := railModel(1, 23).pageStep() // 19

	cases := []struct {
		name    string
		start   int
		key     tea.KeyMsg
		want    int
		section string
	}{
		{"ctrl+d half down", 0, tea.KeyMsg{Type: tea.KeyCtrlD}, half, ""},
		{"ctrl+u half up", 30, tea.KeyMsg{Type: tea.KeyCtrlU}, 30 - half, ""},
		{"pgdown full down", 0, tea.KeyMsg{Type: tea.KeyPgDown}, full, ""},
		{"pgup full up", 30, tea.KeyMsg{Type: tea.KeyPgUp}, 30 - full, ""},
		{"home to top", 20, tea.KeyMsg{Type: tea.KeyHome}, 0, core.SectionWorkgroups},
		{"end to bottom", 0, tea.KeyMsg{Type: tea.KeyEnd}, 0, core.SectionRepos},
		{"g to top", 20, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")}, 0, core.SectionWorkgroups},
		{"G to bottom", 0, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")}, 0, core.SectionRepos},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := railModel(40, 23)
			m.cursor = tc.start
			m.handleKey(tc.key)
			if m.cursor != tc.want || m.sectionCursor != tc.section {
				t.Fatalf("selection = (%d, %q), want (%d, %q)", m.cursor, m.sectionCursor, tc.want, tc.section)
			}
		})
	}
}
