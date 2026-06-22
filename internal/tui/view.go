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

// viewRail renders the compact, always-visible side pane: a pinned console row,
// then the workspace switcher, tracked repos, and detached sessions — each under
// its own section header.
func (m *model) viewRail() string {
	w := m.width
	if w <= 0 {
		w = core.RailWidth
	}
	var b strings.Builder
	b.WriteString(headerStyle.Width(w).Render(" amux"))
	b.WriteByte('\n')

	hi := m.highlight()

	// Pinned, sectionless rows first (the control console).
	for i, s := range m.sessions {
		if s.Section == "" {
			m.writeRailRow(&b, i, hi, s, w)
		}
	}

	// The three sections, always shown in order and visually separated by a
	// blank line and a full-width header bar — even when a section is empty.
	for _, sec := range []struct{ key, label, empty string }{
		{core.SectionWorkspaces, " WORKSPACES", "no workspaces — n to create"},
		{core.SectionRepos, " REPOS", "no repos — r to add"},
		{core.SectionDetached, " DETACHED", "none"},
	} {
		b.WriteByte('\n')
		b.WriteString(sectionStyle.Width(w).Render(sec.label))
		b.WriteByte('\n')
		any := false
		for i, s := range m.sessions {
			if s.Section == sec.key {
				m.writeRailRow(&b, i, hi, s, w)
				any = true
			}
		}
		if !any {
			b.WriteString(dimStyle.Render("   " + truncate(sec.empty, w-3)))
			b.WriteByte('\n')
		}
	}

	b.WriteByte('\n')
	b.WriteString(dimStyle.Render(strings.Repeat("─", w)))
	b.WriteByte('\n')
	b.WriteString(dimStyle.Render(" ↵ open  n new  a +agent\n r +repo  x×2 del  R ↻"))
	b.WriteByte('\n')
	b.WriteString(m.statusLine(w))
	return b.String()
}

// writeRailRow renders one row: a glyph + title line, and (for everything but a
// repo) a colored status sub-line beneath it. hi is the index of the highlighted
// row (the focused agent, or the cursor while scrolling the switcher).
func (m *model) writeRailRow(b *strings.Builder, i, hi int, s core.Session, w int) {
	indent := ""
	if s.RootID != "" { // sub-session: nest under its workspace
		indent = "  "
	}
	line := fmt.Sprintf("%s%s %s", indent, glyph(s), truncate(s.Title, w-4-len(indent)))
	switch {
	case i == hi:
		b.WriteString(selStyle.Width(w).Render(line))
	case s.IsRoot:
		b.WriteString(titleStyle.Render(line))
	default:
		b.WriteString(line)
	}
	b.WriteByte('\n')

	if s.Section == core.SectionRepos || s.Status == "" {
		return // repos are a single line; nothing to detail
	}
	sub := s.Status
	if !s.IsRoot && s.Title != s.ID && !strings.Contains(s.Status, s.ID) {
		sub = s.ID + " · " + s.Status // leaf rows show their short id
	}
	b.WriteString(stateColor(s.State).Render(indent + "   " + truncate(sub, w-6)))
	b.WriteByte('\n')
}

// sectionLabel is the header shown above a section, or "" for the pinned console.
func sectionLabel(section string) string {
	switch section {
	case core.SectionWorkspaces:
		return " WORKSPACES"
	case core.SectionRepos:
		return " REPOS"
	case core.SectionDetached:
		return " DETACHED"
	}
	return ""
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
	hi := m.highlight()
	section := ""
	for i, s := range m.sessions {
		if s.Section != section {
			section = s.Section
			if label := sectionLabel(section); label != "" {
				b.WriteByte('\n')
				b.WriteString(sectionStyle.Width(w).Render(label))
				b.WriteByte('\n')
			}
		}
		cursor := "  "
		if i == hi {
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
		if i == hi {
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
