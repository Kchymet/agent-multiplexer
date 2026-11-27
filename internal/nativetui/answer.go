package nativetui

import (
	"encoding/json"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"amux/internal/core"
	"amux/internal/queue"
)

// The answer view (Enter or r on a queue entry; docs/question-prompts.md):
// the whole question laid out at the pane's width — header chips, the full
// question text, every option with its description and preview — with one
// native control per question: a radio list for a single_select, checkboxes
// for a multi_select, an Other text row beside the options when the question
// allows it, and a text row for a question without options, a goal ask or a
// decision. Nothing is cut: the view scrolls, and keeps the focused control in
// view. It replaces the queue panel's body until it is sent or cancelled. Agent
// text is untrusted and drawn as plain text.

// answerRow is one focusable row: an option (opt >= 0) or the Other / text
// row (opt == rowText) of question q, or the send button (q == -1).
type answerRow struct{ q, opt int }

const rowText = -1

// answerState is the open answer view.
type answerState struct {
	entry queue.Entry
	// qs are the controls: an entry's normalized questions, or one text
	// question carrying a goal ask's or decision's text.
	qs     []queue.Question
	picked [][]bool // per question, per option
	other  []string // per question: Other / text answer
	rows   []answerRow
	cursor int
	scroll int // desired first body row; rendering keeps the cursor in view
	err    string
}

func newAnswerState(e queue.Entry) *answerState {
	a := &answerState{entry: e}
	if e.Kind == queue.KindQuestion && len(e.Questions) > 0 {
		for _, q := range e.Questions {
			a.qs = append(a.qs, q.Normalize())
		}
	} else {
		a.qs = []queue.Question{{Question: firstNonEmptyText(e.Body, e.Title), Input: queue.InputText}}
	}
	a.picked = make([][]bool, len(a.qs))
	a.other = make([]string, len(a.qs))
	for i, q := range a.qs {
		a.picked[i] = make([]bool, len(q.Options))
		for o := range q.Options {
			a.rows = append(a.rows, answerRow{i, o})
		}
		if q.Input == queue.InputText || q.AllowOther {
			a.rows = append(a.rows, answerRow{i, rowText})
		}
	}
	a.rows = append(a.rows, answerRow{-1, 0})
	return a
}

// structured reports an entry answered per question (QueueAnswersField)
// rather than with one text (QueueTextField).
func (a *answerState) structured() bool {
	return a.entry.Kind == queue.KindQuestion && len(a.entry.Questions) > 0
}

func (a *answerState) row() answerRow { return a.rows[a.cursor] }

func (a *answerState) onText() bool { r := a.row(); return r.q >= 0 && r.opt == rowText }

func (a *answerState) move(d int) {
	a.cursor = min(max(0, a.cursor+d), len(a.rows)-1)
}

// nextQuestion moves to the first row of the question after q (or send).
func (a *answerState) nextQuestion(q int) {
	for i, r := range a.rows {
		if r.q > q || r.q < 0 {
			a.cursor = i
			return
		}
	}
}

// choose toggles (multi_select) or picks (single_select) option o of
// question q. A single_select's pick clears its Other text: a choice or other
// text, never both.
func (a *answerState) choose(q, o int) {
	if q < 0 || q >= len(a.qs) || o < 0 || o >= len(a.picked[q]) {
		return
	}
	a.err = ""
	if a.qs[q].Input == queue.InputMultiSelect {
		a.picked[q][o] = !a.picked[q][o]
		return
	}
	on := !a.picked[q][o]
	clear(a.picked[q])
	a.picked[q][o] = on
	if on {
		a.other[q] = ""
	}
}

// typeText edits the focused text row; typing into a single_select's Other
// clears its choice.
func (a *answerState) typeText(edit func(string) string) {
	r := a.row()
	a.other[r.q] = edit(a.other[r.q])
	a.err = ""
	if a.qs[r.q].Input == queue.InputSingleSelect && strings.TrimSpace(a.other[r.q]) != "" {
		clear(a.picked[r.q])
	}
}

