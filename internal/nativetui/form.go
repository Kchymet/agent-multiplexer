package nativetui

import (
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/store"
)

var cursorStyle = lipgloss.NewStyle().Reverse(true)

// openNewRepoAgentForm opens the settings form for a new repo-scoped agent.
func (m *model) openNewRepoAgentForm(repoID, repoTitle string) {
	m.form = &formState{
		title:  "New agent · " + repoTitle,
		action: core.ActionNewRepoAgent,
		id:     repoID,
		submit: "Create agent",
		fields: []*formField{
			{key: "prompt", label: "Prompt"},
			{key: "mode", label: "Mode", value: store.ModeTask, options: []string{store.ModeTask, store.ModeInteractive}},
			harnessField(),
			modelField(agent.DefaultKind()),
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
		action: core.ActionAddAgent,
		id:     rootID,
		submit: "Add agent",
		fields: []*formField{
			{key: "prompt", label: "Prompt"},
			{key: "repos", label: "Repos", picker: true},
			{key: "mode", label: "Mode", value: store.ModeTask, options: []string{store.ModeTask, store.ModeInteractive}},
			harnessField(),
			modelField(agent.DefaultKind()),
		},
	}
}

// openAddRepoForm opens a one-field form to track a new repository from a
// GitHub owner/name, a git URL, or a local path.
func (m *model) openAddRepoForm() {
	m.form = &formState{
		title:  "Add repo",
		action: core.ActionAddRepo,
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
		action: core.ActionNewWorkgroup,
		submit: "Create workgroup",
		fields: []*formField{
			{key: "name", label: "Name"},
			{key: "prompt", label: "Prompt"},
			{key: "repos", label: "Repos (first agent)", picker: true},
			{key: "mode", label: "Mode", value: store.ModeTask, options: []string{store.ModeTask, store.ModeInteractive}},
			harnessField(),
			modelField(agent.DefaultKind()),
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
		action: core.ActionRename,
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
	picker  bool // repos-style field: activating it opens the fuzzy repo picker
	cursor  int  // rune index of the vim cursor (text fields)
	scroll  int  // first visible row of a multi-row text field (see renderRows)
}

func (f *formField) isSelect() bool { return len(f.options) > 0 }
func (f *formField) isPicker() bool { return f.picker }

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

// display renders an inactive field in at most width cells on one row. A text
// value shows its first line, cut with an ellipsis if long, and a count of the
// lines it hides: a pasted multi-line prompt stays one row until selected.
func (f *formField) display(width int) string {
	if f.isSelect() {
		return "‹ " + f.value + " ›"
	}
	if f.isPicker() {
		if f.value == "" {
			return dimStyle.Render("(none — ↵ to pick)")
		}
		return strings.ReplaceAll(f.value, ",", ", ")
	}
	if f.value == "" {
		return dimStyle.Render("(empty)")
	}
	first, rest, multi := strings.Cut(f.value, "\n")
	if !multi {
		return truncateCells(first, width)
	}
	more := dimStyle.Render(" (+" + strconv.Itoa(strings.Count(rest, "\n")+1) + " lines)")
	return truncateCells(first, width-lipgloss.Width(more)) + more
}

// lineCount and cursorLine are the field's logical (newline-separated) line
// count and the 1-based line the cursor is on, for the active field's label.
func (f *formField) lineCount() int { return strings.Count(f.value, "\n") + 1 }
func (f *formField) cursorLine() int {
	r := []rune(f.value)
	c := f.cursor
	if c > len(r) {
		c = len(r)
	}
	return strings.Count(string(r[:c]), "\n") + 1
}

// renderRows draws the active text field: its value wrapped into rows of at
// most width cells, with a block cursor at f.cursor, showing at most maxRows of
// them. The window scrolls only as far as needed to keep the cursor's row in
// view (f.scroll remembers it between frames), so moving through a long pasted
// prompt pans it inside the box rather than growing the box.
func (f *formField) renderRows(width, maxRows int) []string {
	if maxRows < 1 {
		maxRows = 1
	}
	r := []rune(f.value)
	c := f.cursor
	if c < 0 {
		c = 0
	}
	if c > len(r) {
		c = len(r)
	}
	// Wrap one cell short so a cursor sitting past a full row's last rune still
	// fits on that row instead of spilling into a new one.
	spans := wrapSpans(r, width-1)
	row := cursorRow(spans, r, c)
	if row < f.scroll {
		f.scroll = row
	}
	if row >= f.scroll+maxRows {
		f.scroll = row - maxRows + 1
	}
	if f.scroll > len(spans)-maxRows {
		f.scroll = len(spans) - maxRows
	}
	if f.scroll < 0 {
		f.scroll = 0
	}
	end := f.scroll + maxRows
	if end > len(spans) {
		end = len(spans)
	}
	out := make([]string, 0, end-f.scroll)
	for i := f.scroll; i < end; i++ {
		sp := spans[i]
		if i != row {
			out = append(out, expandTabs(r[sp.start:sp.end]))
			continue
		}
		on := " "
		after := ""
		if c < sp.end {
			on = expandTabs(r[c : c+1])
			after = expandTabs(r[c+1 : sp.end])
		}
		out = append(out, expandTabs(r[sp.start:c])+cursorStyle.Render(on)+after)
	}
	return out
}

// ---- vim editor primitives (operate on the rune slice) ----

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

func isWordSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }

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

// field returns the field with the given key, or nil when the form has none.
func (fs *formState) field(key string) *formField {
	for _, f := range fs.fields {
		if f.key == key {
			return f
		}
	}
	return nil
}

// syncDependents refreshes fields whose choices depend on another select's
// value, after that select changes. Today the only such pair is Harness → Model:
// the chosen harness filters the model list to its own models, and a selection
// that the new list no longer offers resets to the harness default.
func (fs *formState) syncDependents(changed *formField) {
	if changed == nil || changed.key != "agent" {
		return
	}
	model := fs.field("model")
	if model == nil {
		return
	}
	h := agent.HarnessFor(changed.value)
	model.options = h.Models()
	if !slices.Contains(model.options, model.value) {
		model.value = h.DefaultModel()
	}
}

// harnessField builds a form's Harness selector from the agent registry,
// defaulting to the first-registered harness.
func harnessField() *formField {
	return &formField{key: "agent", label: "Harness", value: agent.DefaultKind(), options: agent.Kinds()}
}

// modelField builds a form's Model selector for a harness kind — its offered
// models, defaulting to that harness's built-in default.
func modelField(kind string) *formField {
	h := agent.HarnessFor(kind)
	return &formField{key: "model", label: "Model", value: h.DefaultModel(), options: h.Models()}
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

	// A picker field (repos) opens the fuzzy repo picker on activation; otherwise
	// j/k/tab move between fields, like a select.
	if field != nil && field.isPicker() {
		switch k.String() {
		case "esc", "ctrl+c":
			m.cancelForm()
		case "enter", " ", "i", "a":
			p := newRepoPicker("Repos", m.sessions, store.SplitRepos(field.value))
			p.field = field
			m.openRepoPicker(p)
		case "tab", "down", "j":
			fs.next()
		case "shift+tab", "up", "k":
			fs.prev()
		}
		return m, nil
	}

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
			field.insertRunes(inputRunes(k))
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
		if k.Paste { // a paste is text to keep, never a run of commands
			field.insertRunes(inputRunes(k))
			field.clampNormal()
			return m, nil
		}
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
			fs.syncDependents(field)
		}
	case "right", "l", " ":
		if field != nil {
			field.cycle(true)
			fs.syncDependents(field)
		}
	}
	return m, nil
}

