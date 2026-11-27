package nativetui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"amux/internal/core"
	"amux/internal/keymap"
	"amux/internal/store"
)

func (m *model) View() string {
	if m.frames.enabled && m.frames.viewValid {
		if m.hw != nil {
			m.hw.set(m.hardwareCursor(), true)
		}
		return m.lastView
	}
	v := m.view()
	if m.hw != nil {
		m.hw.set(m.hardwareCursor(), v == m.lastView)
	}
	m.lastView = v
	m.frames.viewValid = m.frames.enabled
	return v
}

func (m *model) view() string {
	if m.w == 0 || m.h == 0 {
		return "starting…"
	}
	// Everything amux draws folds to ASCII when asked; the embedded pane is the
	// agent's own output and passes through untouched.
	main := m.renderMain()
	if !m.mainIsPane() {
		main = foldASCII(main)
	}
	body := lipgloss.JoinHorizontal(lipgloss.Top, m.sidebar(), foldASCII(m.renderDivider()), main)
	view := strings.Join([]string{
		foldASCII(m.renderTopBorder()),
		body,
		foldASCII(m.renderBottomBorder()),
		foldASCII(cellPad(m.renderHelp(), m.w)),
	}, "\n")
	if m.accounts != nil {
		box := foldASCII(m.renderAccounts())
		return overlayCells(view, box, (m.w-lipgloss.Width(box))/2, (m.h-lipgloss.Height(box))/2, m.w, m.h)
	}
	return view
}

// overlayCells preserves the live pane outside the modal. Cut works in terminal
// cells (including wide glyphs) and preserves ANSI styles on both exposed edges.
func overlayCells(base, box string, x, y, width, height int) string {
	back, front := strings.Split(base, "\n"), strings.Split(box, "\n")
	lines := make([]string, height)
	for row := range lines {
		line := ""
		if row < len(back) {
			line = back[row]
		}
		line = cellPad(line, width)
		if row >= y && row-y < len(front) {
			fg := front[row-y]
			start, end := max(0, x), min(width, x+ansi.StringWidth(fg))
			if start < end {
				fg = cellSlice(fg, start-x, end-x)
				line = cellSlice(line, 0, start) + "\x1b[0m" + fg + "\x1b[0m" + cellSlice(line, end, width)
			}
		}
		lines[row] = line
	}
	return strings.Join(lines, "\n")
}

func cellSlice(s string, left, right int) string {
	// Cut includes a whole glyph straddling its left edge. Replace that glyph's
	// exposed cells with spaces so the suffix keeps its original columns.
	start := left
	for start < right && ansi.StringWidth(ansi.Truncate(s, start, "")) < start {
		start++
	}
	return cellPad(strings.Repeat(" ", start-left)+ansi.Cut(s, start, right), right-left)
}

func cellPad(s string, width int) string {
	s = ansi.Truncate(s, max(0, width), "")
	return s + strings.Repeat(" ", max(0, width-ansi.StringWidth(s)))
}

