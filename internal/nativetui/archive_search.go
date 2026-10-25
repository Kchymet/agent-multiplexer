package nativetui

import (
	"strings"

	"amux/internal/core"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type archiveSearchState struct {
	query    string
	selected string // preserve identity when inventory updates reorder results
	details  string // read-only details target, never a pending runtime attachment
}

func archivedSession(s core.Session) bool {
	return s.Archived || s.Section == core.SectionArchived
}

func (m *model) archivedMatches() []core.Session {
	var rows []core.Session
	for _, s := range m.sessions {
		if archivedSession(s) && (fuzzyMatch(m.archiveSearch.query, s.Title) || fuzzyMatch(m.archiveSearch.query, s.ID)) {
			rows = append(rows, s)
		}
	}
	return rows
}

func (m *model) archiveSelection(rows []core.Session) int {
	for i, s := range rows {
		if s.ID == m.archiveSearch.selected {
			return i
		}
	}
	if len(rows) > 0 {
		m.archiveSearch.selected = rows[0].ID
	} else {
		m.archiveSearch.selected = ""
	}
	return 0
}

func (m *model) handleArchiveSearch(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	s := m.archiveSearch
	if s.details != "" {
		switch k.Type {
		case tea.KeyEsc:
			s.details = ""
		case tea.KeyCtrlC:
			m.archiveSearch = nil
		case tea.KeyCtrlR:
			if row := m.sessionByID(s.details); row != nil && archivedSession(*row) {
				m.confirmArchiveRestore(*row)
			}
		}
		return m, nil
	}
	rows := m.archivedMatches()
	i := m.archiveSelection(rows)
	switch k.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.archiveSearch = nil
	case tea.KeyUp, tea.KeyCtrlP, tea.KeyDown, tea.KeyCtrlN:
		if len(rows) > 0 {
			delta := 1
			if k.Type == tea.KeyUp || k.Type == tea.KeyCtrlP {
				delta = -1
			}
			s.selected = rows[(i+delta+len(rows))%len(rows)].ID
		}
	case tea.KeyBackspace:
		if r := []rune(s.query); len(r) > 0 {
			s.query = string(r[:len(r)-1])
		}
	case tea.KeyRunes:
		s.query += accountText(string(k.Runes))
	case tea.KeySpace:
		s.query += " "
	case tea.KeyEnter, tea.KeyCtrlR:
		if len(rows) == 0 {
			return m, nil
		}
		selected := rows[i]
		if k.Type == tea.KeyCtrlR {
			m.confirmArchiveRestore(selected)
		} else {
			s.details = selected.ID
		}

	}
	return m, nil
}

func (m *model) confirmArchiveRestore(s core.Session) {
	m.archiveSearch = nil
	m.confirm = &confirmState{
		archivedOnly: true,
		message:      "Restore " + accountText(s.Title) + " to the active list?\nOpen it from the rail afterward to resume.",
		action:       core.Action{Action: core.ActionArchive, ID: s.ID},
	}
}

func (m *model) renderArchivedDetails() string {
	w, h := m.mainWidth(), m.paneRows()
	lines := []string{titleStyle.Render("Archived session · read only"), ""}
	s := m.sessionByID(m.archiveSearch.details)
	if s == nil || !archivedSession(*s) {
		lines = append(lines, "This session is no longer in the archive.", "Esc: return to search")
	} else {
		runtime := s.Runtime
		if runtime == "" {
			runtime = s.Kind
		}
		if runtime == "" {
			runtime = "unknown"
		}
		lines = append(lines, "Title: "+accountText(s.Title), "ID: "+accountText(s.ID),
			"Runtime: "+accountText(runtime), "State: archived", "",
			"History is not available in this TUI.",
			"Restore before reopening from the active rail.", "",
			"Ctrl+R: restore · Esc: back to results")
	}
	for i := range lines {
		lines[i] = ansi.Wrap(lines[i], max(1, w-2), "")
	}
	return lipgloss.NewStyle().Width(w).Height(h).MaxWidth(w).MaxHeight(h).Render(strings.Join(lines, "\n"))
}

func (m *model) renderArchiveSearch() string {
	if m.archiveSearch.details != "" {
		return m.renderArchivedDetails()
	}
	w, h := m.mainWidth(), m.paneRows()
	rows := m.archivedMatches()
	i := m.archiveSelection(rows)
	lines := []string{titleStyle.Render("Search archived sessions"), "Filter: " + accountText(m.archiveSearch.query) + "▏", ""}
	visible := max(1, h-6)
	start := max(0, i-visible+1)
	for n := start; n < min(len(rows), start+visible); n++ {
		label := padTo("  "+accountText(rows[n].Title)+" · "+accountText(rows[n].ID), w)
		if n == i {
			label = selStyle.Render(label)
		}
		lines = append(lines, label)
	}
	if len(rows) == 0 {
		lines = append(lines, "No archived sessions match.")
	}
	lines = append(lines, "", "Enter: details · Ctrl+R: restore · Esc: close")
	for i := range lines {
		lines[i] = padTo(lines[i], w)
	}
	return lipgloss.NewStyle().Width(w).Height(h).MaxWidth(w).MaxHeight(h).Render(strings.Join(lines, "\n"))
}
