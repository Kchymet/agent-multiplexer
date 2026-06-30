package nativetui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"amux/internal/core"
	"amux/internal/store"
)

var cursorStyle = lipgloss.NewStyle().Reverse(true)

// openNewRepoAgentForm opens the settings form for a new repo-scoped agent.
func (m *model) openNewRepoAgentForm(repoID, repoTitle string) {
	m.form = &formState{
		title:  "New agent · " + repoTitle,
		action: "new-repo-agent",
		id:     repoID,
		submit: "Create agent",
		fields: []*formField{
			{key: "prompt", label: "Prompt"},
			{key: "mode", label: "Mode", value: store.ModeTask, options: []string{store.ModeTask, store.ModeLoop}},
			{key: "model", label: "Model", value: store.ModelOpus, options: store.Models},
		},
	}
}

// openAddAgentForm opens the settings form for an additional agent on an
// existing workgroup (root id). The repos field is left blank by default, which
// the daemon expands to the whole workgroup's repos; fill it to scope the agent
// to a subset.
func (m *model) openAddAgentForm(rootID, rootTitle string) {
	m.form = &formState{
		title:  "Add agent · " + rootTitle,
		action: "add-agent",
		id:     rootID,
		submit: "Add agent",
		fields: []*formField{
			{key: "prompt", label: "Prompt"},
			{key: "repos", label: "Repos (blank = all)"},
			{key: "mode", label: "Mode", value: store.ModeTask, options: []string{store.ModeTask, store.ModeLoop}},
			{key: "model", label: "Model", value: store.ModelOpus, options: store.Models},
		},
	}
}

// openAddRepoForm opens a one-field form to track a new repository from a
// GitHub owner/name, a git URL, or a local path.
func (m *model) openAddRepoForm() {
	m.form = &formState{
		title:  "Add repo",
		action: "add-repo",
		submit: "Track repo",
		fields: []*formField{
			{key: "source", label: "URL / owner/name / path"},
		},
	}
}

// openNewWorkgroupForm opens the settings form for a new work-scoped workgroup.
func (m *model) openNewWorkgroupForm() {
	m.form = &formState{
		title:  "New workgroup",
		action: "new-workgroup",
		submit: "Create workgroup",
		fields: []*formField{
			{key: "name", label: "Name"},
			{key: "repos", label: "Repos (comma)"},
			{key: "prompt", label: "Description"},
			{key: "linear", label: "Linear issue/URL"},
		},
	}
}

// openRenameForm opens a one-field form to set a session's display name. The
// name is purely cosmetic — the session keeps its id — so the field starts empty
// (a blank submit clears any existing name, reverting the rail to the id).
func (m *model) openRenameForm(id, title string) {
	m.form = &formState{
		title:  "Rename · " + title,
		action: "rename",
		id:     id,
		submit: "Rename",
		fields: []*formField{
			{key: "name", label: "Display name"},
		},
	}
}

// formField is one editable field: a free-text field with a vim cursor, or (when
// options is non-empty) a cycle-through-options select.
type formField struct {
	key     string
	label   string
	value   string
	options []string
	cursor  int // rune index of the vim cursor (text fields)
}

func (f *formField) isSelect() bool { return len(f.options) > 0 }

func (f *formField) cycle(forward bool) {
	idx := 0
	for i, o := range f.options {
		if o == f.value {
			idx = i
			break
		}
	}
	n := len(f.options)
	if forward {
		idx = (idx + 1) % n
	} else {
		idx = (idx - 1 + n) % n
	}
	f.value = f.options[idx]
}

func (f *formField) display() string {
	if f.isSelect() {
		return "‹ " + f.value + " ›"
	}
	if f.value == "" {
		return dimStyle.Render("(empty)")
	}
	return f.value
}

// renderActive renders a text field with a block cursor at f.cursor.
func (f *formField) renderActive() string {
	r := []rune(f.value)
	c := f.cursor
	if c < 0 {
		c = 0
	}
	if c > len(r) {
		c = len(r)
	}
	on := " "
	after := ""
	if c < len(r) {
		on = string(r[c])
		after = string(r[c+1:])
	}
	return string(r[:c]) + cursorStyle.Render(on) + after
}

// ---- single-line vim editor primitives (operate on the rune slice) ----

func (f *formField) end() int { return len([]rune(f.value)) }

func (f *formField) clampNormal() {
	n := f.end()
	if n == 0 {
		f.cursor = 0
		return
	}
	if f.cursor > n-1 {
		f.cursor = n - 1
	}
	if f.cursor < 0 {
		f.cursor = 0
	}
}

func (f *formField) clampInsert() {
	if f.cursor > f.end() {
		f.cursor = f.end()
	}
	if f.cursor < 0 {
		f.cursor = 0
	}
}

func (f *formField) left() {
	if f.cursor > 0 {
		f.cursor--
	}
}

func (f *formField) insertRunes(rs []rune) {
	r := []rune(f.value)
	c := f.cursor
	if c > len(r) {
		c = len(r)
	}
	out := append([]rune{}, r[:c]...)
	out = append(out, rs...)
	out = append(out, r[c:]...)
	f.value = string(out)
	f.cursor = c + len(rs)
}

