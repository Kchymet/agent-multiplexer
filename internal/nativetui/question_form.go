package nativetui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"amux/internal/core"
	"amux/internal/queue"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// The question form view (docs/question-prompts.md §5.2): a v2 question
// answered in the TUI with everything the harness's own dialog offers — one
// tab per question plus a review tab, options picked with the arrows, digits
// or Space (multi-select toggles), "Other" free text (masked when secret), a
// note per question, dismissal with or without a reply, and "answer in the
// pane" for a held Claude question. Text is wrapped with the shared helpers
// (wrap.go) on every render, so a resize re-wraps.

// formEdit is what the form is typing into, if anything.
type formEdit int

const (
	editNone formEdit = iota
	editOther
	editNote
	editReply
)

// questionForm is an open question form.
type questionForm struct {
	entry queue.Entry
	tab   int // question index; len(questions) is the review tab
	// Per question: cursor row (options, then Other), selections, free text,
	// and note.
	cursor   []int
	selected []map[int]bool
	other    []string
	hasOther []bool
	note     []string
	edit     formEdit
	buf      string
	reply    string
	err      string
}

func newQuestionForm(e queue.Entry) *questionForm {
	n := len(e.Form.Questions)
	f := &questionForm{entry: e, cursor: make([]int, n), selected: make([]map[int]bool, n),
		other: make([]string, n), hasOther: make([]bool, n), note: make([]string, n)}
	for i := range f.selected {
		f.selected[i] = map[int]bool{}
	}
	return f
}

func (f *questionForm) form() harnessproto.Form { return *f.entry.Form }

func (f *questionForm) question() (harnessproto.FormQuestion, bool) {
	qs := f.form().Questions
	if f.tab < 0 || f.tab >= len(qs) {
		return harnessproto.FormQuestion{}, false
	}
	return qs[f.tab], true
}

// rows is the question's selectable rows: its options, then Other.
func rows(q harnessproto.FormQuestion) int {
	n := len(q.Options)
	if q.Other {
		n++
	}
	return n
}

// oneTap is a form one key answers: one single-select question with options
// and no note to give, as in Claude's own dialog.
func (f *questionForm) oneTap() bool {
	qs := f.form().Questions
	return len(qs) == 1 && !qs[0].MultiSelect && len(qs[0].Options) > 0 && !qs[0].Notes
}

// answer builds the FormAnswer from the form's state.
func (f *questionForm) answer() harnessproto.FormAnswer {
	a := harnessproto.FormAnswer{Version: harnessproto.FormVersion, Action: harnessproto.FormActionAnswer}
	for i, q := range f.form().Questions {
		qa := harnessproto.QuestionAnswer{ID: q.ID, Note: strings.TrimSpace(f.note[i])}
		for oi := range q.Options {
			if f.selected[i][oi] {
				qa.Selected = append(qa.Selected, oi)
			}
		}
		if f.hasOther[i] && strings.TrimSpace(f.other[i]) != "" {
			o := f.other[i]
			qa.Other = &o
		}
		a.Answers = append(a.Answers, qa)
	}
	return a
}

// answered reports whether question i has an answer.
func (f *questionForm) answered(i int) bool {
	return len(f.selected[i]) > 0 || (f.hasOther[i] && strings.TrimSpace(f.other[i]) != "")
}

// pick selects option oi of the current question (or toggles it on a
// multi-select); a single-select pick clears Other and moves on.
func (f *questionForm) pick(oi int) (submit bool) {
	q, ok := f.question()
	if !ok || oi < 0 || oi >= len(q.Options) {
		return false
	}
	i := f.tab
	f.cursor[i] = oi
	if q.MultiSelect {
		if f.selected[i][oi] {
			delete(f.selected[i], oi)
		} else {
			f.selected[i][oi] = true
		}
		return false
	}
	f.selected[i] = map[int]bool{oi: true}
	f.hasOther[i] = false
	if f.oneTap() {
		return true
	}
	f.tab++
	return false
}

// openQuestionForm opens the form for a v2 question entry.
func (m *model) openQuestionForm(e queue.Entry) {
	m.qf = newQuestionForm(e)
}

// sendForm sends a form answer and closes the form.
func (m *model) sendForm(a harnessproto.FormAnswer) tea.Cmd {
	f := m.qf
	b, err := json.Marshal(a)
	if err != nil {
		f.err = err.Error()
		return nil
	}
	if err := harnessproto.ValidateAnswer(f.form(), a); err != nil {
		f.err = strings.TrimPrefix(err.Error(), harnessproto.ErrInvalidAnswer.Error()+": ")
		return nil
	}
	m.qf = nil
	return m.queueAnswer(f.entry, map[string]string{core.QueueFormField: string(b)})
}

