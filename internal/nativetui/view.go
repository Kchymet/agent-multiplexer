package nativetui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"amux/internal/core"
)

var (
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	selStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(lipgloss.Color("24"))
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(lipgloss.Color("24"))
	sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252")).Background(lipgloss.Color("237"))
)

func (m *model) View() string {
	if m.w == 0 {
		return "starting…"
	}
	row := lipgloss.JoinHorizontal(lipgloss.Top, m.renderSidebar(), m.renderMain())
	return row + "\n" + m.renderFooter()
}

func (m *model) renderSidebar() string {
	lines := []string{headerStyle.Width(sidebarWidth).Render(" amux")}
	section := "\x00"
	for i, s := range m.sessions {
		if s.Section != section {
			section = s.Section
			if lbl := sectionLabel(section); lbl != "" {
				lines = append(lines, sectionStyle.Width(sidebarWidth).Render(lbl))
			}
		}
		lines = append(lines, m.renderRow(i, s))
	}
	return lipgloss.NewStyle().
		Width(sidebarWidth).Height(m.paneRows()).MaxHeight(m.paneRows()).
		Render(strings.Join(lines, "\n"))
}

func (m *model) renderRow(i int, s core.Session) string {
	indent := ""
	if s.RootID != "" {
		indent = " "
	}
	mark := " "
	if s.ID == m.attached {
		mark = "▸"
	}
	label := truncate(fmt.Sprintf("%s%s%s %s", mark, indent, glyph(s), s.Title), sidebarWidth)
	if i == m.cursor {
		return selStyle.Width(sidebarWidth).Render(label)
	}
	if s.IsRoot {
		return lipgloss.NewStyle().Bold(true).Render(label)
	}
	return label
}

func (m *model) renderMain() string {
	if m.term == nil {
		return lipgloss.NewStyle().
			Width(m.mainWidth()).Height(m.paneRows()).
			Align(lipgloss.Center, lipgloss.Center).
			Render(dimStyle.Render("select an agent and press ↵"))
	}
	return m.term.Render()
}

func (m *model) renderFooter() string {
	if m.status != "" {
		return dimStyle.Render(truncate(m.status, m.w))
	}
	h := " ↑/↓ move · ↵ open · tab → agent · q quit"
	if m.focus == focusAgent {
		h = " agent focus · C-a → switcher"
	}
	return dimStyle.Render(truncate(h, m.w))
}

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

// glyph mirrors the rail: console ⚙, repo ⛁, root ▸, loop ∞, external ◇,
// otherwise the activity state — running/waiting ●, ready ◐, idle ○.
func glyph(s core.Session) string {
	switch {
	case s.Kind == "repo":
		return "⛁"
	case s.Mode == "console":
		return "⚙"
	case s.IsRoot:
		return "▸"
	case s.Mode == "loop":
		return "∞"
	case s.Mode == "external":
		return "◇"
	}
	switch s.State {
	case core.StateRunning, core.StateWaiting, core.StateUnknown:
		return "●"
	case core.StateReady:
		return "◐"
	default:
		return "○"
	}
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
