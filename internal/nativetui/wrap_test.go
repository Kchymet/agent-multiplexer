package nativetui

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"amux/internal/queue"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata/")

// checkGolden compares got with testdata/<name>.golden (run with -update to
// rewrite it), like cmd/amux's help goldens.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s drifted (run with -update if intended)\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}

// assertFits fails on any rendered line wider than w cells.
func assertFits(t *testing.T, lines []string, w int) {
	t.Helper()
	for i, l := range lines {
		if got := ansi.StringWidth(l); got > w {
			t.Errorf("line %d is %d cells, wider than %d: %q", i, got, w, ansi.Strip(l))
		}
	}
}

// plainLines strips styling and trailing padding, for goldens.
func plainLines(lines []string) string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimRight(ansi.Strip(l), " ")
	}
	return strings.Join(out, "\n") + "\n"
}

const longURL = "https://github.com/Kchymet/agent-multiplexer/blob/master/internal/nativetui/queue.go#L280-L320?plain=1"

func wrapFixtures() map[string]queue.Entry {
	return map[string]queue.Entry{
		"long-question": {ID: "q-long", Kind: queue.KindQuestion, Session: "m2", Workgroup: "question-prompt-parity",
			SessionTitle: "a session title long enough to need wrapping on a narrow pane",
			Title:        "Should the settings page use tabs, one long scrolling page, or a wizard",
			Questions: []queue.Question{{Header: "Layout", Question: "Should the settings page use tabs, one long scrolling page, or a wizard that walks through each section in order? Consider that most users only change one setting per visit.",
				Options: []queue.Option{{Label: "Tabs (Recommended)"}, {Label: "Single page"}, {Label: "Wizard"}}}}},
		"long-options": {ID: "q-opts", Kind: queue.KindQuestion, Session: "m3", Title: "Which database?",
			Questions: []queue.Question{
				{Header: "Database", Question: "Which database should the billing service use in production?", Options: []queue.Option{
					{Label: "PostgreSQL with logical replication to the analytics warehouse", Description: "A managed server; the team already runs two clusters, so on-call knows it, but it adds a network hop to every request."},
					{Label: "SQLite", Description: "Embedded, zero-ops, one file per tenant; replication would need Litestream or similar tooling."},
				}},
				{Header: "Extras", Question: "Which extras?", MultiSelect: true, Options: []queue.Option{{Label: "Backups"}, {Label: "Read replicas", Description: "two regions"}}},
			}},
		"long-url": {ID: "q-url", Kind: queue.KindQuestion, Session: "m4", Title: "Is this the right file?",
			Questions: []queue.Question{{Question: "Is this the right file to change? " + longURL,
				Options: []queue.Option{{Label: "Yes", Description: "see " + longURL}, {Label: "No, use /Users/someone/src/agent-multiplexer/internal/nativetui/view_with_a_long_name.go"}}}}},
		"wide-chars": {ID: "q-wide", Kind: queue.KindQuestion, Session: "m5", Title: "日本語",
			Questions: []queue.Question{{Header: "言語", Question: "どの言語でドキュメントを書きますか？🎉 全角の文字と絵文字が混ざった長い質問文がきちんと折り返されることを確認します。",
				Options: []queue.Option{{Label: "日本語 🇯🇵 (おすすめ)", Description: "既存のドキュメントはすべて日本語で書かれているので、そのまま続けるのが簡単です 📚"}, {Label: "English 🇬🇧"}}}}},
		"long-decision": {ID: "d-long", Kind: queue.KindDecision, Session: "wg1", Item: "it-1", Title: "Rewrite the billing service before the Q3 launch or ship the patch?",
			Body:    "The billing service has accumulated three years of special cases. A rewrite takes about six weeks; the patch takes one but leaves the double-charge race open.\nSee " + longURL,
			Suggest: "ship the patch now, start the rewrite in a new workgroup after launch"},
	}
}

