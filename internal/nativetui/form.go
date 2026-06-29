package nativetui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"amux/internal/core"
	"amux/internal/store"
)

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
			{key: "model", label: "Model"},
		},
	}
}

// openNewWorkgroupForm opens the settings form for a new work-scoped workgroup.
// It can seed a baseline prompt/description and a Linear issue to work on.
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

// formField is one editable field in a modal form: a free-text field, or (when
// options is non-empty) a cycle-through-options select.
type formField struct {
	key     string   // identifier carried in the dispatched action's Fields
	label   string   // shown label
	value   string   // current value (for selects, the chosen option)
	options []string // non-empty => a select field
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

// formState is a pending modal form: a column of fields plus a submit button
// (cursor == len(fields)). Submitting dispatches `action` with the field values.
type formState struct {
	title  string
	action string // daemon action to dispatch
	id     string // action ID (e.g. the repo name)
	submit string // submit button label / status verb
	fields []*formField
	cursor int
}

func (fs *formState) values() map[string]string {
	v := map[string]string{}
	for _, f := range fs.fields {
		v[f.key] = strings.TrimSpace(f.value)
	}
	return v
}

func (fs *formState) onSubmit() bool { return fs.cursor == len(fs.fields) }

// handleForm processes a keystroke while a form modal is open.
func (m *model) handleForm(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	fs := m.form
	switch k.String() {
	case "esc", "ctrl+c":
		m.form = nil
		m.status = "cancelled"
		return m, nil
	case "tab", "down":
		fs.cursor = (fs.cursor + 1) % (len(fs.fields) + 1)
		return m, nil
	case "shift+tab", "up":
		fs.cursor = (fs.cursor - 1 + len(fs.fields) + 1) % (len(fs.fields) + 1)
		return m, nil
	case "enter":
		if fs.onSubmit() {
			return m, m.submitForm()
		}
		fs.cursor++ // advance toward the submit button
		return m, nil
	}
	if fs.onSubmit() {
		return m, nil
	}
	f := fs.fields[fs.cursor]
	if f.isSelect() {
		switch k.String() {
		case "left":
			f.cycle(false)
		case "right", " ":
			f.cycle(true)
		}
		return m, nil
	}
	switch k.Type {
	case tea.KeyBackspace:
		if r := []rune(f.value); len(r) > 0 {
			f.value = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		f.value += " "
	case tea.KeyRunes:
		f.value += string(k.Runes)
	}
	return m, nil
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
		line := f.label + ": " + f.display()
		if i == fs.cursor {
			b.WriteString(selStyle.Render(" " + line + " "))
		} else {
			b.WriteString(" " + dimStyle.Render(f.label+": ") + f.display())
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	submit := " " + fs.submit + " "
	if fs.onSubmit() {
		b.WriteString(selStyle.Render(submit))
	} else {
		b.WriteString(keyStyle.Render(submit))
	}
	b.WriteString("   " + dimStyle.Render("Tab next · ←/→ change · Enter submit · Esc cancel"))
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(accent).
		Padding(1, 2).Width(w).
		Render(b.String())
	return lipgloss.Place(m.mainWidth(), m.paneRows(), lipgloss.Center, lipgloss.Center, box)
}