func (f *formField) backspace() {
	r := []rune(f.value)
	if f.cursor > 0 && f.cursor <= len(r) {
		f.value = string(append(r[:f.cursor-1], r[f.cursor:]...))
		f.cursor--
	}
}

func (f *formField) deleteAt() { // x
	r := []rune(f.value)
	if f.cursor < len(r) {
		f.value = string(append(r[:f.cursor], r[f.cursor+1:]...))
	}
	f.clampNormal()
}

func (f *formField) deleteToEnd() { // D / d$
	r := []rune(f.value)
	if f.cursor < len(r) {
		f.value = string(r[:f.cursor])
	}
	f.clampNormal()
}

func (f *formField) deleteToStart() { // d0
	r := []rune(f.value)
	if f.cursor <= len(r) {
		f.value = string(r[f.cursor:])
	}
	f.cursor = 0
}

func (f *formField) deleteWord() { // dw
	r := []rune(f.value)
	e := f.cursor
	for e < len(r) && !isWordSpace(r[e]) {
		e++
	}
	for e < len(r) && isWordSpace(r[e]) {
		e++
	}
	f.value = string(append(r[:f.cursor], r[e:]...))
	f.clampNormal()
}

func (f *formField) replaceAt(ch rune) { // r<char>
	r := []rune(f.value)
	if f.cursor < len(r) {
		r[f.cursor] = ch
		f.value = string(r)
	}
}

func (f *formField) wordForward() { // w
	r := []rune(f.value)
	c := f.cursor
	for c < len(r) && !isWordSpace(r[c]) {
		c++
	}
	for c < len(r) && isWordSpace(r[c]) {
		c++
	}
	f.cursor = c
	f.clampNormal()
}

func (f *formField) wordBack() { // b
	r := []rune(f.value)
	c := f.cursor - 1
	for c > 0 && isWordSpace(r[c]) {
		c--
	}
	for c > 0 && !isWordSpace(r[c-1]) {
		c--
	}
	if c < 0 {
		c = 0
	}
	f.cursor = c
}

func (f *formField) wordEnd() { // e
	r := []rune(f.value)
	c := f.cursor + 1
	for c < len(r) && isWordSpace(r[c]) {
		c++
	}
	for c+1 < len(r) && !isWordSpace(r[c+1]) {
		c++
	}
	if c >= len(r) {
		c = len(r) - 1
	}
	if c < 0 {
		c = 0
	}
	f.cursor = c
}

func isWordSpace(r rune) bool { return r == ' ' || r == '\t' }

// formState is a pending modal form: a column of fields plus a submit button
// (cursor == len(fields)). Text fields are vim-edited; `insert` is the vim mode
// and `pending` holds a half-typed operator (d/c/r).
type formState struct {
	title   string
	action  string
	id      string
	submit  string
	fields  []*formField
	cursor  int
	insert  bool
	pending string
}

func (fs *formState) values() map[string]string {
	v := map[string]string{}
	for _, f := range fs.fields {
		v[f.key] = strings.TrimSpace(f.value)
	}
	return v
}

func (fs *formState) onSubmit() bool { return fs.cursor == len(fs.fields) }

func (fs *formState) active() *formField {
	if fs.cursor < 0 || fs.cursor >= len(fs.fields) {
		return nil
	}
	return fs.fields[fs.cursor]
}

func (fs *formState) next() { fs.move(1) }
func (fs *formState) prev() { fs.move(-1) }
func (fs *formState) move(d int) {
	n := len(fs.fields) + 1
	fs.cursor = (fs.cursor + d + n) % n
	fs.pending = ""
	if f := fs.active(); f != nil && !f.isSelect() {
		if fs.insert {
			f.cursor = f.end() // land ready to append
		} else {
			f.clampNormal() // normal mode: keep the cursor on a real rune
		}
	}
}