// TestQuestionWrapGolden: a selected entry wraps every part of the question
// at a narrow (40) and a wide (120) pane, with hanging indents, and no line
// passes the pane edge.
func TestQuestionWrapGolden(t *testing.T) {
	for name, e := range wrapFixtures() {
		for _, w := range []int{40, 120} {
			t.Run(fmt.Sprintf("%s-%d", name, w), func(t *testing.T) {
				lines := queueEntryLines(e, true, w)
				assertFits(t, lines, w)
				checkGolden(t, fmt.Sprintf("question-wrap/%s-%d", name, w), plainLines(lines))
			})
		}
	}
}

// TestOptionLinesPreviewGolden: previews (shown as plain text under the
// option, line breaks kept) and multi-select marks hang under the label.
func TestOptionLinesPreviewGolden(t *testing.T) {
	o := questionOption{Number: 1, Mark: "[ ]", Label: "Tabs, one per area of the settings page (Recommended)",
		Description: "Each tab holds one area; deep links open a tab directly.",
		Preview:     "┌Tabs┐\n│ General │ Account │ Notifications │ Billing │ Advanced │\n\tsee " + longURL}
	for _, w := range []int{40, 120} {
		lines := optionLines(o, 2, w, true)
		assertFits(t, lines, w)
		checkGolden(t, fmt.Sprintf("question-wrap/option-preview-%d", w), plainLines(lines))
	}
}

func TestWrapText(t *testing.T) {
	for _, tc := range []struct {
		in   string
		w    int
		want []string
	}{
		{"hello world this is a long line", 12, []string{"hello world", "this is a", "long line"}},
		{"see https://example.com/abcdefghij", 10, []string{"see", "https://ex", "ample.com/", "abcdefghij"}},
		{"日本語のテキスト", 6, []string{"日本語", "のテキ", "スト"}},
		{"a\x1b[2Jb\n\nc\td", 20, []string{"a[2Jb", "", "c    d"}},
		{"", 5, []string{""}},
	} {
		got := wrapText(tc.in, tc.w)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("wrapText(%q, %d) = %q, want %q", tc.in, tc.w, got, tc.want)
		}
		assertFits(t, got, tc.w)
	}
	// A prefix too wide to hang under drops the text to its own lines.
	got := hang("    10. ", "a label", 14, nil, true)
	if len(got) != 2 || strings.TrimSpace(got[0]) != "10." || got[1] != "      a label" {
		t.Errorf("narrow hang = %q", got)
	}
}

func TestScrollWindow(t *testing.T) {
	for _, tc := range []struct{ top, total, avail, s, e, want int }{
		{0, 100, 10, 0, 3, 0},
		{0, 100, 10, 20, 25, 15},  // scroll down to show the selection
		{50, 100, 10, 20, 25, 20}, // scroll up to it
		{95, 100, 10, 0, 0, 90},   // clamp to the end
		{0, 100, 10, 20, 50, 20},  // tall selection: start at its top
		{45, 100, 10, 20, 50, 40}, // never past its end
		{30, 100, 10, 20, 50, 30}, // free to page inside it
		{-5, 5, 10, 0, 5, 0},      // everything fits
	} {
		if got := scrollWindow(tc.top, tc.total, tc.avail, tc.s, tc.e); got != tc.want {
			t.Errorf("scrollWindow(%+v) = %d, want %d", tc, got, tc.want)
		}
	}
}

func wrapOverview() queue.Overview {
	f := wrapFixtures()
	return queue.Overview{
		Entries: []queue.Entry{f["long-question"], f["long-options"], f["long-url"], f["wide-chars"], f["long-decision"]},
		Counts:  queue.Counts{Total: 5},
	}
}

// visibleSelection reports whether the selected entry's head row is on screen.
func visibleSelection(t *testing.T, m *model) bool {
	t.Helper()
	e, _ := m.qp.selectedEntry()
	return strings.Contains(ansi.Strip(m.renderQueue()), "▸ "+e.Kind)
}

// selectionOnScreen reports whether the window shows the selected entry: all
// of it when it fits, else only rows of it (paging walks through it).
func selectionOnScreen(m *model) bool {
	l := m.queueLayout(m.mainWidth(), m.paneRows())
	top := l.top(m.qp.scroll, m.qp.sel)
	s, e := l.selSpan(m.qp.sel)
	if e-s <= l.avail {
		return s >= top && e <= top+l.avail
	}
	return top >= s && top+l.avail <= e
}

