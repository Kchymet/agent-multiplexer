package nativetui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"amux/internal/store"
)

func key(s string) tea.KeyMsg {
	switch s {
	case "<esc>":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "<bs>":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "<sp>":
		return tea.KeyMsg{Type: tea.KeySpace}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// drive opens a fresh repo-agent form and applies the keys, returning the first
// (Prompt) text field's value. The form opens in normal mode, so editing tests
// press "i" first to start typing.
func drive(keys ...string) string {
	m := &model{}
	m.openNewRepoAgentForm("repo", "Repo")
	for _, k := range keys {
		m.handleForm(key(k))
	}
	return m.form.fields[0].value
}

func TestVimFormEditing(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		want string
	}{
		{"insert + backspace", []string{"i", "abc", "<bs>"}, "ab"},
		{"append with A", []string{"i", "ab", "<esc>", "A", "cd"}, "abcd"},
		{"replace with r", []string{"i", "cat", "<esc>", "0", "r", "b"}, "bat"},
		{"delete char x", []string{"i", "abc", "<esc>", "0", "x"}, "bc"},
		{"delete line dd", []string{"i", "abc", "<esc>", "d", "d"}, ""},
		{"word + D", []string{"i", "foo bar", "<esc>", "0", "w", "D"}, "foo "},
		{"insert at start I", []string{"i", "bar", "<esc>", "I", "X"}, "Xbar"},
		{"change word cw", []string{"i", "foo bar", "<esc>", "0", "c", "w", "baz "}, "baz bar"},
		{"space inserts", []string{"i", "a", "<sp>", "b"}, "a b"},
	}
	for _, tc := range cases {
		if got := drive(tc.keys...); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A freshly opened form starts in normal mode: the first field is selected for
// navigation, not entry. The user opts into typing with i/a/A/I.
func TestFormOpensInNormalMode(t *testing.T) {
	m := &model{}
	m.openNewRepoAgentForm("repo", "Repo")
	if m.form.insert {
		t.Fatal("form should open in normal mode, not insert")
	}
	// A bare letter is a motion/command, not text.
	m.handleForm(key("x"))
	if got := m.form.fields[0].value; got != "" {
		t.Fatalf("normal-mode key should not type, got %q", got)
	}
}

// The new-agent form offers a model selector defaulting to opus, with vim
// motions (j to reach it, l to cycle) driving the selection.
func TestModelSelectorDefaultsOpusAndCycles(t *testing.T) {
	m := &model{}
	m.openNewRepoAgentForm("repo", "Repo")

	var modelField *formField
	for _, f := range m.form.fields {
		if f.key == "model" {
			modelField = f
		}
	}
	if modelField == nil {
		t.Fatal("no model field")
	}
	if !modelField.isSelect() {
		t.Fatal("model field should be a selector")
	}
	if got := modelField.value; got != store.ModelOpus {
		t.Fatalf("default model: got %q, want %q", got, store.ModelOpus)
	}

	// Navigate from the prompt field down to the model selector with j, then
	// cycle forward with l. opus -> sonnet.
	m.handleForm(key("j")) // prompt -> mode
	m.handleForm(key("j")) // mode -> model
	if m.form.active() != modelField {
		t.Fatal("j navigation did not land on the model field")
	}
	m.handleForm(key("l"))
	if got := modelField.value; got != store.ModelSonnet {
		t.Fatalf("after cycling: got %q, want %q", got, store.ModelSonnet)
	}
}

// The rename form carries a single name field and dispatches the "rename"
// action (with the typed name) for the targeted session, leaving its id alone.
func TestRenameFormSubmit(t *testing.T) {
	m := &model{}
	m.openRenameForm("a1b2c3", "old-label")
	if got := m.form.action; got != "rename" {
		t.Fatalf("action: got %q, want rename", got)
	}
	if got := m.form.id; got != "a1b2c3" {
		t.Fatalf("id: got %q, want a1b2c3", got)
	}
	for _, k := range []string{"i", "new", "<sp>", "name"} {
		m.handleForm(key(k))
	}
	if got := m.form.fields[0].value; got != "new name" {
		t.Fatalf("name field: got %q, want %q", got, "new name")
	}
	if got := m.form.values()["name"]; got != "new name" {
		t.Fatalf("submitted name: got %q, want %q", got, "new name")
	}
}

// Esc leaves insert mode; subsequent letters are commands, not text.
func TestVimNormalModeNotTyped(t *testing.T) {
	m := &model{}
	m.openNewRepoAgentForm("repo", "Repo")
	for _, k := range []string{"i", "hi", "<esc>", "x", "x", "x"} { // x deletes, doesn't type
		m.handleForm(key(k))
	}
	if got := m.form.fields[0].value; got != "" {
		t.Fatalf("normal-mode x should delete, got %q", got)
	}
	if m.form.insert {
		t.Fatal("should be in normal mode after Esc")
	}
}