// handleQuestionFormKey drives the open form.
func (m *model) handleQuestionFormKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	f := m.qf
	key := k.String()
	if f.edit != editNone {
		switch key {
		case "esc":
			f.edit, f.buf = editNone, ""
		case "enter":
			switch f.edit {
			case editOther:
				f.other[f.tab], f.hasOther[f.tab] = f.buf, strings.TrimSpace(f.buf) != ""
				if q, ok := f.question(); ok && !q.MultiSelect && f.hasOther[f.tab] {
					f.selected[f.tab] = map[int]bool{}
				}
			case editNote:
				f.note[f.tab] = f.buf
			case editReply:
				f.edit = editNone
				return m, m.sendForm(harnessproto.FormAnswer{Version: harnessproto.FormVersion, Action: harnessproto.FormActionCancel, Message: f.buf})
			}
			f.edit, f.buf = editNone, ""
		case "backspace":
			if f.buf != "" {
				_, size := utf8.DecodeLastRuneInString(f.buf)
				f.buf = f.buf[:len(f.buf)-size]
			}
		default:
			if k.Type == tea.KeyRunes || k.Type == tea.KeySpace {
				f.buf += string(k.Runes)
				if k.Type == tea.KeySpace {
					f.buf += " "
				}
			}
		}
		return m, nil
	}
	form := f.form()
	nq := len(form.Questions)
	q, onQuestion := f.question()
	f.err = ""
	switch key {
	case "esc", "q", "ctrl+c":
		m.qf = nil
	case "left", "shift+tab", "h":
		f.tab = max(0, f.tab-1)
	case "right", "tab", "l":
		f.tab = min(nq, f.tab+1)
	case "up", "k":
		if onQuestion {
			f.cursor[f.tab] = max(0, f.cursor[f.tab]-1)
		}
	case "down", "j":
		if onQuestion {
			f.cursor[f.tab] = min(max(0, rows(q)-1), f.cursor[f.tab]+1)
		}
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		if onQuestion && form.Delivery != harnessproto.DeliveryPane {
			if f.pick(int(key[0] - '1')) {
				return m, m.sendForm(f.answer())
			}
		}
	case " ", "space", "enter":
		if form.Delivery == harnessproto.DeliveryPane {
			break
		}
		if !onQuestion {
			if key == "enter" {
				return m, m.sendForm(f.answer())
			}
			break
		}
		if c := f.cursor[f.tab]; c < len(q.Options) {
			if f.pick(c) {
				return m, m.sendForm(f.answer())
			}
		} else if q.Other {
			f.edit, f.buf = editOther, f.other[f.tab]
		}
	case "o":
		if onQuestion && q.Other && form.Delivery != harnessproto.DeliveryPane {
			f.cursor[f.tab] = len(q.Options)
			f.edit, f.buf = editOther, f.other[f.tab]
		}
	case "n":
		if onQuestion && q.Notes && form.Delivery != harnessproto.DeliveryPane {
			f.edit, f.buf = editNote, f.note[f.tab]
		}
	case "x":
		if form.Cancel && form.Delivery != harnessproto.DeliveryPane {
			return m, m.sendForm(harnessproto.FormAnswer{Version: harnessproto.FormVersion, Action: harnessproto.FormActionCancel})
		}
	case "m":
		if form.Reply && form.Delivery != harnessproto.DeliveryPane {
			f.edit, f.buf = editReply, ""
		}
	case "p":
		if form.Source == harnessproto.FormSourceClaudeAsk && form.Delivery == harnessproto.DeliveryStructured {
			m.qf = nil
			m.status = "handing " + f.entry.ID + " to the agent's pane…"
			return m, tea.Batch(m.sendCmd(core.Action{Action: core.ActionQueueRelease, Fields: map[string]string{core.QueueEntryField: f.entry.ID}}), m.refreshQueue())
		}
	}
	return m, nil
}

