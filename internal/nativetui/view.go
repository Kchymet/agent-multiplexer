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
	if m.attached != "" {
		workHint = m.tabStrip()
	}
	right := borderSeg(m.mainWidth(), m.borderStyle(focusAgent), workHint, "")
	return left + sepStyle.Render("┬") + right
}

// tabStrip renders the attached agent's tab row (Alt+1/2/3): the active tab is
// highlighted, launched-but-inactive tabs are bright, unopened ones dim.
func (m *model) tabStrip() string {
	var b strings.Builder
	b.WriteString(" ")
	for i := 0; i < tabCount; i++ {
		if i > 0 {
			b.WriteString(dimStyle.Render("  "))
		}
		num := fmt.Sprintf("%d", i+1)
		label := num + " " + tabNames[i]
		switch {
		case i == m.tab:
			b.WriteString(selStyle.Render(" " + label + " "))
		case m.terms[paneKey{m.attached, i}] != nil:
			b.WriteString(keyStyle.Render(num) + " " + tabNames[i])
		default:
			b.WriteString(keyStyle.Render(num) + dimStyle.Render(" "+tabNames[i]))
		}
	}
	b.WriteString(" ")
	return b.String()
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

	// Pinned, sectionless rows first (the control console).
	for i, s := range m.sessions {
		if s.Section == "" {
			top = append(top, m.renderRow(i, s))
		}
	}

	// WORKGROUPS and REPOS are always shown — with an empty-state hint when they
	// have no rows — so creating the first one is discoverable. ARCHIVED and
	// DETACHED only appear when populated (they have nothing to create).
	for _, sec := range []struct{ key, empty string }{
		{core.SectionWorkgroups, "no workgroups — w to create"},
		{core.SectionRepos, "no repos — R to add"},
		{core.SectionArchived, ""},
		{core.SectionDetached, ""},
	} {
		any := false
		for i, s := range m.sessions {
			if s.Section == sec.key {
				if !any {
					top = append(top, "", sectionStyle.Width(sidebarWidth).Render(sectionLabel(sec.key)))
					any = true
				}
				top = append(top, m.renderRow(i, s))
			}
		}
		if !any && sec.empty != "" {
			top = append(top, "", sectionStyle.Width(sidebarWidth).Render(sectionLabel(sec.key)))
			top = append(top, dimStyle.Render(" "+truncate(sec.empty, sidebarWidth-1)))
		}
	}

	// Pin the rail's command hints to the bottom, padding the gap between the
	// session list and the hints so they sit on the bottom border. Rows can be two
	// lines tall (title + status sub-line), so we count rendered lines, not slots.
	foot := m.railHints()
	rows := m.paneRows()
	footLines := lineCount(foot)
	for lineCount(top) > rows-footLines {
		top = top[:len(top)-1] // drop overflow rows; MaxHeight is the final backstop
	}
	for lineCount(top)+footLines < rows {
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

// lineCount totals the rendered lines across entries, each of which may itself
// span multiple lines (a row with a status sub-line).
func lineCount(entries []string) int {
	n := 0
	for _, e := range entries {
		n += 1 + strings.Count(e, "\n")
	}
	return n
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
	var line string
	switch {
	case i == m.cursor:
		line = selStyle.Width(sidebarWidth).Render(label)
	case s.IsRoot:
		line = titleStyle.Render(label)
	default:
		line = label
	}
	if sub := m.rowStatus(s, indent); sub != "" {
		line += "\n" + sub
	}
	return line
}

// rowStatus is the state-colored status sub-line shown beneath an agent row
// (green working, purple awaiting you, blue ready, dim idle), or "" for rows
// with nothing to detail — repos, containers, and rows without a status.
func (m *model) rowStatus(s core.Session, indent string) string {
	if s.Kind == "repo" || s.IsRoot || s.Status == "" {
		return ""
	}
	sub := s.Status
	if s.Title != s.ID && !strings.Contains(s.Status, s.ID) {
		sub = s.ID + " · " + s.Status // leaf rows show their short id
	}
	return stateColor(s.State).Render(indent + "  " + truncate(sub, sidebarWidth-3-len(indent)))
}

// stateColor styles a status sub-line by activity: green working, purple blocked
// on the user, blue ready, yellow live-but-unknown, dim idle.
func stateColor(state string) lipgloss.Style {
	switch state {
	case core.StateRunning:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("114")) // green
	case core.StateWaiting:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("141")) // purple
	case core.StateReady:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("39")) // blue
	case core.StateUnknown:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("220")) // yellow
	default:
		return dimStyle // idle
	}
}

func (m *model) renderMain() string {
	if m.form != nil {
		return m.renderForm()
	}
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
		return hints("agent", []hint{{"⌥ 1/2/3", "tabs"}, {"⌥ h", "rail"}, {"⌥ a", "toggle"}, {"⌥ q", "quit"}})
	default:
		return hints("", []hint{{"↵", "open"}, {"a", "+agent"}, {"w", "+group"}, {"R", "+repo"}, {"m", "move"}, {"r", "rename"}, {"x", "done"}, {"D", "del"}, {"q", "quit"}})
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
