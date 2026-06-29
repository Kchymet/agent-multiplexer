package nativetui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
// (Prompt) text field's value.
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
		{"insert + backspace", []string{"abc", "<bs>"}, "ab"},
		{"append with A", []string{"ab", "<esc>", "A", "cd"}, "abcd"},
		{"replace with r", []string{"cat", "<esc>", "0", "r", "b"}, "bat"},
		{"delete char x", []string{"abc", "<esc>", "0", "x"}, "bc"},
		{"delete line dd", []string{"abc", "<esc>", "d", "d"}, ""},
		{"word + D", []string{"foo bar", "<esc>", "0", "w", "D"}, "foo "},
		{"insert at start I", []string{"bar", "<esc>", "I", "X"}, "Xbar"},
		{"change word cw", []string{"foo bar", "<esc>", "0", "c", "w", "baz "}, "baz bar"},
		{"space inserts", []string{"a", "<sp>", "b"}, "a b"},
	}
	for _, tc := range cases {
		if got := drive(tc.keys...); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Esc leaves insert mode; subsequent letters are commands, not text.
func TestVimNormalModeNotTyped(t *testing.T) {
	m := &model{}
	m.openNewRepoAgentForm("repo", "Repo")
	for _, k := range []string{"hi", "<esc>", "x", "x", "x"} { // x deletes, doesn't type
		m.handleForm(key(k))
	}
	if got := m.form.fields[0].value; got != "" {
		t.Fatalf("normal-mode x should delete, got %q", got)
	}
	if m.form.insert {
		t.Fatal("should be in normal mode after Esc")
	}
}