// questionFormLines renders the form at w×h.
func (m *model) questionFormLines(w, h int) []string {
	f := m.qf
	form := f.form()
	e := f.entry
	who := firstNonEmptyText(e.SessionTitle, e.Session)
	dim := dimFn
	out := hang(keyStyle.Render("Answer")+dim(" · "), flat(who)+" · "+e.ID, w, dim, true)
	out = append(out, hang("  ", formDeliveryWords(form), w, dim, true)...)
	if form.Claim != nil {
		out = append(out, hang("  ", "being answered by "+form.Claim.By, w, attnFn, true)...)
	}

	// Tab strip: one chip per question (✔ when answered), then Review.
	var tabs []string
	for i, q := range form.Questions {
		mark := " "
		if f.answered(i) {
			mark = "✔"
		}
		label := flat(q.Header)
		if label == "" {
			label = fmt.Sprintf("Q%d", i+1)
		}
		chip := "[" + mark + " " + label + "]"
		if i == f.tab {
			chip = selStyle.Render(chip)
		}
		tabs = append(tabs, chip)
	}
	review := "[Review]"
	if f.tab == len(form.Questions) {
		review = selStyle.Render(review)
	}
	tabs = append(tabs, review)
	for _, l := range wrapFlat(strings.Join(tabs, " "), w-2) {
		out = append(out, "  "+l)
	}
	out = append(out, "")

	if q, ok := f.question(); ok {
		out = append(out, m.questionBody(f, q, w)...)
	} else {
		out = append(out, f.reviewLines(w)...)
	}
	if f.err != "" {
		out = append(out, hang("  ", f.err, w, attnFn, false)...)
	}
	if f.edit != editNone {
		label := map[formEdit]string{editOther: "Other", editNote: "Note", editReply: "Reply"}[f.edit]
		text := f.buf
		if q, ok := f.question(); ok && f.edit == editOther && q.Secret {
			text = strings.Repeat("•", utf8.RuneCountInString(text))
		}
		out = append(out, hang("  "+keyStyle.Render(label+": "), text+"▏", w, nil, false)...)
	}
	return fitFrame(out, w, h, m.questionFormHints(w))
}

// questionBody is one question's text, options (with the cursor's preview
// beside the list at 100+ columns, under it otherwise), Other and note.
func (m *model) questionBody(f *questionForm, q harnessproto.FormQuestion, w int) []string {
	dim := dimFn
	i := f.tab
	out := hang("  ", headerChip(q.Header)+cleanText(q.Question), w, nil, false)
	listW := w
	preview := ""
	if c := f.cursor[i]; c < len(q.Options) {
		preview = q.Options[c].Preview
	}
	side := preview != "" && w >= 100
	if side {
		listW = w / 2
	}
	var list []string
	for oi, o := range q.Options {
		mark := "( )"
		if q.MultiSelect {
			mark = "[ ]"
		}
		if f.selected[i][oi] {
			mark = "(•)"
			if q.MultiSelect {
				mark = "[✔]"
			}
		}
		list = append(list, optionLines(questionOption{Number: oi + 1, Mark: mark, Label: o.Label, Description: o.Description}, 2, listW, oi == f.cursor[i])...)
	}
	if q.Other {
		text := f.other[i]
		if q.Secret {
			text = strings.Repeat("•", utf8.RuneCountInString(text))
		}
		label := "Other"
		if f.hasOther[i] && strings.TrimSpace(f.other[i]) != "" {
			label += ": " + text
		}
		list = append(list, optionLines(questionOption{Number: len(q.Options) + 1, Mark: "   ", Label: label}, 2, listW, f.cursor[i] == len(q.Options))...)
	}
	if side {
		pw := w - listW - 3
		pv := []string{dim("preview")}
		for _, l := range wrapText(cleanText(preview), pw) {
			pv = append(pv, l)
		}
		for r := 0; r < max(len(list), len(pv)); r++ {
			left, right := "", ""
			if r < len(list) {
				left = list[r]
			}
			if r < len(pv) {
				right = pv[r]
			}
			pad := max(0, listW-ansi.StringWidth(left))
			out = append(out, left+strings.Repeat(" ", pad)+dim(" │ ")+right)
		}
	} else {
		out = append(out, list...)
		if preview != "" {
			out = append(out, dim("    preview"))
			for _, l := range wrapText(cleanText(preview), w-6) {
				out = append(out, "    "+dim("│ ")+l)
			}
		}
	}
	if q.Notes && strings.TrimSpace(f.note[i]) != "" {
		out = append(out, hang("  "+dim("note: "), f.note[i], w, dim, false)...)
	}
	return out
}