// answers are the per-question answers in the questions' order.
func (a *answerState) answers() []queue.Answer {
	out := make([]queue.Answer, len(a.qs))
	for i, q := range a.qs {
		for o, on := range a.picked[i] {
			if on {
				out[i].Choice = append(out[i].Choice, q.Options[o].Label)
			}
		}
		out[i].Other = strings.TrimSpace(a.other[i])
	}
	return out
}

// fields are the ActionQueueAnswer fields the view sends, or an error to
// show instead.
func (a *answerState) fields() (map[string]string, error) {
	if !a.structured() {
		text := strings.TrimSpace(a.other[0])
		if text == "" {
			return nil, fmt.Errorf("type an answer first")
		}
		return map[string]string{core.QueueTextField: text}, nil
	}
	as := a.answers()
	if err := queue.ValidateAnswers(a.qs, as); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(as)
	if err != nil {
		return nil, err
	}
	return map[string]string{core.QueueAnswersField: string(raw)}, nil
}

// openAnswer opens the answer view on an entry.
func (m *model) openAnswer(e queue.Entry) {
	m.qp.answer = newAnswerState(e)
}

func (m *model) handleAnswerKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	a := m.qp.answer
	if a.onText() && (k.Paste || k.Type == tea.KeyRunes || k.Type == tea.KeySpace) {
		rs := k.Runes
		if k.Type == tea.KeySpace {
			rs = []rune{' '}
		}
		rs = pasteRunes(rs)
		if a.qs[a.row().q].Input != queue.InputText { // Other is one line
			rs = []rune(strings.ReplaceAll(string(rs), "\n", " "))
		}
		a.typeText(func(s string) string { return s + string(rs) })
		return m, nil
	}
	switch key := k.String(); key {
	case "esc", "ctrl+c":
		m.qp.answer = nil
		m.status = "answer cancelled"
	case "up", "shift+tab":
		a.move(-1)
	case "down", "tab":
		a.move(1)
	case "k":
		a.move(-1)
	case "j":
		a.move(1)
	case "home":
		a.cursor = 0
	case "end":
		a.cursor = len(a.rows) - 1
	case "pgdown", "ctrl+f":
		m.answerPage(1)
	case "pgup", "ctrl+b":
		m.answerPage(-1)
	case "backspace", "ctrl+h":
		if a.onText() {
			a.typeText(func(s string) string {
				if r := []rune(s); len(r) > 0 {
					return string(r[:len(r)-1])
				}
				return s
			})
		}
	case "ctrl+u":
		if a.onText() {
			a.typeText(func(string) string { return "" })
		}
	case "ctrl+j", "alt+enter":
		if a.onText() && a.qs[a.row().q].Input == queue.InputText {
			a.typeText(func(s string) string { return s + "\n" })
		}
	case "ctrl+s":
		return m, m.sendAnswer()
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		r := a.row()
		q := r.q
		if q < 0 && len(a.qs) == 1 {
			q = 0
		}
		if o := int(key[0] - '1'); q >= 0 && o < len(a.qs[q].Options) {
			a.choose(q, o)
			for i, row := range a.rows {
				if row == (answerRow{q, o}) {
					a.cursor = i
				}
			}
		}
	case " ", "x":
		if r := a.row(); r.q >= 0 && r.opt >= 0 {
			a.choose(r.q, r.opt)
		}
	case "enter":
		r := a.row()
		switch {
		case r.q < 0:
			return m, m.sendAnswer()
		case r.opt == rowText:
			a.move(1)
		case a.qs[r.q].Input == queue.InputMultiSelect:
			a.choose(r.q, r.opt)
		default:
			// A single_select's Enter picks and moves on, like the selector.
			if !a.picked[r.q][r.opt] {
				a.choose(r.q, r.opt)
			}
			a.nextQuestion(r.q)
		}
	}
	return m, nil
}

