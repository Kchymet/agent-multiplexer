package tui

import (
	"fmt"
	"strings"

	"amux/internal/core"
)

func (m *model) View() string {
	if m.full {
		return m.viewFull()
	}
	return m.viewRail()
}

// viewRail renders the compact, always-visible side pane.
func (m *model) viewRail() string {
	w := m.width
	if w <= 0 {
		w = core.RailWidth
	}
	var b strings.Builder
	b.WriteString(headerStyle.Width(w).Render(" amux · workspaces"))
	b.WriteByte('\n')

	if len(m.sessions) == 0 {
		b.WriteString(dimStyle.Render(" no workspaces\n press n to create"))
		b.WriteByte('\n')
	}
	for i, s := range m.sessions {
		indent := ""
		if s.RootID != "" { // sub-session: nest under its root
			indent = "  "
		}
		line := fmt.Sprintf("%s%s %s", indent, glyph(s), truncate(s.Title, w-4-len(indent)))
		switch {
		case i == m.cursor:
			b.WriteString(selStyle.Width(w).Render(line))
		case s.IsRoot:
			b.WriteString(titleStyle.Render(line))
		default:
			b.WriteString(line)
		}
		b.WriteByte('\n')
		if !s.IsRoot { // status sub-line for leaf rows
			sub := s.Status
			if s.Title != s.ID && !strings.Contains(s.Status, s.ID) {
				sub = s.ID + " · " + s.Status
			}
			b.WriteString(stateColor(s.State).Render(indent + "   " + truncate(sub, w-6)))
			b.WriteByte('\n')
		}
	}

	b.WriteByte('\n')
	b.WriteString(dimStyle.Render(strings.Repeat("─", w)))
	b.WriteByte('\n')
	b.WriteString(dimStyle.Render(" ↵ open   n new   a +agent\n x×2 del  R ↻"))
	b.WriteByte('\n')
	b.WriteString(m.statusLine(w))
	return b.String()
}

// viewFull renders the full-screen dashboard.
func (m *model) viewFull() string {
	w := m.width
	if w <= 0 {
		w = 100
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render("amux") + dimStyle.Render("  —  workspace control plane"))
	b.WriteString("\n\n")

	header := fmt.Sprintf("  %-2s %-8s %-20s %-6s %-8s %s", "", "ID", "WORKSPACE", "MODE", "AGENT", "STATUS")
	b.WriteString(headerStyle.Width(w).Render(header))
	b.WriteByte('\n')

	if len(m.sessions) == 0 {
		b.WriteString(dimStyle.Render("\n  no workspaces — press n to create one\n"))
	}
	for i, s := range m.sessions {
		cursor := "  "
		if i == m.cursor {
			cursor = "▶ "
		}
		name := s.Title
		if name == s.ID {
			name = "—"
		}
		row := fmt.Sprintf("%s%-2s %-8s %-20s %-6s %-8s %s",
			cursor, glyph(s),
			truncate(s.ID, 8),
			truncate(name, 20),
			truncate(s.Mode, 6),
			truncate(s.Kind, 8),
			truncate(s.Status, max(0, w-50)),
		)
		if i == m.cursor {
			b.WriteString(selStyle.Width(w).Render(row))
		} else {
			b.WriteString(row)
		}
		b.WriteByte('\n')
	}

	b.WriteString("\n")
	b.WriteString(dimStyle.Render("  ↵ open   n new   x x delete   R refresh   q quit\n"))
	b.WriteString("  " + m.statusLine(w))
	return b.String()
}

func (m *model) statusLine(w int) string {
	s := m.status
	if strings.HasPrefix(s, "✗") {
		return errStyle.Render(truncate(s, w))
	}
	return dimStyle.Render(truncate(s, w))
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