// handleForm processes a keystroke while a form modal is open, with vim editing
// on text fields.
func (m *model) handleForm(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	fs := m.form
	field := fs.active()
	text := field != nil && !field.isSelect()

	// A half-typed operator (d/c/r) consumes the next key.
	if text && fs.pending != "" {
		m.applyOperator(field, k)
		return m, nil
	}

	if text && fs.insert {
		switch k.Type {
		case tea.KeyEsc:
			fs.insert = false
			field.left() // vim drops the cursor leaving insert
			return m, nil
		case tea.KeyEnter:
			return m.formEnter()
		case tea.KeyTab:
			fs.next()
		case tea.KeyShiftTab:
			fs.prev()
		case tea.KeyCtrlC:
			m.cancelForm()
		case tea.KeyRunes:
			field.insertRunes(k.Runes)
		case tea.KeySpace:
			field.insertRunes([]rune{' '})
		case tea.KeyBackspace:
			field.backspace()
		case tea.KeyLeft:
			field.left()
		case tea.KeyRight:
			field.cursor++
			field.clampInsert()
		}
		return m, nil
	}

	if text { // normal mode
		switch k.String() {
		case "esc", "ctrl+c":
			m.cancelForm()
		case "tab":
			fs.next()
		case "shift+tab":
			fs.prev()
		case "enter":
			return m.formEnter()
		case "i":
			fs.insert = true
		case "a":
			field.cursor++
			field.clampInsert()
			fs.insert = true
		case "I":
			field.cursor = 0
			fs.insert = true
		case "A":
			field.cursor = field.end()
			fs.insert = true
		case "s":
			field.deleteAt()
			fs.insert = true
		case "h", "left":
			field.left()
		case "l", "right":
			field.cursor++
			field.clampNormal()
		case "0":
			field.cursor = 0
		case "$":
			field.cursor = field.end()
			field.clampNormal()
		case "w":
			field.wordForward()
		case "b":
			field.wordBack()
		case "e":
			field.wordEnd()
		case "x":
			field.deleteAt()
		case "X":
			field.backspace()
			field.clampNormal()
		case "D":
			field.deleteToEnd()
		case "C":
			field.deleteToEnd()
			fs.insert = true
		case "d", "c", "r":
			fs.pending = k.String()
		case "j", "down":
			fs.next()
		case "k", "up":
			fs.prev()
		}
		return m, nil
	}

	// Select field or the submit button. Vim motions navigate between fields
	// (j/k) and cycle a select's options (h/l), alongside the arrow keys.
	switch k.String() {
	case "esc", "ctrl+c":
		m.cancelForm()
	case "tab", "down", "j":
		fs.next()
	case "shift+tab", "up", "k":
		fs.prev()
	case "enter":
		return m.formEnter()
	case "left", "h":
		if field != nil {
			field.cycle(false)
		}
	case "right", "l", " ":
		if field != nil {
			field.cycle(true)
		}
	}
	return m, nil
}

// applyOperator finishes a pending vim operator with its argument key.
func (m *model) applyOperator(field *formField, k tea.KeyMsg) {
	fs := m.form
	op := fs.pending
	fs.pending = ""
	switch op {
	case "r":
		if k.Type == tea.KeyRunes && len(k.Runes) == 1 {
			field.replaceAt(k.Runes[0])
		} else if k.Type == tea.KeySpace {
			field.replaceAt(' ')
		}
	case "d":
		switch k.String() {
		case "d":
			field.value, field.cursor = "", 0
		case "$":
			field.deleteToEnd()
		case "0":
			field.deleteToStart()
		case "w":
			field.deleteWord()
		}
	case "c":
		switch k.String() {
		case "c":
			field.value, field.cursor = "", 0
			fs.insert = true
		case "$":
			field.deleteToEnd()
			fs.insert = true
		case "w":
			field.deleteWord()
			fs.insert = true
		}
	}
}

func (m *model) formEnter() (tea.Model, tea.Cmd) {
	if m.form.onSubmit() {
		return m, m.submitForm()
	}
	m.form.next()
	return m, nil
}

func (m *model) cancelForm() {
	m.form = nil
	m.status = "cancelled"
}

// submitForm dispatches the form's action with its field values.
func (m *model) submitForm() tea.Cmd {
	fs := m.form
	m.form = nil
	m.status = fs.submit + "…"
	return m.sendCmd(core.Action{Action: fs.action, ID: fs.id, Fields: fs.values()})
}

// renderForm draws the form as a centered modal in the main pane.
func (m *model) renderForm() string {
	fs := m.form
	w := m.mainWidth() - 8
	if w > 58 {
		w = 58
	}
	if w < 18 {
		w = 18
	}
	var b strings.Builder
	b.WriteString(titleStyle.Render(fs.title))
	b.WriteString("\n\n")
	for i, f := range fs.fields {
		active := i == fs.cursor
		marker := "  "
		label := dimStyle.Render(f.label + ": ")
		val := f.display()
		if active {
			marker = keyStyle.Render("▸ ")
			label = f.label + ": "
			if !f.isSelect() {
				val = f.renderActive()
			}
		}
		b.WriteString(marker + label + val + "\n")
	}
	b.WriteString("\n")
	submit := " " + fs.submit + " "
	if fs.onSubmit() {
		b.WriteString(selStyle.Render(submit))
	} else {
		b.WriteString(keyStyle.Render(submit))
	}
	b.WriteString("  " + m.formHint())
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(accent).
		Padding(1, 2).Width(w).
		Render(b.String())
	return lipgloss.Place(m.mainWidth(), m.paneRows(), lipgloss.Center, lipgloss.Center, box)
}

// formHint is the footer line: the vim mode on a text field, else generic help.
func (m *model) formHint() string {
	fs := m.form
	if f := fs.active(); f != nil && !f.isSelect() {
		if fs.insert {
			return titleStyle.Render("-- INSERT --") + dimStyle.Render("  Esc normal · Enter next")
		}
		return titleStyle.Render("-- NORMAL --") + dimStyle.Render("  i insert · j/k move · Esc cancel")
	}
	return dimStyle.Render("j/k move · h/l change · Enter submit · Esc cancel")
}
