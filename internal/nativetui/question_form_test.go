package nativetui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"amux/internal/core"
	"amux/internal/queue"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func formEntry() queue.Entry {
	f := harnessproto.Form{Version: 2, Source: harnessproto.FormSourceClaudeAsk, Cancel: true, Reply: true, Delivery: harnessproto.DeliveryStructured,
		Questions: []harnessproto.FormQuestion{
			{ID: "q0", Header: "Layout", Question: "Which layout should the settings page use? It has to work on phones too.", Other: true, Notes: true,
				Options: []harnessproto.FormOption{
					{Label: "Tabs", Description: "One tab per area", Preview: "┌ General │ Account ┐\x1b[31m"},
					{Label: "Single page", Description: "One long scroll"},
				}},
			{ID: "q1", Header: "Extras", Question: "Which extras?", MultiSelect: true, Other: true,
				Options: []harnessproto.FormOption{{Label: "Search"}, {Label: "Dark mode"}}},
			{ID: "q2", Header: "Token", Question: "Deploy token?", Other: true, Secret: true},
		}}
	return queue.Entry{ID: "q-aaaaaaaaaa", Kind: queue.KindQuestion, Session: "m1", SessionTitle: "api work", Title: "Which layout?", Form: &f}
}

func qkey(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func typeText(m *model, s string) {
	for _, r := range s {
		m.handleQuestionFormKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// TestQuestionFormAnswer drives every feature: a pick with a note, a
// multi-select with Other, a secret typed masked, review and submit.
func TestQuestionFormAnswer(t *testing.T) {
	m := resourcesModel()
	ov := queue.Overview{Entries: []queue.Entry{formEntry()}, Counts: queue.Counts{Total: 1}}
	m.Update(queueFrame(t, ov))
	m.Update(qkey("i"))
	m.Update(qkey("enter"))
	if m.qf == nil {
		t.Fatal("Enter did not open the question form")
	}
	// Question 1: note, then pick 1 (moves on to question 2).
	m.Update(qkey("n"))
	typeText(m, "keep icons")
	m.Update(qkey("enter"))
	m.Update(qkey("1"))
	if m.qf.tab != 1 {
		t.Fatalf("a single-select pick did not move on: tab %d", m.qf.tab)
	}
	// Question 2: toggle both, untoggle the first, add Other.
	m.Update(qkey("1"))
	m.Update(qkey("2"))
	m.Update(qkey("1"))
	m.Update(qkey("o"))
	typeText(m, "export")
	m.Update(qkey("enter"))
	m.Update(qkey("right"))
	// Question 3: a secret, masked while typed and in review.
	m.Update(qkey("o"))
	typeText(m, "s3cr3t")
	if out := ansi.Strip(strings.Join(m.questionFormLines(100, 40), "\n")); strings.Contains(out, "s3cr3t") || !strings.Contains(out, "••••••") {
		t.Fatalf("secret not masked while typed:\n%s", out)
	}
	m.Update(qkey("enter"))
	m.Update(qkey("right"))
	review := ansi.Strip(strings.Join(m.questionFormLines(100, 40), "\n"))
	for _, want := range []string{"→ Tabs", "note: keep icons", "→ Dark mode, export", "(secret text)", "[✔ Layout]"} {
		if !strings.Contains(review, want) {
			t.Errorf("review missing %q:\n%s", want, review)
		}
	}
	if strings.Contains(review, "s3cr3t") {
		t.Fatal("review shows the secret")
	}
	a := m.qf.answer()
	if err := harnessproto.ValidateAnswer(*formEntry().Form, a); err != nil {
		t.Fatalf("answer %+v: %v", a, err)
	}
	if a.Answers[0].Selected[0] != 0 || a.Answers[0].Note != "keep icons" || len(a.Answers[1].Selected) != 1 || a.Answers[1].Selected[0] != 1 ||
		*a.Answers[1].Other != "export" || *a.Answers[2].Other != "s3cr3t" {
		t.Fatalf("answer = %+v", a)
	}
	m.Update(qkey("enter"))
	if m.qf != nil {
		t.Fatal("Enter on review did not submit")
	}
}

func TestQuestionFormRefusesIncomplete(t *testing.T) {
	m := resourcesModel()
	m.openQuestionForm(formEntry())
	m.qf.tab = 3
	m.handleQuestionFormKey(qkey("enter"))
	if m.qf == nil || m.qf.err == "" {
		t.Fatalf("an incomplete form was sent: %+v", m.qf)
	}
}

func TestQuestionFormOneTapAndDismiss(t *testing.T) {
	e := formEntry()
	f := *e.Form
	q := f.Questions[0]
	q.Notes = false
	f.Questions = []harnessproto.FormQuestion{q}
	e.Form = &f
	m := resourcesModel()
	m.openQuestionForm(e)
	m.handleQuestionFormKey(qkey("2"))
	if m.qf != nil {
		t.Fatal("a lone single-select question did not answer on one key")
	}
	m.openQuestionForm(e)
	m.handleQuestionFormKey(qkey("x"))
	if m.qf != nil {
		t.Fatal("x did not dismiss")
	}
	m.openQuestionForm(e)
	m.handleQuestionFormKey(qkey("m"))
	typeText(m, "talk first")
	if m.qf.edit != editReply || m.qf.buf != "talk first" {
		t.Fatalf("reply edit = %+v", m.qf)
	}
	m.handleQuestionFormKey(qkey("enter"))
	if m.qf != nil {
		t.Fatal("the reply was not sent")
	}
}

// TestQuestionFormLayout pins the wrapped layout at a wide and a narrow
// width: nothing wider than the pane, previews beside the list when wide and
// under it when narrow, and no agent control sequence passed through.
func TestQuestionFormLayout(t *testing.T) {
	for _, w := range []int{120, 60} {
		m := resourcesModel()
		m.openQuestionForm(formEntry())
		lines := m.questionFormLines(w, 30)
		joined := strings.Join(lines, "\n")
		if strings.Contains(joined, "\x1b[31m") {
			t.Fatalf("%d: control sequence passed through", w)
		}
		for i, l := range lines {
			if ansi.StringWidth(l) > w {
				t.Fatalf("%d: line %d is %d cells: %q", w, i, ansi.StringWidth(l), ansi.Strip(l))
			}
		}
		plain := ansi.Strip(joined)
		if !strings.Contains(plain, "phones too") || !strings.Contains(plain, "answers go straight to the agent") || (w >= 100 && !strings.Contains(plain, "answer in pane")) {
			t.Errorf("%d: form text missing:\n%s", w, plain)
		}
		previewBeside := false
		for _, l := range strings.Split(plain, "\n") {
			if strings.Contains(l, "Tabs") && strings.Contains(l, "│ preview") {
				previewBeside = true
			}
		}
		if previewBeside != (w >= 100) {
			t.Errorf("%d: preview beside = %v:\n%s", w, previewBeside, plain)
		}
	}
}

func TestQuestionFormPaneDelivery(t *testing.T) {
	e := formEntry()
	f := *e.Form
	f.Delivery = harnessproto.DeliveryPane
	e.Form = &f
	m := resourcesModel()
	m.openQuestionForm(e)
	m.handleQuestionFormKey(qkey("1"))
	m.handleQuestionFormKey(qkey("x"))
	if m.qf == nil || m.qf.answered(0) {
		t.Fatal("a pane-delivered form took an answer")
	}
	if out := ansi.Strip(strings.Join(m.questionFormLines(100, 30), "\n")); !strings.Contains(out, "answer this in the agent's pane") {
		t.Fatalf("pane delivery not said:\n%s", out)
	}
}

func TestReleaseFocusedQuestion(t *testing.T) {
	m := resourcesModel()
	ov := queue.Overview{Entries: []queue.Entry{formEntry()}}
	m.qp.ov = &ov
	m.attached, m.tab, m.focus = "m1", tabAgent, focusAgent
	if m.releaseFocusedQuestion() == nil || !m.qp.released["q-aaaaaaaaaa"] {
		t.Fatal("focusing the pane did not release its held question")
	}
	if m.releaseFocusedQuestion() != nil {
		t.Fatal("released twice")
	}
	m.focus = focusSidebar
	m.qp.released = nil
	if m.releaseFocusedQuestion() != nil {
		t.Fatal("released without focus")
	}
	_ = core.ActionQueueRelease
}