// sendAnswer sends the view's answer, or shows why it cannot yet.
func (m *model) sendAnswer() tea.Cmd {
	a := m.qp.answer
	fields, err := a.fields()
	if err != nil {
		a.err = err.Error()
		return nil
	}
	m.qp.answer = nil
	return m.queueAnswer(a.entry, fields)
}

// answerHead is the view's fixed title row: a label, cut to the width
// (the question below it never is).
func (m *model) answerHead(w int) []string {
	a := m.qp.answer
	e := a.entry
	who := e.Session
	if e.SessionTitle != "" && e.SessionTitle != e.Session {
		who = e.SessionTitle
	}
	return []string{fitLine(titleStyle.Render(" Answer")+dimStyle.Render(" · "+e.Kind+" · "+truncate(flat(who), 40)+" · "+e.ID), w)}
}

// answerLayout is the view's body laid out at one width: every line, each
// focusable row's [start, end) span, and lead, where each row's context
// starts — a question's first control leads from its question text, so
// focusing it brings the question into view when both fit.
type answerLayout struct {
	body  []string
	spans [][2]int
	lead  []int
}

// view is the span the window keeps in view for row i: from its lead when
// that fits in avail rows, else the control alone.
func (l answerLayout) view(i, avail int) (int, int) {
	s := l.spans[i]
	if s[1]-l.lead[i] <= avail {
		return l.lead[i], s[1]
	}
	return s[0], s[1]
}

// top is the first body row shown for the desired scroll.
func (l answerLayout) top(scroll, cursor, avail int) int {
	s, e := l.view(cursor, avail)
	return scrollWindow(scroll, len(l.body), avail, s, e)
}

// answerBody lays the view out at width w.
func (a *answerState) answerBody(w int) answerLayout {
	var body []string
	spans := make([][2]int, len(a.rows))
	lead := make([]int, len(a.rows))
	rowIdx := map[answerRow]int{}
	for i, r := range a.rows {
		rowIdx[r] = i
	}
	qStart, first := 0, false
	mark := func(r answerRow, lines []string) {
		i := rowIdx[r]
		spans[i] = [2]int{len(body), len(body) + len(lines)}
		lead[i] = len(body)
		if first {
			lead[i], first = qStart, false
		}
		body = append(body, lines...)
	}
	e := a.entry
	if !a.structured() && e.Body != "" && e.Title != "" && e.Body != e.Title {
		body = append(body, hang(" ", e.Title, w, func(s string) string { return titleStyle.Render(s) }, true)...)
	}
	for qi, q := range a.qs {
		qStart, first = len(body), true
		if qi == 0 {
			qStart = 0 // the title above the first question, too
		}
		body = append(body, "")
		body = append(body, hang(" ", headerChip(q.Header)+cleanText(q.Question), w, nil, false)...)
		hint := map[string]string{
			queue.InputSingleSelect: "choose one",
			queue.InputMultiSelect:  "choose any",
			queue.InputText:         "type an answer",
		}[q.Input]
		if q.AllowOther {
			hint += ", or type your own"
		}
		body = append(body, dimStyle.Render("   "+hint))
		for oi, o := range q.Options {
			on := a.picked[qi][oi]
			glyph := "( )"
			if q.Input == queue.InputMultiSelect {
				glyph = "[ ]"
				if on {
					glyph = "[✔]"
				}
			} else if on {
				glyph = "(•)"
			}
			r := answerRow{qi, oi}
			focused := a.rows[a.cursor] == r
			lines := optionLines(questionOption{Number: oi + 1, Mark: glyph, Label: o.Label, Description: o.Description, Preview: o.Preview}, 4, w, focused)
			if focused {
				lines[0] = keyStyle.Render(" ▸") + strings.TrimPrefix(lines[0], "  ")
			}
			mark(r, lines)
		}
		if q.Input == queue.InputText || q.AllowOther {
			r := answerRow{qi, rowText}
			focused := a.rows[a.cursor] == r
			label := "    Other: "
			if q.Input == queue.InputText {
				label = "    Answer: "
			}
			text := a.other[qi]
			if focused {
				label = keyStyle.Render(" ▸") + label[2:]
				text += "▏"
			} else if text == "" {
				text = dimStyle.Render("…")
			}
			var lines []string
			if q.Input == queue.InputText {
				lines = hang(label, text, w, nil, false)
			} else {
				lines = hang(label, text, w, nil, true)
			}
			mark(r, lines)
		}
	}
	if !a.structured() && e.Suggest != "" {
		body = append(body, "")
		body = append(body, hang(dimStyle.Render(" suggested: "), e.Suggest, w, dimFn, true)...)
	}
	body = append(body, "")
	send := " Send answer "
	if a.rows[a.cursor].q < 0 {
		send = keyStyle.Render(" ▸") + selStyle.Render(send)
	} else {
		send = "  " + keyStyle.Render(send)
	}
	mark(answerRow{-1, 0}, []string{fitLine(send, w)})
	if a.err != "" {
		body = append(body, hang(" ", "⚠ "+a.err, w, func(s string) string { return attnTextStyle.Render(s) }, true)...)
	}
	return answerLayout{body: body, spans: spans, lead: lead}
}

