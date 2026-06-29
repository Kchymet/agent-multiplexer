package nativetui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"amux/internal/core"
)

var (
	accent       = lipgloss.Color("39") // cyan — amux's accent
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	sepStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(accent)
	selStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("231")).Background(lipgloss.Color("24"))
	headerStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("16")).Background(accent)
	sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252")).Background(lipgloss.Color("237"))
	keyStyle     = lipgloss.NewStyle().Foreground(accent)
)

func (m *model) View() string {
	if m.w == 0 || m.h == 0 {
		return "starting…"
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.renderSidebar(), m.renderDivider(), m.renderMain())
	return strings.Join([]string{
		m.renderTopBorder(),
		body,
		m.renderBottomBorder(),
		m.renderHelp(),
	}, "\n")
}

// renderTopBorder is the per-pane hint row, mirroring tmux's pane-border-format:
// above the rail, the shortcut to jump into the agent (anchored right, toward
// the divider); above the terminal, the shortcut back to the rail (anchored
// left, toward the divider). The hints are embedded in the ─ border so the line
// runs the full width, and the focused pane's border is drawn in the accent
// color.
func (m *model) renderTopBorder() string {
	label := titleStyle.Render(" amux ")
	railHint := keyStyle.Render(" ⌥ l ") + dimStyle.Render("▸")
	left := borderSeg(sidebarWidth, m.borderStyle(focusSidebar), label, railHint)

	workHint := ""
	if m.cur() != nil {
		workHint = dimStyle.Render("◂") + keyStyle.Render(" ⌥ h ")
	}
	right := borderSeg(m.mainWidth(), m.borderStyle(focusAgent), workHint, "")
	return left + sepStyle.Render("┬") + right
}

// renderBottomBorder closes the frame with a ┴ under the divider, each side in
// its pane's focus color.
func (m *model) renderBottomBorder() string {
	left := m.borderStyle(focusSidebar).Render(strings.Repeat("─", sidebarWidth))
	right := m.borderStyle(focusAgent).Render(strings.Repeat("─", m.mainWidth()))
	return left + sepStyle.Render("┴") + right
}

// renderDivider is the vertical rule between the sidebar and the agent pane,
// drawn in the focused pane's color.
func (m *model) renderDivider() string {
	style := m.borderStyle(focusSidebar)
	if m.focus == focusAgent {
		style = m.borderStyle(focusAgent)
	}
	col := style.Render("│")
	rows := make([]string, m.paneRows())
	for i := range rows {
		rows[i] = col
	}
	return strings.Join(rows, "\n")
}

// borderStyle is the accent color for the focused pane's border, grey otherwise.
func (m *model) borderStyle(target focus) lipgloss.Style {
	if m.focus == target {
		return lipgloss.NewStyle().Foreground(accent)
	}
	return sepStyle
}

// borderSeg builds a width-wide line: `left` and `right` (pre-styled) anchored to
// the ends, with the gap between them filled by a ─ rule in fill's color.
func borderSeg(width int, fill lipgloss.Style, left, right string) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		return padTo(left+right, width)
	}
	return left + fill.Render(strings.Repeat("─", gap)) + right
}

// padTo pads (or truncates) a possibly-styled string to exactly w cells.
func padTo(s string, w int) string {
	return lipgloss.NewStyle().Width(w).MaxWidth(w).Render(s)
}

func (m *model) renderSidebar() string {
	var top []string
	section := "\x00"
	for i, s := range m.sessions {
		if s.Section != section {
			section = s.Section
			if lbl := sectionLabel(section); lbl != "" {
				top = append(top, "") // blank line separates sections
				top = append(top, sectionStyle.Width(sidebarWidth).Render(lbl))
			}
		}
		top = append(top, m.renderRow(i, s))
	}

	// Pin the rail's command hints to the bottom, padding the gap between the
	// session list and the hints so they sit on the bottom border.
	foot := m.railHints()
	rows := m.paneRows()
	if len(top)+len(foot) > rows && rows-len(foot) >= 0 {
		top = top[:rows-len(foot)]
	}
	for len(top)+len(foot) < rows {
		top = append(top, "")
	}
	lines := append(top, foot...)
	return lipgloss.NewStyle().
		Width(sidebarWidth).MaxHeight(rows).
		Render(strings.Join(lines, "\n"))
}

// railHints is the pinned command help inside the rail: the switcher's own
// actions (navigation lives on the pane borders), embedded in a ─ border that
// runs to the divider (focus-colored).
func (m *model) railHints() []string {
	fill := m.borderStyle(focusSidebar)
	l1 := " " + keyStyle.Render("↵") + dimStyle.Render(" open  ") + keyStyle.Render("↑↓") + dimStyle.Render(" move ")
	return []string{
		borderSeg(sidebarWidth, fill, l1, ""),
	}
}

func (m *model) renderRow(i int, s core.Session) string {
	indent := ""
	if s.RootID != "" {
		indent = " "
	}
	mark := " "
	if s.ID == m.attached {
		mark = "▸" // currently embedded
	}
	label := truncate(fmt.Sprintf("%s%s%s %s", mark, indent, glyph(s), s.Title), sidebarWidth)
	switch {
	case i == m.cursor:
		return selStyle.Width(sidebarWidth).Render(label)
	case s.IsRoot:
		return titleStyle.Render(label)
	default:
		return label
	}
}

func (m *model) renderMain() string {
	if m.confirm != nil {
		return m.renderDialog()
	}
	if t := m.cur(); t != nil {
		return t.Render()
	}
	return lipgloss.NewStyle().
		Width(m.mainWidth()).Height(m.paneRows()).
		Align(lipgloss.Center, lipgloss.Center).
		Render(dimStyle.Render("select an agent and press ↵"))
}

// renderDialog draws a centered modal confirmation box in the main pane.
func (m *model) renderDialog() string {
	w := m.mainWidth() - 8
	if w > 48 {
		w = 48
	}
	if w < 12 {
		w = 12
	}
	keys := keyStyle.Render("Enter") + dimStyle.Render(" confirm   ") + keyStyle.Render("Esc") + dimStyle.Render(" cancel")
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(accent).
		Padding(1, 2).Width(w).
		Render(m.confirm.message + "\n\n" + keys)
	return lipgloss.Place(m.mainWidth(), m.paneRows(), lipgloss.Center, lipgloss.Center, box)
}

// renderHelp is the bottom line: a status message when there is one, the agent
// keys while the agent is focused, otherwise nothing (the rail carries its own
// command hints).
func (m *model) renderHelp() string {
	switch {
	case m.status != "":
		return dimStyle.Render(truncate(m.status, m.w))
	case m.focus == focusAgent:
		return hints("agent", []hint{{"⌥ h", "rail"}, {"⌥ a", "toggle"}, {"⌥ q", "quit"}})
	default:
		return ""
	}
}

type hint struct{ key, desc string }

// hints renders a " label · key desc · key desc " help line with keys accented.
func hints(label string, hs []hint) string {
	var b strings.Builder
	b.WriteByte(' ')
	if label != "" {
		b.WriteString(titleStyle.Render(label))
		b.WriteString(dimStyle.Render(" · "))
	}
	for i, h := range hs {
		if i > 0 {
			b.WriteString(dimStyle.Render(" · "))
		}
		b.WriteString(keyStyle.Render(h.key))
		b.WriteString(dimStyle.Render(" " + h.desc))
	}
	return b.String()
}

func sectionLabel(section string) string {
	switch section {
	case core.SectionWorkgroups:
		return " WORKGROUPS"
	case core.SectionRepos:
		return " REPOS"
	case core.SectionArchived:
		return " ARCHIVED"
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