// renderTopBorder is the per-pane hint row, mirroring tmux's pane-border-format:
// above the rail, the shortcut to jump into the agent (anchored right, toward
// the divider); above the terminal, the shortcut back to the rail (anchored
// left, toward the divider). The hints are embedded in the ─ border so the line
// runs the full width, and the focused pane's border is drawn in the accent
// color.
func (m *model) renderTopBorder() string {
	label := titleStyle.Render(" amux ")
	if self := m.selfMetric(); self != "" && !m.compactRail() {
		label += dimStyle.Render(self + " ") // amux's own CPU/RSS (resources.go)
	}
	railHint := keyStyle.Render(" "+keyLabel(m.keys.Chord(keymap.FocusAgent))+" ") + dimStyle.Render("▸")
	left := borderSeg(m.railWidth(), m.borderStyle(focusSidebar), label, railHint)

	workHint := ""
	if m.attached != "" {
		// Lead with the back-to-rail shortcut (◂ ⌥ h by default, anchored toward
		// the divider on the left), then the agent's tab strip.
		workHint = dimStyle.Render("◂") + keyStyle.Render(" "+keyLabel(m.keys.Chord(keymap.FocusRail))+" ") + m.tabStrip()
	}
	// Reserve space for history state before truncating navigation hints. Even
	// a one-cell pane gets an arrow; no badge ever covers terminal content.
	badge := ""
	if t := m.cur(); t != nil {
		badge = strings.TrimSpace(t.Indicator())
		if ansi.StringWidth(badge) > m.mainWidth() {
			badge = "↑"
		}
	}
	if badge != "" {
		workHint = ansi.Truncate(workHint, max(0, m.mainWidth()-ansi.StringWidth(badge)), "")
		badge = keyStyle.Render(badge)
	}
	right := borderSeg(m.mainWidth(), m.borderStyle(focusAgent), workHint, badge)
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
	left := m.borderStyle(focusSidebar).Render(strings.Repeat("─", m.railWidth()))
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

// borderStyle is the focus color for the focused pane's border, the rule color
// otherwise.
func (m *model) borderStyle(target focus) lipgloss.Style {
	if m.focus == target {
		return focusStyle
	}
	return sepStyle
}

// borderSeg builds a width-wide line: `left` and `right` (pre-styled) anchored to
// the ends, with the gap between them filled by a ─ rule in fill's color.
func borderSeg(width int, fill lipgloss.Style, left, right string) string {
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 0 {
		return cellPad(left+right, width)
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

	entries := m.railEntries()
	if m.loading() {
		// No snapshot yet: an empty inventory is unknown, not empty, so the
		// "no workgroups" hints would mislead.
		entries = nil
		top = m.loadingRail()
	}
	for _, e := range entries {
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
							top = append(top, dimStyle.Render(" "+truncate(sec.empty, m.railWidth()-1)))
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
	// The attention summary leads the footer: mouse hit-testing counts the
	// account and hint rows up from the bottom, so nothing below it moves.
	foot := append(m.accountOverviewLines(), m.railHints()...)
	rows := m.paneRows()
	if sum := m.attentionSummary(); rows-lineCount(foot)-len(sum) >= minRailListRows {
		foot = append(sum, foot...) // a short rail keeps its list and hints instead
	}
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
	return foldASCII(lipgloss.NewStyle().
		Width(m.railWidth()).MaxWidth(m.railWidth()).MaxHeight(rows).
		Render(strings.Join(lines, "\n")))
}

// railHints is the pinned command help inside the rail: the switcher's own
// actions (navigation lives on the pane borders), embedded in a ─ border that
// runs to the divider (focus-colored).
func (m *model) railHints() []string {
	fill := m.borderStyle(focusSidebar)
	l1 := " " + keyStyle.Render(enterKey()) + dimStyle.Render(" open  ") + keyStyle.Render(upDownKeys()) + dimStyle.Render(" move ")
	return []string{
		borderSeg(m.railWidth(), fill, l1, ""),
		borderSeg(m.railWidth(), fill, " "+keyStyle.Render("Space")+dimStyle.Render(" fold/unfold "), ""),
		borderSeg(m.railWidth(), fill, " "+keyStyle.Render("/")+dimStyle.Render(" search archived "), ""),
	}
}

// minRailListRows is the list height the attention summary must leave.
const minRailListRows = 4

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
	rw := m.railWidth()
	selected := m.sectionCursor == "" && i == m.cursor
	indent := ""
	if s.RootID != "" {
		indent = " "
	}
	// The first column is the row's leading edge: the open session's bar, else
	// the selection's. Arrows stay with disclosure and discs with activity.
	mark := " "
	switch {
	case s.ID == m.attached:
		mark = glyphOpen
	case selected:
		mark = glyphCursor
	}
	icon, iconStyle := glyph(s), lipgloss.NewStyle()
	switch {
	case container(&s):
		// Reserve disclosure arrows for workgroups that actually have children
		// to fold and unfold. A childless workgroup takes the leaf bullet in the
		// arrow column so it still reads as a row.
		icon = leaf
		if m.groupHasChildren(&s) {
			icon = disclosure(m.collapsed[m.group(&s)])
		}
	case s.Kind == "repo":
		// A repo row never has children; keep the arrow column blank so its
		// ⛁ glyph lines up with the workgroup titles' structural glyphs.
		icon = "  " + glyph(s)
	case icon == stateGlyph(s.State):
		iconStyle = stateStyle(s.State)
	}
	lead := mark + indent + icon
	label := lead + " " + s.Title
	cell, cols := m.railMetric(&s) // fixed-width resource cell (resources.go)
	w := rw
	if cols {
		w = rw - metricWidth // pad below so the cell sits flush right
	}
	label = truncate(label, w)
	if cols {
		label += strings.Repeat(" ", max(0, w-ansi.StringWidth(label)))
	}
	var line string
	switch {
	case selected:
		line = selStyle.Width(rw).Render(label + cell.plain)
	case !strings.HasPrefix(label, lead):
		line = label + cell.styled // too narrow to hold even the lead
	default:
		title := label[len(lead):]
		if s.IsRoot {
			title = titleStyle.Render(title)
		}
		if mark != " " {
			mark = focusStyle.Render(mark)
		}
		line = mark + indent + iconStyle.Render(icon) + title + cell.styled
	}
	if sub := m.rowStatus(s, indent); sub != "" {
		line += "\n" + sub
	}
	return line
}

// rowStatus is the status sub-line shown beneath an agent row, or "" for rows
// with nothing to detail — bare containers, idle container sessions, and rows
// without a status. The state word leads it, styled by state (see statusLine);
// a workgroup with members waiting on the user says so first, so a folded
// group never hides them.
func (m *model) rowStatus(s core.Session, indent string) string {
	if s.Status == "" {
		return ""
	}
	// A bare container (no session of its own) has nothing to detail; a repo home
	// shows its status only while it is live, so a quiet rail isn't a column of
	// "stopped" under every repo header. A coordinator always shows its status:
	// a stopped coordinator must read as stopped, not blend into a calm rail.
	if (s.Kind == "repo" || s.IsRoot) && (s.Role == "" || (s.Kind == "repo" && core.IsStopped(s.State))) {
		return ""
	}
	var segs []segment
	waiting := m.waitingMembers(&s)
	if waiting > 0 {
		segs = append(segs, segment{fmt.Sprintf(" %s %d waiting ", glyphWaiting, waiting), attnStyle}, segment{" ", dimStyle})
	}
	// The title now carries the agent's task summary (or name); surface the id
	// beneath it so the session stays identifiable. Skip it when the title is
	// itself the id — a prompt-less agent whose title falls back to the short id
	// — and under a workgroup's waiting chip, which needs the room more.
	if waiting == 0 && s.Title != s.ID && !strings.HasPrefix(s.ID, s.Title) && !strings.Contains(s.Status, s.ID) {
		segs = append(segs, segment{s.ID + " · ", dimStyle})
	}
	segs = append(segs, m.sessionStatusSegments(s)...)
	return indent + "  " + renderSegments(segs, m.railWidth()-3-len(indent))
}

// waitingMembers counts a workgroup's live members that are waiting on the
// user; 0 for any other row.
func (m *model) waitingMembers(s *core.Session) int {
	if !container(s) {
		return 0
	}
	// A daemon that names who needs the user leads the coordinator's own
	// status with it ("waiting · 1 agent needs you"); a chip would repeat it.
	if s.Supervision != nil && len(s.Supervision.NeedsYou) > 0 {
		return 0
	}
	n := 0
	for i := range m.sessions {
		c := &m.sessions[i]
		if c.RootID == s.ID && c.Section == s.Section && c.State == core.StateWaiting && !archivedSession(*c) {
			n++
		}
	}
	return n
}

// segment is a run of text in one style, truncated as a unit of the line.
type segment struct {
	text  string
	style lipgloss.Style
}

// statusSegments styles a daemon status ("running · 2 agents"): the state word
// that leads it takes its state's color — waiting, the attention chip — and
// the rest stays readable neutral metadata. The word is always present, so the
// line reads the same with no color at all.
func statusSegments(status string) []segment {
	word, rest, _ := strings.Cut(status, " ")
	switch word {
	case core.StateWaiting:
		return []segment{{" " + word + " ", attnStyle}, {" " + rest, dimStyle}}
	case core.SubStateSupervising:
		// A coordinator whose members are working reads as running.
		return []segment{{word, stateStyle(core.StateRunning)}, {" " + rest, dimStyle}}
	case core.StateRunning, core.StateReady, core.StateUnknown, core.StateStopped, core.StateIdle:
		if rest == "" {
			return []segment{{word, stateStyle(word)}}
		}
		return []segment{{word, stateStyle(word)}, {" " + rest, dimStyle}}
	}
	return []segment{{status, dimStyle}}
}

// renderSegments renders segs within width cells, cutting (with an ellipsis)
// in whichever segment crosses the edge.
func renderSegments(segs []segment, width int) string {
	var b strings.Builder
	for _, sg := range segs {
		if width <= 0 {
			break
		}
		text := truncate(sg.text, width)
		width -= ansi.StringWidth(text)
		b.WriteString(sg.style.Render(text))
		if text != sg.text {
			break
		}
	}
	return b.String()
}

// attentionSummary is the rail's one-line count of workgroups by their most
// demanding state: how many wait on the user (the attention chip when any do),
// how many are running, how many are ready. It counts workgroups, not agents —
// the same aggregate each workgroup row already carries — and is omitted until
// there is a workgroup to count, and whenever the counts cannot be shown whole
// in the rail's width. A workgroup whose state is unknown is never
// reported as running: it gets its own unknown mark, shown only when there is
// one, so the running figure stays a statement about work known to be running.
func (m *model) attentionSummary() []string {
	if m.loading() {
		return nil
	}
	var total, waiting, running, ready, unknown int
	for i := range m.sessions {
		s := &m.sessions[i]
		if !s.IsRoot || s.Section != core.SectionWorkgroups || archivedSession(*s) {
			continue
		}
		total++
		switch s.State {
		case core.StateWaiting:
			waiting++
		case core.StateRunning:
			running++
		case core.StateReady:
			ready++
		case core.StateUnknown:
			unknown++
		}
	}
	if total == 0 {
		return nil
	}
	line, ok := summaryLine(waiting, running, ready, unknown, m.railWidth())
	if !ok {
		return nil
	}
	if q := m.queueSummaryLine(); q != "" {
		return []string{line, ansi.Truncate(q, m.railWidth(), "")}
	}
	return []string{line}
}

// summaryLine lays the counts out for width. Attention is spelled out; running
// and ready keep the glyph the rows above pair with their word. When the line
// does not fit it gives up, in order: the word "waiting", the categories whose
// count is zero, the double spaces, the space inside each figure, and last the
// space between figures. If even the tightest packing is wider than the rail it
// reports false and the caller omits the summary: a clipped count would misstate
// the fleet, and each workgroup row still carries its own state.
func summaryLine(waiting, running, ready, unknown, width int) (string, bool) {
	type figure struct {
		glyph string
		n     int
	}
	all := []figure{{glyphRunning, running}, {glyphReady, ready}}
	if unknown > 0 {
		all = append(all, figure{glyphUnknown, unknown})
	}
	var nonZero []figure
	for _, f := range all {
		if f.n > 0 {
			nonZero = append(nonZero, f)
		}
	}
	layout := func(word string, figs []figure, sep, gap string) (string, string) {
		wait := fmt.Sprintf(" %s%s%d%s", glyphWaiting, gap, waiting, word)
		var rest strings.Builder
		for _, f := range figs {
			rest.WriteString(sep + f.glyph + gap + strconv.Itoa(f.n))
		}
		// The chip keeps one cell of its own padding out of the first separator.
		if sep != "" && rest.Len() > 0 {
			return wait + " ", rest.String()[1:]
		}
		return wait + " ", rest.String()
	}
	var wait, rest string
	for _, try := range []struct {
		word     string
		figs     []figure
		sep, gap string
	}{
		{" waiting", all, "  ", " "},
		{"", all, "  ", " "},
		{"", nonZero, "  ", " "},
		{"", nonZero, " ", " "},
		{"", nonZero, " ", ""},
		{"", nonZero, "", ""},
	} {
		wait, rest = layout(try.word, try.figs, try.sep, try.gap)
		if ansi.StringWidth(wait+rest) <= width {
			break
		}
	}
	if ansi.StringWidth(wait+rest) > width {
		return "", false
	}
	waitStyle := dimStyle
	if waiting > 0 {
		waitStyle = attnStyle
	}
	return cellPad(waitStyle.Render(wait)+dimStyle.Render(rest), width), true
}

func (m *model) renderMain() string {
	m.shown.Store(m.cur()) // the pane allowed to copy (see model.clip)
	if m.archiveSearch != nil {
		return m.renderArchiveSearch()
	}
	if m.picker != nil {
		return m.renderPicker()
	}
	if m.form != nil {
		return m.renderForm()
	}
	if m.confirm != nil {
		return m.renderDialog()
	}
	if m.res.open {
		return m.renderResources()
	}
	if m.qp.open {
		return m.renderQueue()
	}
	if m.prPanel.open {
		return m.renderPRs()
	}
	if t := m.cur(); t != nil {
		return m.renderSelectedPane(t)
	}
	if m.load != loadReady {
		return m.renderLoadingMain()
	}
	return lipgloss.NewStyle().
		Width(m.mainWidth()).Height(m.paneRows()).
		Align(lipgloss.Center, lipgloss.Center).
		Render(dimStyle.Render("select an agent and press " + enterKey()))
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
	box := boxStyle().
		Padding(1, 2).Width(w).
		MaxWidth(m.mainWidth()).MaxHeight(m.paneRows()).
		Render(m.confirm.message + "\n\n" + keys)
	return lipgloss.Place(m.mainWidth(), m.paneRows(), lipgloss.Center, lipgloss.Center, box)
}

// renderHelp is the bottom line: a status message when there is one, the agent
// keys while the agent is focused, otherwise the rail command hints.
func (m *model) renderHelp() string {
	switch {
	case m.archiveSearch != nil:
		if m.archiveSearch.details != "" {
			return ansi.Truncate(hints("read only", []hint{{"Ctrl+R", "restore"}, {"Esc", "results"}}), m.w, "")
		}
		return ansi.Truncate(hints("archive", []hint{{upDownKeys(), "move"}, {"Enter", "details"}, {"Ctrl+R", "restore"}, {"Esc", "close"}}), m.w, "")
	case m.accounts != nil:
		if s := m.accounts; s.pending != "" {
			return hints("accounts", []hint{{"Esc", "close and disconnect"}})
		} else if s.confirm != nil {
			return hints("accounts", []hint{{"Enter", "confirm"}, {"Esc", "back"}})
		} else if s.attempt.active() && s.attempt.method == "setup-token" {
			return ansi.Truncate(hints("token hidden", []hint{{"Enter", "submit"}, {"Ctrl+U", "clear"}, {"Esc", "cancel"}}), m.w, "")
		} else if s.attempt.active() {
			return ansi.Truncate(hints("login", []hint{{"c", "cancel"}, {"PgUp/PgDn", "scroll"}, {"Esc", "cancel and close"}}), m.w, "")
		}
		return ansi.Truncate(hints("accounts", []hint{{"e", "subscription"}, {"a", "API key"}, {"x", "logout"}, {"r", "refresh"}, {"Esc", "close"}}), m.w, "")
	case m.prPanel.open:
		return ansi.Truncate(hints("pull requests", []hint{{upDownKeys(), "move"}, {"Enter/o", "open in browser"}, {"p/Esc", "close"}}), m.w, "")
	case m.res.open:
		return ansi.Truncate(hints("resources", []hint{{"s", "sort CPU/mem"}, {upDownKeys(), "scroll"}, {"t/Esc", "close"}}), m.w, "")
	case m.qp.open && m.qp.answer != nil:
		return ansi.Truncate(hints("answer", m.answerHints()), m.w, "")
	case m.qp.open:
		return ansi.Truncate(hints("questions", []hint{{upDownKeys(), "select"}, {"y/n", "allow/deny"}, {"1-9", "choose"}, {"PgUp/PgDn", "scroll"}, {"Enter", "answer"}, {"a", "ack item"}, {"d", "hide"}, {"i/Esc", "close"}}), m.w, "")
	case m.status != "":
		return dimStyle.Render(truncate(m.status, m.w))
	case m.focus == focusAgent:
		return hints("agent", []hint{
			{m.tabsHint(), "tabs"},
			{keyLabel(m.keys.Chord(keymap.FocusRail)), "rail"},
			{keyLabel(m.keys.Chord(keymap.ToggleFocus)), "toggle"},
			{keyLabel(m.keys.Chord(keymap.RestartAgent)), "restart"},
			{keyLabel(m.keys.Chord(keymap.Quit)), "quit"},
		})
	default:
		return ansi.Truncate(hints("", []hint{{"Space", "fold/unfold"}, {leftRightKeys(), "collapse/expand"}, {enterKey(), "open"}, {"a", "+agent"}, {"e", "repos"}, {"w", "+group"}, {"R", "+repo"}, {"m", "move"}, {"r", "rename"}, {"t", "top"}, {"p", "PRs"}, {"o", "open PR"}, {"i", "questions"}, {"x", "done"}, {"D", "del"}, {"q", "quit"}}), m.w, "")
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

// leaf marks a container with nothing to disclose. A hollow bullet sits in the
// disclosure column: it is single-cell, from the same Geometric Shapes block
// as the arrows and activity circles, and reads as "empty" without inviting a
// fold like ▸ would or looking like an idle agent's ○.
const leaf = "◦"

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
		return style.Width(m.railWidth()).Render(label)
	}
	hint := keys.Render(hotkey) + style.Render(" + ")
	gap := m.railWidth() - lipgloss.Width(label) - lipgloss.Width(hint)
	if gap < 1 {
		return style.Width(m.railWidth()).Render(label)
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

// glyph mirrors the rail: console ⚙, overseer ◉, repo ⛁, root ▸, external ↗, otherwise the
// activity state's own shape (see stateGlyph). task and interactive agents
// share the activity glyphs; the mode is surfaced in listings, not here.
// A container row keeps its structural glyph even when its own session (the
// coordinator, the repo home) is live — its status sub-line carries the state.
func glyph(s core.Session) string {
	switch {
	case s.Kind == "repo":
		return "⛁"
	case s.Mode == store.ModeConsole:
		return "⚙"
	case s.Mode == store.ModeOverseer:
		return "◉"
	case s.IsRoot:
		return "▸"
	case s.Mode == "external":
		return glyphExternal
	}
	return stateGlyph(s.State)
}

// truncate cuts s to at most n terminal cells, ending in an ellipsis when it
// had to cut. It measures cells, not runes, so wide (CJK, emoji) text cannot
// overrun the column it was fitted to.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if ansi.StringWidth(s) <= n {
		return s
	}
	return ansi.Truncate(s, n, "…")
}
