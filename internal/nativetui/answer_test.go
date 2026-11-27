package nativetui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"amux/internal/core"
	"amux/internal/queue"
)

// answerEntry is a question entry with one question of each input kind.
func answerEntry() queue.Entry {
	qs, _ := queue.ParseQuestions(json.RawMessage(`{"questions":[
		{"header":"Scope","question":"Which parts of the service should this change touch? Pick every part the rollout plan has to cover before we start.","multiSelect":true,
		 "options":[{"label":"api","description":"the public HTTP API and its OpenAPI document"},{"label":"web"},{"label":"cli","preview":"amux inbox answer <id>\n  --choice <label>"}]},
		{"header":"Ship","question":"Ship today?","options":[{"label":"yes"},{"label":"no","description":"wait for the review"}]},
		{"question":"Anything else the reviewer should know?"}]}`))
	return queue.Entry{ID: "q-answer", Kind: queue.KindQuestion, Session: "m1", Title: "Which parts? (+2 more)", Questions: qs}
}

func answerModel(t *testing.T, w, h int, e queue.Entry) *model {
	t.Helper()
	m := resourcesModel()
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	m.Update(queueFrame(t, queue.Overview{Entries: []queue.Entry{e}, Counts: queue.Counts{Total: 1}}))
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.qp.answer == nil {
		t.Fatal("Enter did not open the answer view")
	}
	return m
}

func press(m *model, keys ...string) {
	for _, k := range keys {
		switch k {
		case "enter":
			m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		case "down":
			m.Update(tea.KeyMsg{Type: tea.KeyDown})
		case "up":
			m.Update(tea.KeyMsg{Type: tea.KeyUp})
		case "tab":
			m.Update(tea.KeyMsg{Type: tea.KeyTab})
		case "space":
			m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
		case "backspace":
			m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		case "ctrl+s":
			m.Update(tea.KeyMsg{Type: tea.KeyCtrlS})
		case "ctrl+j":
			m.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
		case "esc":
			m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		default:
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
		}
	}
}

// TestAnswerViewControls: each question gets its own native control —
// checkboxes toggle, a radio pick clears the other text and moves on, Other
// text replaces a radio pick, a text question takes typed lines — and the
// answer goes out as per-question answers.
func TestAnswerViewControls(t *testing.T) {
	m := answerModel(t, 100, 40, answerEntry())
	a := m.qp.answer
	// Scope (multi): toggle api and cli, untoggle api, toggle it back; then
	// type an Other.
	press(m, "space", "3", "1", "1", "down", "down", "down")
	if !a.onText() {
		t.Fatalf("cursor at %+v, want Scope's Other row", a.row())
	}
	press(m, "d", "o", "c", "s", "x", "backspace")
	// Ship (single): Enter picks and moves on; typing Other clears the pick.
	press(m, "down", "enter")
	if got := a.answers()[1]; len(got.Choice) != 1 || got.Choice[0] != "yes" {
		t.Fatalf("Ship = %+v", got)
	}
	if a.row() != (answerRow{2, rowText}) {
		t.Fatalf("after a pick the cursor is at %+v, want the next question", a.row())
	}
	press(m, "up", "l", "a", "t", "e", "r")
	if got := a.answers()[1]; len(got.Choice) != 0 || got.Other != "later" {
		t.Fatalf("Other did not replace the pick: %+v", got)
	}
	// …and picking again clears the Other text.
	press(m, "up", "space")
	if got := a.answers()[1]; len(got.Choice) != 1 || got.Choice[0] != "no" || got.Other != "" {
		t.Fatalf("pick did not clear Other: %+v", got)
	}
	// The text question: digits and j/k are text here, Ctrl+J a new line.
	press(m, "down", "down", "j", "1", "ctrl+j", "k")
	fields, err := a.fields()
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"choice":["api","cli"],"other":"docs"},{"choice":["no"]},{"other":"j1\nk"}]`
	if fields[core.QueueAnswersField] != want {
		t.Errorf("answers = %s, want %s", fields[core.QueueAnswersField], want)
	}
	press(m, "ctrl+s")
	if m.qp.answer != nil || !strings.Contains(m.status, "answering q-answer") {
		t.Errorf("Ctrl+S did not send: answer=%v status=%q", m.qp.answer, m.status)
	}
}

// TestAnswerViewRefusesAnEmptyAnswer: sending with nothing chosen shows why
// and keeps the view open; Esc goes back to the queue.
func TestAnswerViewRefusesAnEmptyAnswer(t *testing.T) {
	m := answerModel(t, 100, 40, answerEntry())
	press(m, "ctrl+s")
	if m.qp.answer == nil || m.qp.answer.err == "" {
		t.Fatal("an empty answer was sent")
	}
	if !strings.Contains(ansi.Strip(m.renderQueue()), "no question is answered") {
		t.Errorf("the refusal is not shown:\n%s", ansi.Strip(m.renderQueue()))
	}
	press(m, "esc")
	if m.qp.answer != nil || !m.qp.open {
		t.Error("Esc did not return to the queue")
	}
}

// TestAnswerViewTextEntries: a goal ask answers with one text row under its
// whole note, sent as text.
func TestAnswerViewTextEntries(t *testing.T) {
	e := queue.Entry{ID: "g-1", Kind: queue.KindGoalInput, Session: "wg1", Title: "Which region?",
		Body: "Which region should the rollout start in? eu-west has the most tenants, us-east the most traffic."}
	m := answerModel(t, 60, 30, e)
	if !strings.Contains(strings.Join(strings.Fields(ansi.Strip(m.renderQueue())), " "), "us-east the most traffic.") {
		t.Errorf("the note is cut:\n%s", ansi.Strip(m.renderQueue()))
	}
	press(m, "e", "u", " ", "w")
	fields, err := m.qp.answer.fields()
	if err != nil || fields[core.QueueTextField] != "eu w" || fields[core.QueueAnswersField] != "" {
		t.Errorf("fields = %v, %v", fields, err)
	}
}

// TestAnswerViewGolden: the view at a narrow and a wide pane, with choices
// made, wraps everything and fits the width.
func TestAnswerViewGolden(t *testing.T) {
	a := newAnswerState(answerEntry())
	a.choose(0, 0)
	a.choose(0, 2)
	a.other[0] = "and the docs site, which links to every endpoint"
	a.choose(1, 1)
	a.cursor = 2 // Scope's cli option, with its preview
	for _, w := range []int{40, 120} {
		lines := a.answerBody(w).body
		assertFits(t, lines, w)
		checkGolden(t, fmt.Sprintf("answer-view/controls-%d", w), plainLines(lines))
	}
}

// TestAnswerViewPaging: in a short pane PgDn walks the focus down to Send and
// PgUp back to the first option, and the focused control is always shown.
func TestAnswerViewPaging(t *testing.T) {
	m := answerModel(t, 80, 12, answerEntry())
	a := m.qp.answer
	for i := 0; i < 20 && a.row().q >= 0; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
		if !strings.Contains(ansi.Strip(m.renderQueue()), "▸") {
			t.Fatalf("focus off screen after PgDn %d:\n%s", i, ansi.Strip(m.renderQueue()))
		}
	}
	if a.row().q >= 0 {
		t.Fatalf("PgDn never reached Send: %+v", a.row())
	}
	for i := 0; i < 20 && a.cursor > 0; i++ {
		m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	}
	if a.cursor != 0 || !strings.Contains(ansi.Strip(m.renderQueue()), "[Scope] Which parts") {
		t.Errorf("PgUp did not return to the top:\n%s", ansi.Strip(m.renderQueue()))
	}
}
