package nativetui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// paste is a bracketed-paste key event: the whole text in one message, newlines
// included, as bubbletea delivers it.
func paste(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s), Paste: true}
}

func longPrompt(n int) string {
	var lines []string
	for i := 0; i < n; i++ {
		lines = append(lines, fmt.Sprintf("line %02d of the pasted prompt, long enough to need wrapping inside the box", i))
	}
	return strings.Join(lines, "\n")
}

// plain strips ANSI styling so tests can look at the picture.
func plain(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if r == 'm' {
				inEsc = false
			}
		case r == '\x1b':
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Pasting a many-line prompt into the add-agent form must not grow the modal
// past the main pane: the box stays inside paneRows × mainWidth, complete with
// its bottom border, while the full pasted text is still what the form submits.
func TestPastedMultilinePromptStaysInBox(t *testing.T) {
	m := &model{w: 100, h: 24}
	m.openAddAgentForm("wg1", "payments")
	m.handleForm(key("i"))
	text := longPrompt(40)
	m.handleForm(paste(text))

	if got := m.form.values()["prompt"]; got != text {
		t.Fatalf("pasted prompt altered:\n got %q\nwant %q", got, text)
	}
	out := m.renderForm()
	if h := lipgloss.Height(out); h > m.paneRows() {
		t.Errorf("form is %d rows tall, pane is %d", h, m.paneRows())
	}
	if w := lipgloss.Width(out); w > m.mainWidth() {
		t.Errorf("form is %d cols wide, pane is %d", w, m.mainWidth())
	}
	if !strings.Contains(out, "╯") {
		t.Errorf("form lost its bottom border (cut off rather than laid out)")
	}
	if v := m.View(); lipgloss.Height(v) != m.h {
		t.Errorf("whole view is %d rows for a %d-row terminal", lipgloss.Height(v), m.h)
	}
	if t.Failed() {
		t.Log("\n" + out)
	}
}

// The modal fits the pane at every reasonable terminal size — and never exceeds
// it even when the pane is too small for the form's chrome.
func TestFormFitsPaneAcrossSizes(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {120, 40}, {100, 16}, {60, 14}, {50, 10}, {40, 8}} {
		m := &model{w: sz[0], h: sz[1]}
		m.openNewWorkgroupForm()
		m.handleForm(key("j")) // onto Prompt
		m.handleForm(key("i"))
		m.handleForm(paste(longPrompt(30)))
		out := m.renderForm()
		if h := lipgloss.Height(out); h > m.paneRows() {
			t.Errorf("%dx%d: form is %d rows tall, pane is %d", sz[0], sz[1], h, m.paneRows())
		}
		if w := lipgloss.Width(out); w > m.mainWidth() {
			t.Errorf("%dx%d: form is %d cols wide, pane is %d", sz[0], sz[1], w, m.mainWidth())
		}
		if sz[1] >= 24 && !strings.Contains(out, "╯") {
			t.Errorf("%dx%d: form lost its bottom border:\n%s", sz[0], sz[1], out)
		}
	}
}

// A multi-line value scrolls inside its rows: the window follows the cursor,
// showing the top of the prompt at line 1 and the bottom at the end.
func TestMultilineFieldScrollsWithCursor(t *testing.T) {
	m := &model{w: 100, h: 24}
	m.openAddAgentForm("wg1", "payments")
	m.handleForm(key("i"))
	m.handleForm(paste(longPrompt(40)))

	out := plain(m.renderForm()) // cursor at the end after the paste
	if !strings.Contains(out, "line 39 of") || strings.Contains(out, "line 00 of") {
		t.Errorf("after paste the window should show the last line, not the first:\n%s", out)
	}
	if !strings.Contains(out, "[line 40/40]") {
		t.Errorf("label should report the cursor line:\n%s", out)
	}
	m.handleForm(key("<esc>"))
	m.handleForm(key("0"))
	out = plain(m.renderForm())
	if !strings.Contains(out, "line 00 of") || strings.Contains(out, "line 39 of") {
		t.Errorf("cursor at start should scroll the window to the top:\n%s", out)
	}
	if !strings.Contains(out, "[line 1/40]") {
		t.Errorf("label should report line 1:\n%s", out)
	}
	// Only the active text field expands; a multi-line value that is not
	// selected collapses to one summary row.
	m.handleForm(key("j"))
	out = plain(m.renderForm())
	if !strings.Contains(out, "(+39 lines)") || strings.Contains(out, "line 01 of") {
		t.Errorf("inactive field should be a one-row summary:\n%s", out)
	}
}

// Terminals end pasted lines with LF, CR, or CRLF; the field keeps one newline
// per line either way, and never a stray carriage return.
func TestPasteNormalizesLineEndings(t *testing.T) {
	m := &model{}
	m.openNewRepoAgentForm("repo", "Repo")
	m.handleForm(key("i"))
	m.handleForm(paste("one\r\ntwo\rthree\nfour"))
	if got := m.form.fields[0].value; got != "one\ntwo\nthree\nfour" {
		t.Fatalf("got %q", got)
	}
}

// A paste while the field is in normal mode is text to keep, not a burst of vim
// commands: it lands in the field instead of being ignored or executed.
func TestPasteInNormalModeInserts(t *testing.T) {
	m := &model{}
	m.openNewRepoAgentForm("repo", "Repo")
	m.handleForm(paste("x\ndd"))
	if got := m.form.fields[0].value; got != "x\ndd" {
		t.Fatalf("got %q, want the pasted text", got)
	}
	if m.form.insert {
		t.Fatal("a paste should leave the field in normal mode")
	}
}

// Wrapping breaks at newlines and after blanks, hard-breaks over-long words,
// and always yields a row for an empty or trailing line so every cursor
// position has a row.
func TestWrapSpans(t *testing.T) {
	rows := func(s string, w int) []string {
		r := []rune(s)
		var out []string
		for _, sp := range wrapSpans(r, w) {
			out = append(out, string(r[sp.start:sp.end]))
		}
		return out
	}
	cases := []struct {
		in   string
		w    int
		want []string
	}{
		{"", 10, []string{""}},
		{"abc", 10, []string{"abc"}},
		{"abc\n", 10, []string{"abc", ""}},
		{"a\n\nb", 10, []string{"a", "", "b"}},
		{"the quick brown fox", 10, []string{"the quick ", "brown fox"}},
		{"abcdefghijkl", 5, []string{"abcde", "fghij", "kl"}},
		{"日本語 text", 4, []string{"日本", "語 ", "text"}},
	}
	for _, tc := range cases {
		got := rows(tc.in, tc.w)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("wrap(%q, %d) = %q, want %q", tc.in, tc.w, got, tc.want)
		}
	}
}

// The cursor just past a wrapped row's last rune starts the next row; just past
// a logical line's last rune it stays on that line.
func TestCursorRow(t *testing.T) {
	r := []rune("the quick brown fox\nend")
	spans := wrapSpans(r, 10) // "the quick " | "brown fox" | "end"
	for c, want := range map[int]int{0: 0, 9: 0, 10: 1, 19: 1, 20: 2, 23: 2} {
		if got := cursorRow(spans, r, c); got != want {
			t.Errorf("cursorRow(%d) = %d, want %d", c, got, want)
		}
	}
}