// inputRunes is the text a key press adds to a field. Typed runes pass through;
// a bracketed paste arrives whole, newlines included, and is kept that way (a
// multi-line prompt stays multi-line) with the terminal's CR / CRLF line ends
// folded to LF so the value has one kind of newline.
func inputRunes(k tea.KeyMsg) []rune {
	if !k.Paste {
		return k.Runes
	}
	out := make([]rune, 0, len(k.Runes))
	for i, r := range k.Runes {
		switch {
		case r == '\r' && i+1 < len(k.Runes) && k.Runes[i+1] == '\n':
			continue // CRLF: the LF that follows carries the newline
		case r == '\r':
			out = append(out, '\n')
		default:
			out = append(out, r)
		}
	}
	return out
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
	m.pendingPicker = nil // drop any picker parked behind a track-new form
	m.status = "cancelled"
}

// submitForm dispatches the form's action with its field values. When the form
// is the add-repo form spawned from the picker's "track new repo" row, it reopens
// that parked picker so the freshly-tracked repo can be selected (it lands in a
// later snapshot; the picker's list refreshes live).
func (m *model) submitForm() tea.Cmd {
	fs := m.form
	m.form = nil
	m.status = fs.submit + "…"
	cmd := m.sendCmd(core.Action{Action: fs.action, ID: fs.id, Fields: fs.values()})
	if fs.action == core.ActionAddRepo && m.pendingPicker != nil {
		m.picker = m.pendingPicker
		m.pendingPicker = nil
	}
	return cmd
}