// reviewLines lists each question's answer.
func (f *questionForm) reviewLines(w int) []string {
	var out []string
	for i, q := range f.form().Questions {
		var parts []string
		for oi, o := range q.Options {
			if f.selected[i][oi] {
				parts = append(parts, flat(o.Label))
			}
		}
		if f.hasOther[i] && strings.TrimSpace(f.other[i]) != "" {
			if q.Secret {
				parts = append(parts, "(secret text)")
			} else {
				parts = append(parts, flat(f.other[i]))
			}
		}
		ans := strings.Join(parts, ", ")
		if ans == "" {
			ans = attnTextStyle.Render("not answered")
		}
		out = append(out, hang("  "+headerChip(q.Header), cleanText(q.Question), w, nil, false)...)
		out = append(out, hang("    → ", ans, w, nil, false)...)
		if n := strings.TrimSpace(f.note[i]); n != "" {
			out = append(out, hang("      note: ", n, w, dimFn, false)...)
		}
	}
	return out
}

// questionFormHints is the form's key bar.
func (m *model) questionFormHints(w int) string {
	f := m.qf
	form := f.form()
	k := func(key, what string) string { return keyStyle.Render(key) + dimStyle.Render(" "+what) }
	var hs []string
	switch {
	case f.edit != editNone:
		hs = []string{k("Enter", "done"), k("Esc", "cancel")}
	case form.Delivery == harnessproto.DeliveryPane:
		hs = []string{k("Esc", "close")}
	default:
		q, onQuestion := f.question()
		hs = append(hs, k("←/→", "question"))
		if onQuestion {
			pick := "pick"
			if q.MultiSelect {
				pick = "toggle"
			}
			hs = append(hs, k("↑/↓ 1-9 Space", pick))
			if q.Other {
				hs = append(hs, k("o", "other"))
			}
			if q.Notes {
				hs = append(hs, k("n", "note"))
			}
		} else {
			hs = append(hs, k("Enter", "submit"))
		}
		if form.Cancel {
			hs = append(hs, k("x", "dismiss"))
		}
		if form.Reply {
			hs = append(hs, k("m", "reply instead"))
		}
		if form.Source == harnessproto.FormSourceClaudeAsk && form.Delivery == harnessproto.DeliveryStructured {
			hs = append(hs, k("p", "answer in pane"))
		}
		hs = append(hs, k("Esc", "back"))
	}
	return ansi.Truncate(strings.Join(hs, "  "), w, "")
}

// formDeliveryWords says how an answer will be delivered.
func formDeliveryWords(f harnessproto.Form) string {
	switch f.Delivery {
	case harnessproto.DeliveryStructured:
		s := "answers go straight to the agent"
		if f.Deadline > 0 {
			s += " (until " + time.UnixMilli(f.Deadline).Format(time.Kitchen) + ", then it opens in the pane)"
		}
		return s
	case harnessproto.DeliveryKeys:
		return "amux answers the dialog in the agent's pane"
	}
	return "answer this in the agent's pane"
}

// fitFrame keeps the body within h rows above the hint bar.
func fitFrame(lines []string, w, h int, hints string) []string {
	avail := max(1, h-1)
	if len(lines) > avail {
		lines = lines[:avail]
	}
	for len(lines) < avail {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = fitLine(lines[i], w)
	}
	return append(lines, hints)
}

// attnFn is attnTextStyle.Render as a one-line styler.
func attnFn(s string) string { return attnTextStyle.Render(s) }

// renderQuestionForm draws the form into the main pane.
func (m *model) renderQuestionForm() string {
	w, h := m.mainWidth(), m.paneRows()
	return lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(strings.Join(m.questionFormLines(w, h), "\n"))
}

// releaseFocusedQuestion hands a held Claude question back to its pane when
// the user focuses that agent's pane (docs/question-prompts.md §4.1): they
// are at the dialog, so Claude should draw it. Each entry is released once.
func (m *model) releaseFocusedQuestion() tea.Cmd {
	if m.focus != focusAgent || m.tab != tabAgent || m.attached == "" || m.qp.ov == nil {
		return nil
	}
	var cmds []tea.Cmd
	for _, e := range m.qp.ov.Entries {
		if e.Session != m.attached || e.Form == nil || e.Form.Source != harnessproto.FormSourceClaudeAsk ||
			e.Form.Delivery != harnessproto.DeliveryStructured || m.qp.released[e.ID] {
			continue
		}
		if m.qp.released == nil {
			m.qp.released = map[string]bool{}
		}
		m.qp.released[e.ID] = true
		cmds = append(cmds, m.sendCmd(core.Action{Action: core.ActionQueueRelease, Fields: map[string]string{core.QueueEntryField: e.ID}}))
	}
	if len(cmds) == 0 {
		return nil
	}
	return tea.Batch(append(cmds, m.refreshQueue())...)
}
