package nativetui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"amux/internal/core"
	"amux/internal/keymap"
	"amux/internal/store"
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
	// sectionKeyStyle accents a hotkey inside a section bar, keeping the bar's
	// background so the header reads as one solid strip.
	sectionKeyStyle = keyStyle.Bold(true).Background(lipgloss.Color("237"))
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
	railHint := keyStyle.Render(" "+keyLabel(m.keys.Chord(keymap.FocusAgent))+" ") + dimStyle.Render("▸")
	left := borderSeg(sidebarWidth, m.borderStyle(focusSidebar), label, railHint)

	workHint := ""
	if m.attached != "" {
		// Lead with the back-to-rail shortcut (◂ ⌥ h by default, anchored toward
		// the divider on the left), then the agent's tab strip.
		workHint = dimStyle.Render("◂") + keyStyle.Render(" "+keyLabel(m.keys.Chord(keymap.FocusRail))+" ") + m.tabStrip()
	}
	right := borderSeg(m.mainWidth(), m.borderStyle(focusAgent), workHint, "")
	return left + sepStyle.Render("┬") + right
}

// tabStrip renders the attached agent's tab row (Alt+1/2/3 by default): the
// active tab is highlighted, launched-but-inactive tabs are bright, unopened
// ones dim. Each tab's key hint is the base key of its configured chord.
func (m *model) tabStrip() string {
	var b strings.Builder
	b.WriteString(" ")
	for i := 0; i < tabCount; i++ {
		if i > 0 {
			b.WriteString(dimStyle.Render("  "))
		}
		num := chordBase(m.keys.Chord(tabActions[i]))
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
	cursorEntry := -1 // index into top of the selected row, for scroll-follow

	for _, e := range m.railEntries() {
		if e.section != "" {
			top = append(top, "")
			if m.railSelected(e) {
				cursorEntry = len(top)
			}
			top = append(top, m.sectionHeader(e.section))
			if !m.collapsed[railGroup{section: e.section}] {
				populated := false
				for _, s := range m.sessions {
					if s.Section == e.section {
						populated = true
						break
					}
				}
				if !populated {
					for _, sec := range railSections {
						if sec.key == e.section && sec.empty != "" {
							top = append(top, dimStyle.Render(" "+truncate(sec.empty, sidebarWidth-1)))
						}
					}
				}
			}
		} else {
			if m.railSelected(e) {
				cursorEntry = len(top)
			}
			top = append(top, m.renderRow(e.index, m.sessions[e.index]))
		}
	}

	// Pin the rail's command hints to the bottom. The session list occupies the
	// space above them; when it overflows we scroll it (keeping the selected row
	// visible) rather than dropping rows off the bottom. Rows can be two lines tall
	// (title + status sub-line), so we work in rendered lines, not slots.
	foot := m.railHints()
	rows := m.paneRows()
	footLines := lineCount(foot)
	avail := rows - footLines // lines available to the scrollable session list

	// Flatten entries to lines, tracking where the selected row lands so we can
	// scroll it into view.
	var body []string
	cursorStart, cursorSpan := 0, 0
	for i, e := range top {
		n := 1 + strings.Count(e, "\n")
		if i == cursorEntry {
			cursorStart = len(body)
			cursorSpan = n
		}
		body = append(body, strings.Split(e, "\n")...)
	}

	if avail < 1 {
		avail = 1
	}
	// Keep the selected row within the viewport, then clamp to the valid range.
	if cursorEntry >= 0 {
		if cursorStart < m.railScroll {
			m.railScroll = cursorStart
		}
		if cursorStart+cursorSpan > m.railScroll+avail {
			m.railScroll = cursorStart + cursorSpan - avail
		}
	}
	if max := len(body) - avail; m.railScroll > max {
		m.railScroll = max
	}
	if m.railScroll < 0 {
		m.railScroll = 0
	}

	end := m.railScroll + avail
	if end > len(body) {
		end = len(body)
	}
	visible := append([]string(nil), body[m.railScroll:end]...)
	for len(visible) < avail {
		visible = append(visible, "")
	}
	lines := append(visible, foot...)
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
		borderSeg(sidebarWidth, fill, " "+keyStyle.Render("Space")+dimStyle.Render(" fold/unfold "), ""),
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
		mark = "▌" // open session; keep arrows for disclosure and circles for activity
	}
	icon := glyph(s)
	if container(&s) {
		icon = disclosure(m.collapsed[m.group(&s)])
		if s.Kind == "repo" {
			icon += " " + glyph(s)
		}
	}
	label := truncate(fmt.Sprintf("%s%s%s %s", mark, indent, icon, s.Title), sidebarWidth)
	var line string
	switch {
	case m.sectionCursor == "" && i == m.cursor:
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
// with nothing to detail — bare containers, idle container sessions, and rows
// without a status.
func (m *model) rowStatus(s core.Session, indent string) string {
	if s.Status == "" {
		return ""
	}
	// A bare container (no session of its own) has nothing to detail; a container
	// that is a default session (a coordinator, a repo home) shows its status only
	// while it is live, so an idle rail isn't a column of "idle" under every header.
	if (s.Kind == "repo" || s.IsRoot) && (s.Role == "" || s.State == core.StateIdle) {
		return ""
	}
	sub := s.Status
	// The title now carries the agent's task summary (or name); surface the id
	// beneath it so the session stays identifiable. Skip it when the title is
	// itself the id — a prompt-less agent whose title falls back to the short id.
	if s.Title != s.ID && !strings.HasPrefix(s.ID, s.Title) && !strings.Contains(s.Status, s.ID) {
		sub = s.ID + " · " + s.Status
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
	if m.picker != nil {
		return m.renderPicker()
	}
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
		MaxWidth(m.mainWidth()).MaxHeight(m.paneRows()).
		Render(m.confirm.message + "\n\n" + keys)
	return lipgloss.Place(m.mainWidth(), m.paneRows(), lipgloss.Center, lipgloss.Center, box)
}

// renderHelp is the bottom line: a status message when there is one, the agent
// keys while the agent is focused, otherwise the rail command hints.
func (m *model) renderHelp() string {
	switch {
	case m.status != "":
		return dimStyle.Render(truncate(m.status, m.w))
	case m.focus == focusAgent:
		return hints("agent", []hint{
			{m.tabsHint(), "tabs"},
			{keyLabel(m.keys.Chord(keymap.FocusRail)), "rail"},
			{keyLabel(m.keys.Chord(keymap.ToggleFocus)), "toggle"},
			{keyLabel(m.keys.Chord(keymap.Quit)), "quit"},
		})
	default:
		return ansi.Truncate(hints("", []hint{{"Space", "fold/unfold"}, {"←/→", "collapse/expand"}, {"↵", "open"}, {"a", "+agent"}, {"e", "repos"}, {"w", "+group"}, {"R", "+repo"}, {"m", "move"}, {"r", "rename"}, {"x", "done"}, {"D", "del"}, {"q", "quit"}}), m.w, "")
	}
}

// tabActions maps a tab index to its keymap action.
var tabActions = [tabCount]string{keymap.TabAgent, keymap.TabEditor, keymap.TabTerm}

// tabsHint is the compact help label for the three tab chords: "⌥ 1/2/3" when
// they share modifiers, otherwise the full chords joined with "/".
func (m *model) tabsHint() string {
	c := [tabCount]string{}
	for i, a := range tabActions {
		c[i] = m.keys.Chord(a)
	}
	p0 := chordPrefix(c[0])
	if p0 != "" && chordPrefix(c[1]) == p0 && chordPrefix(c[2]) == p0 {
		return keyLabel(p0) + chordBase(c[0]) + "/" + chordBase(c[1]) + "/" + chordBase(c[2])
	}
	return keyLabel(c[0]) + "/" + keyLabel(c[1]) + "/" + keyLabel(c[2])
}

// keyLabel renders a chord for the UI: "alt+" as the ⌥ glyph, "ctrl+" as ^.
func keyLabel(chord string) string {
	s := strings.ReplaceAll(chord, "alt+", "⌥ ")
	return strings.ReplaceAll(s, "ctrl+", "^")
}

// chordPrefix is everything up to and including the last "+" ("" for a bare key).
func chordPrefix(chord string) string {
	if i := strings.LastIndex(chord, "+"); i >= 0 {
		return chord[:i+1]
	}
	return ""
}

// chordBase is the key after the modifiers ("alt+2" → "2").
func chordBase(chord string) string {
	return chord[len(chordPrefix(chord)):]
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

func disclosure(collapsed bool) string {
	if collapsed {
		return "▸"
	}
	return "▾"
}

// Section headers participate in cursor navigation and retain their create hint.
func (m *model) sectionHeader(section string) string {
	label := " " + disclosure(m.collapsed[railGroup{section: section}]) + sectionLabel(section)
	style, keys := sectionStyle, sectionKeyStyle
	if m.sectionCursor == section {
		style, keys = selStyle, selStyle
	}
	hotkey := ""
	for _, sec := range railSections {
		if sec.key == section {
			hotkey = sec.hotkey
		}
	}
	if hotkey == "" {
		return style.Width(sidebarWidth).Render(label)
	}
	hint := keys.Render(hotkey) + style.Render(" + ")
	gap := sidebarWidth - lipgloss.Width(label) - lipgloss.Width(hint)
	if gap < 1 {
		return style.Width(sidebarWidth).Render(label)
	}
	return style.Render(label+strings.Repeat(" ", gap)) + hint
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

// glyph mirrors the rail: console ⚙, repo ⛁, root ▸, external ◇, otherwise the
// activity state — running/waiting ●, ready ◐, idle ○. task and interactive
// agents share the activity glyphs; the mode is surfaced in listings, not here.
// A container row keeps its structural glyph even when its own session (the
// coordinator, the repo home) is live — its status sub-line carries the state.
func glyph(s core.Session) string {
	switch {
	case s.Kind == "repo":
		return "⛁"
	case s.Mode == store.ModeConsole:
		return "⚙"
	case s.IsRoot:
		return "▸"
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