// minValueCells is the narrowest a text field's rows may get beside their label
// before they drop under it instead.
const minValueCells = 12

// formChrome is the rows around the fields inside the modal: border (2),
// padding (2), title + blank (2), blank + footer row (2).
const formChrome = 8

// renderForm draws the form as a centered modal in the main pane. The modal is
// sized to the pane: each field takes one row except the active text field,
// which gets the rows left over (up to maxFieldRows) and scrolls within them,
// so a long or pasted multi-line prompt never grows the box past the pane.
func (m *model) renderForm() string {
	fs := m.form
	w := m.mainWidth() - 8
	if w > 58 {
		w = 58
	}
	if w < 18 {
		w = 18
	}
	inner := w - 4 // Width includes the padding
	// The footer is the submit button beside the key hint, or the two stacked
	// when they do not fit on one row (the hint is worth a field row).
	submit := " " + fs.submit + " "
	if fs.onSubmit() {
		submit = selStyle.Render(submit)
	} else {
		submit = keyStyle.Render(submit)
	}
	footer := []string{submit + "  " + m.formHint()}
	if lipgloss.Width(footer[0]) > inner {
		footer = []string{submit, m.formHint()}
	}
	rows := m.paneRows() - formChrome - (len(footer) - 1) - (len(fs.fields) - 1)
	if rows > maxFieldRows {
		rows = maxFieldRows
	}
	if rows < 1 {
		rows = 1
	}
	// Every row is budgeted as exactly one row: anything wider than the box is
	// cut rather than wrapped, which would push the box past the pane.
	var b strings.Builder
	row := func(s string) { b.WriteString(ansi.Truncate(s, inner, "…") + "\n") }
	row(titleStyle.Render(fs.title))
	row("")
	for i, f := range fs.fields {
		active := i == fs.cursor
		marker := "  "
		label := dimStyle.Render(f.label + ": ")
		if !active {
			row(marker + label + f.display(inner-lipgloss.Width(marker+label)))
			continue
		}
		marker = keyStyle.Render("▸ ")
		label = f.label + ": "
		if f.isSelect() || f.isPicker() {
			row(marker + label + f.display(inner-lipgloss.Width(marker+label)))
			continue
		}
		if f.lineCount() > 1 {
			label = f.label + dimStyle.Render(" [line "+strconv.Itoa(f.cursorLine())+"/"+strconv.Itoa(f.lineCount())+"]") + ": "
		}
		// Rows hang under the value, after the label; when the label leaves too
		// little room (narrow pane, long label) they start on the next row.
		prefix := marker + label
		indent := strings.Repeat(" ", lipgloss.Width(prefix))
		if inner-len(indent) < minValueCells {
			row(prefix)
			prefix, indent = "    ", "    "
			rows--
		}
		for j, r := range f.renderRows(inner-len(indent), rows) {
			if j == 0 {
				row(prefix + r)
			} else {
				row(indent + r)
			}
		}
	}
	row("")
	for _, f := range footer {
		row(f)
	}
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(accent).
		Padding(1, 2).Width(w).
		Render(strings.TrimSuffix(b.String(), "\n"))
	box = clampBlock(box, m.mainWidth(), m.paneRows())
	return lipgloss.Place(m.mainWidth(), m.paneRows(), lipgloss.Center, lipgloss.Center, box)
}

// formHint is the footer line: the vim mode on a text field, else generic help.
func (m *model) formHint() string {
	fs := m.form
	if f := fs.active(); f != nil && f.isPicker() {
		return dimStyle.Render("↵ pick repos · j/k move · Esc cancel")
	}
	if f := fs.active(); f != nil && !f.isSelect() {
		if fs.insert {
			return titleStyle.Render("-- INSERT --") + dimStyle.Render("  Esc normal · Enter next")
		}
		return titleStyle.Render("-- NORMAL --") + dimStyle.Render("  i insert · j/k move · Esc cancel")
	}
	return dimStyle.Render("j/k move · h/l change · Enter submit · Esc cancel")
}
