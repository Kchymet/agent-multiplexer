package nativetui

import (
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"amux/internal/core"
)

// trackNewID is the sentinel id of the picker's trailing "track a new repo" row.
// It's not a real repo — choosing it drops into the add-repo form so a brand-new
// repo can be tracked and then fuzzy-selected on the next open.
const trackNewID = "\x00track-new"

// repoItem is one selectable repo in the picker: its tracked name plus the source
// it was added from (shown dimmed as a hint).
type repoItem struct {
	name   string
	source string
}

// pickerState is the fuzzy repo picker modal: a filter buffer over a list of
// tracked repos with multi-select, plus a target describing what to do on submit.
// Exactly one of field / (action,id) is set:
//   - field != nil  → write the chosen names back into that form field (the
//     picker was opened from a create form's repos field).
//   - action != ""  → dispatch action with the chosen repos (e.g. agent-set-repos
//     to re-scope an existing agent).
type pickerState struct {
	title    string
	filter   string
	items    []repoItem
	selected map[string]bool
	cursor   int // index into the currently-filtered list

	field  *formField // write target, or nil
	action string     // dispatch target, or ""
	id     string     // session id for the dispatched action
}

// newRepoPicker builds a picker over the repos currently in the rail snapshot
// (the SectionRepos rows), pre-selecting the names in seed.
func newRepoPicker(title string, sessions []core.Session, seed []string) *pickerState {
	var items []repoItem
	for i := range sessions {
		s := &sessions[i]
		if s.Kind == "repo" {
			items = append(items, repoItem{name: s.ID, source: repoHint(s)})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	sel := map[string]bool{}
	for _, r := range seed {
		if r = strings.TrimSpace(r); r != "" {
			sel[r] = true
		}
	}
	return &pickerState{title: title, items: items, selected: sel}
}

// repoHint pulls a short source/label hint off a repo rail row for the dim
// right-hand annotation. The rail Title already carries a friendly label.
func repoHint(s *core.Session) string {
	if s.Title != "" && s.Title != s.ID {
		return s.Title
	}
	return ""
}

// filtered returns the repo rows matching the current filter (fuzzy), followed
// by the always-present "track a new repo" row.
func (p *pickerState) filtered() []repoItem {
	var out []repoItem
	for _, it := range p.items {
		if fuzzyMatch(p.filter, it.name) || fuzzyMatch(p.filter, it.source) {
			out = append(out, it)
		}
	}
	out = append(out, repoItem{name: trackNewID, source: "＋ track a new repo…"})
	return out
}

func (p *pickerState) clampCursor(n int) {
	if p.cursor < 0 {
		p.cursor = 0
	}
	if p.cursor > n-1 {
		p.cursor = n - 1
	}
}

// chosen returns the selected repo names in a stable order.
func (p *pickerState) chosen() []string {
	var out []string
	for _, it := range p.items {
		if p.selected[it.name] {
			out = append(out, it.name)
		}
	}
	return out
}

// fuzzyMatch reports whether every rune of query appears in cand in order
// (case-insensitive subsequence match). An empty query matches everything.
func fuzzyMatch(query, cand string) bool {
	if query == "" {
		return true
	}
	q := []rune(strings.ToLower(query))
	c := []rune(strings.ToLower(cand))
	i := 0
	for _, r := range c {
		if i < len(q) && r == q[i] {
			i++
		}
	}
	return i == len(q)
}

// openRepoPicker opens the fuzzy picker for the given target, seeded with any
// currently-selected repos. Called both from a form field and from the rail's
// edit-repos key.
func (m *model) openRepoPicker(p *pickerState) {
	m.picker = p
}

// refreshPickerItems rebuilds the picker's repo list from the latest rail
// snapshot, preserving the current selection. This keeps the list live so a repo
// tracked via the "track new" row appears once the daemon reports it.
func (m *model) refreshPickerItems() {
	p := m.picker
	if p == nil {
		return
	}
	var items []repoItem
	for i := range m.sessions {
		s := &m.sessions[i]
		if s.Kind == "repo" {
			items = append(items, repoItem{name: s.ID, source: repoHint(s)})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	p.items = items
}

// handlePicker processes a keystroke while the repo picker modal is open.
func (m *model) handlePicker(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.refreshPickerItems()
	p := m.picker
	rows := p.filtered()
	p.clampCursor(len(rows))

	switch k.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.picker = nil
		return m, nil
	case tea.KeyUp, tea.KeyCtrlP:
		p.cursor--
		p.clampCursor(len(rows))
		return m, nil
	case tea.KeyDown, tea.KeyCtrlN:
		p.cursor++
		p.clampCursor(len(rows))
		return m, nil
	case tea.KeyTab, tea.KeySpace:
		m.pickerToggle(rows)
		return m, nil
	case tea.KeyEnter:
		return m.pickerActivate(rows)
	case tea.KeyBackspace:
		if r := []rune(p.filter); len(r) > 0 {
			p.filter = string(r[:len(r)-1])
			p.cursor = 0
		}
		return m, nil
	case tea.KeyRunes:
		p.filter += string(k.Runes)
		p.cursor = 0
		return m, nil
	}
	return m, nil
}

// pickerToggle flips the selection of the row under the cursor (the track-new
// row can't be toggled — it's activated with Enter).
func (m *model) pickerToggle(rows []repoItem) {
	p := m.picker
	if p.cursor < 0 || p.cursor >= len(rows) {
		return
	}
	it := rows[p.cursor]
	if it.name == trackNewID {
		return
	}
	if p.selected[it.name] {
		delete(p.selected, it.name)
	} else {
		p.selected[it.name] = true
	}
}

// pickerActivate handles Enter: on the track-new row it opens the add-repo form;
// otherwise it commits the current selection to the picker's target.
func (m *model) pickerActivate(rows []repoItem) (tea.Model, tea.Cmd) {
	p := m.picker
	if p.cursor >= 0 && p.cursor < len(rows) && rows[p.cursor].name == trackNewID {
		// Keep the picker's target so we can reopen it after tracking; the add-repo
		// form submits an add-repo action and the next snapshot carries the new repo.
		m.pendingPicker = p
		m.picker = nil
		m.openAddRepoForm()
		return m, nil
	}
	return m.commitPicker()
}

// commitPicker applies the picker's selection to its target and closes it.
func (m *model) commitPicker() (tea.Model, tea.Cmd) {
	p := m.picker
	m.picker = nil
	names := p.chosen()
	joined := strings.Join(names, ",")
	if p.field != nil {
		p.field.value = joined
		return m, nil
	}
	if p.action != "" {
		m.status = "updating repos…"
		return m, m.sendCmd(core.Action{Action: p.action, ID: p.id, Fields: map[string]string{"repos": joined}})
	}
	return m, nil
}

// renderPicker draws the fuzzy repo picker as a centered modal.
func (m *model) renderPicker() string {
	m.refreshPickerItems()
	p := m.picker
	w := m.mainWidth() - 8
	if w > 58 {
		w = 58
	}
	if w < 24 {
		w = 24
	}
	rows := p.filtered()
	p.clampCursor(len(rows))

	var b strings.Builder
	b.WriteString(titleStyle.Render(p.title))
	b.WriteString("\n")
	b.WriteString(dimStyle.Render("filter: ") + p.filter + cursorStyle.Render(" "))
	b.WriteString("\n\n")

	// A modest window so a long repo list stays inside the box.
	const maxRows = 12
	start := 0
	if p.cursor >= maxRows {
		start = p.cursor - maxRows + 1
	}
	end := start + maxRows
	if end > len(rows) {
		end = len(rows)
	}
	for i := start; i < end; i++ {
		it := rows[i]
		marker := "  "
		if i == p.cursor {
			marker = keyStyle.Render("▸ ")
		}
		b.WriteString(marker + p.rowLabel(it) + "\n")
	}

	b.WriteString("\n")
	b.WriteString(dimStyle.Render("type filter · "))
	b.WriteString(keyStyle.Render("Space") + dimStyle.Render(" select · "))
	b.WriteString(keyStyle.Render("↵") + dimStyle.Render(" done · "))
	b.WriteString(keyStyle.Render("Esc") + dimStyle.Render(" cancel"))

	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(accent).
		Padding(1, 2).Width(w).
		Render(b.String())
	return lipgloss.Place(m.mainWidth(), m.paneRows(), lipgloss.Center, lipgloss.Center, box)
}

// rowLabel renders one picker row: the track-new sentinel as an accented action,
// otherwise a checkbox (filled when selected) plus the repo name and a dim source.
func (p *pickerState) rowLabel(it repoItem) string {
	if it.name == trackNewID {
		return keyStyle.Render(it.source)
	}
	box := dimStyle.Render("○ ")
	if p.selected[it.name] {
		box = keyStyle.Render("● ")
	}
	label := it.name
	if it.source != "" {
		label += "  " + dimStyle.Render(it.source)
	}
	return box + label
}