// TestQueueScrollKeepsSelectionVisible: in a short pane the wrapped queue
// scrolls; moving the selection and paging always keep it on screen, paging
// walks through every entry, and the header shows where the window is.
func TestQueueScrollKeepsSelectionVisible(t *testing.T) {
	m := resourcesModel()
	m.Update(tea.WindowSizeMsg{Width: 70, Height: 16})
	m.Update(queueFrame(t, wrapOverview()))
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	if !strings.Contains(ansi.Strip(m.renderQueue()), "↕1–") {
		t.Fatalf("no scroll indicator on an overflowing queue:\n%s", ansi.Strip(m.renderQueue()))
	}
	for i := 0; i < 6; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		if !visibleSelection(t, m) {
			t.Fatalf("after %d× j the selection %d is off screen:\n%s", i+1, m.qp.sel, ansi.Strip(m.renderQueue()))
		}
	}
	if m.qp.sel != 4 {
		t.Fatalf("j stopped at %d", m.qp.sel)
	}
	seen := map[int]bool{}
	for i := 0; i < 40 && !(m.qp.sel == 0 && seen[0]); i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
		seen[m.qp.sel] = true
		if !selectionOnScreen(m) {
			t.Fatalf("pgup left selection %d off screen:\n%s", m.qp.sel, ansi.Strip(m.renderQueue()))
		}
	}
	for i := 0; i < 40 && m.qp.sel != 4; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
		seen[m.qp.sel] = true
		if !selectionOnScreen(m) {
			t.Fatalf("pgdown left selection %d off screen:\n%s", m.qp.sel, ansi.Strip(m.renderQueue()))
		}
	}
	if m.qp.sel != 4 || len(seen) != 5 {
		t.Fatalf("paging did not walk every entry: sel %d, seen %v", m.qp.sel, seen)
	}
}

// TestQueueRewrapsOnResize: a resize re-wraps on the next frame; nothing is
// cut at the pane edge at either width, and the end of a long question is
// readable at both.
func TestQueueRewrapsOnResize(t *testing.T) {
	m := resourcesModel()
	m.Update(queueFrame(t, wrapOverview()))
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	for _, size := range []tea.WindowSizeMsg{{Width: 160, Height: 60}, {Width: 70, Height: 60}, {Width: 140, Height: 60}} {
		m.Update(size)
		w := m.mainWidth()
		lines := m.queueLines(w, m.paneRows())
		assertFits(t, lines, w)
		joined := strings.Join(strings.Fields(ansi.Strip(strings.Join(lines, " "))), " ")
		if !strings.Contains(joined, "change one setting per visit.") {
			t.Errorf("at %d cols the end of the question is cut:\n%s", w, plainLines(lines))
		}
	}
}

// TestQueueReplyFormShowsQuestion: the reply form, which covers the queue,
// shows the question it answers wrapped inside the box, capped so the box
// still fits the pane, and its title is the only line cut with an ellipsis.
func TestQueueReplyFormShowsQuestion(t *testing.T) {
	m := resourcesModel()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.Update(queueFrame(t, wrapOverview()))
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if m.form == nil {
		t.Fatal("r did not open the reply form")
	}
	out := m.renderForm()
	lines := strings.Split(out, "\n")
	assertFits(t, lines, m.mainWidth())
	if len(lines) > m.paneRows() {
		t.Errorf("form is %d rows in a %d-row pane", len(lines), m.paneRows())
	}
	joined := strings.Join(strings.Fields(strings.ReplaceAll(ansi.Strip(out), "│", " ")), " ")
	for _, want := range []string{"[Layout] Should the settings page use tabs", "Answer:"} {
		if !strings.Contains(joined, want) {
			t.Errorf("reply form missing %q:\n%s", want, ansi.Strip(out))
		}
	}
	// A short pane drops the context before the answer field.
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 12})
	if lines := strings.Split(m.renderForm(), "\n"); len(lines) > m.paneRows() || !strings.Contains(ansi.Strip(m.renderForm()), "Answer:") {
		t.Errorf("short pane form:\n%s", ansi.Strip(m.renderForm()))
	}
}