func (m *model) answerAvail() int {
	return max(1, m.paneRows()-len(m.answerHead(m.mainWidth())))
}

// answerPage scrolls a page down (dir 1) or up (-1). The focus moves to the
// first (down) or last (up) control past it that is wholly in the new
// window, else one step, so it always advances and never leaves the screen.
func (m *model) answerPage(dir int) {
	a := m.qp.answer
	l := a.answerBody(m.mainWidth())
	avail := m.answerAvail()
	top := min(max(0, l.top(a.scroll, a.cursor, avail)+dir*max(1, avail-1)), max(0, len(l.body)-avail))
	pick := -1
	for i, s := range l.spans {
		if (i-a.cursor)*dir <= 0 || s[0] < top || s[1] > top+avail {
			continue
		}
		pick = i
		if dir > 0 {
			break
		}
	}
	if pick < 0 {
		a.move(dir)
		return
	}
	a.cursor, a.scroll = pick, top
}

// renderAnswer draws the answer view into the main pane.
func (m *model) renderAnswer() string {
	w, h := m.mainWidth(), m.paneRows()
	a := m.qp.answer
	head := m.answerHead(w)
	l := a.answerBody(w)
	avail := max(1, h-len(head))
	top := l.top(a.scroll, a.cursor, avail)
	a.scroll = top
	end := min(len(l.body), top+avail)
	head[0] = cellPad(head[0]+scrollMark(top, end-top, len(l.body)), w)
	lines := append(head, l.body[top:end]...)
	for i, l := range lines {
		lines[i] = ansi.Truncate(l, w, "")
	}
	return lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(strings.Join(lines, "\n"))
}

// answerHints is the help line while the view is open.
func (m *model) answerHints() []hint {
	a := m.qp.answer
	if a.onText() {
		hs := []hint{{"type", "answer"}, {upDownKeys() + "/Tab", "move"}}
		if a.qs[a.row().q].Input == queue.InputText {
			hs = append(hs, hint{"Ctrl+J", "newline"})
		}
		return append(hs, hint{"Ctrl+U", "clear"}, hint{"Ctrl+S", "send"}, hint{"Esc", "cancel"})
	}
	return []hint{{upDownKeys(), "move"}, {"Space", "toggle"}, {"1-9", "choose"}, {"Enter", "pick/send"}, {"PgUp/PgDn", "scroll"}, {"Ctrl+S", "send"}, {"Esc", "cancel"}}
}
