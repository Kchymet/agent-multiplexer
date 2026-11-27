package nativetui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"amux/internal/core"
	"amux/internal/items"
	"amux/internal/queue"
)

// The question queue panel (i on the rail): every pending question for the
// user — permission requests, AskUserQuestion prompts, coordinators' goal
// asks, decision items in the overseer's inbox — plus the global inbox counts
// and the workgroup summary (internal/queue). It polls core.QueryQueue on the
// dashboard's stream like the resources panel; the counts also lead the rail
// footer. Answers go back as core.ActionQueueAnswer, which the daemon re-checks
// against fresh state before delivering to the asking session.

const (
	queuePollInterval = 5 * time.Second
	queueBackoff      = 30 * time.Second // the daemon predates the queue
)

type queueTickMsg struct{ serial uint64 }

// queuePanel is the TUI's cached queue view.
type queuePanel struct {
	serial      uint64
	ov          *queue.Overview
	unavailable bool
	reason      string

	open bool
	sel  int
	// scroll is the desired first body row; rendering clamps it so the
	// selected entry stays in view at the current width.
	scroll int
}

func (q *queuePanel) interval() time.Duration {
	if q.unavailable && q.ov == nil {
		return queueBackoff
	}
	return queuePollInterval
}

// startQueue begins a fresh poll chain on a new connection.
func (m *model) startQueue() tea.Cmd {
	m.qp.serial++
	return m.pollQueue()
}

// pollQueue sends one queue query and schedules the next.
func (m *model) pollQueue() tea.Cmd {
	c, serial := m.client, m.qp.serial
	if c == nil {
		return nil
	}
	send := func() tea.Msg {
		_ = c.Send(core.Action{Action: core.ActionQuery, Query: core.QueryQueue})
		return nil
	}
	tick := tea.Tick(m.qp.interval(), func(time.Time) tea.Msg { return queueTickMsg{serial} })
	return tea.Batch(send, tick)
}

// refreshQueue asks for the queue now (after an answer), outside the chain.
func (m *model) refreshQueue() tea.Cmd {
	c := m.client
	if c == nil {
		return nil
	}
	return func() tea.Msg {
		_ = c.Send(core.Action{Action: core.ActionQuery, Query: core.QueryQueue})
		return nil
	}
}

// applyQueue folds a QueryQueue reply into the panel.
func (m *model) applyQueue(d *core.Data) {
	q := &m.qp
	var ov queue.Overview
	switch {
	case !d.OK:
		q.unavailable, q.reason = true, d.Error
		return
	case json.Unmarshal(d.Rows, &ov) != nil:
		q.unavailable, q.reason = true, "unreadable queue"
		return
	}
	q.unavailable, q.reason = false, ""
	q.ov = &ov
	if q.sel >= len(ov.Entries) {
		q.sel = max(0, len(ov.Entries)-1)
	}
}

func (m *model) toggleQueue() tea.Cmd {
	m.qp.open = !m.qp.open
	if m.qp.open {
		return m.refreshQueue()
	}
	return nil
}

// selectedEntry is the highlighted entry, if any.
func (q *queuePanel) selectedEntry() (queue.Entry, bool) {
	if q.ov == nil || q.sel < 0 || q.sel >= len(q.ov.Entries) {
		return queue.Entry{}, false
	}
	return q.ov.Entries[q.sel], true
}

// queueAnswer sends one answer and re-reads the queue.
func (m *model) queueAnswer(e queue.Entry, fields map[string]string) tea.Cmd {
	fields[core.QueueEntryField] = e.ID
	m.status = "answering " + e.ID + "…"
	return tea.Batch(m.sendCmd(core.Action{Action: core.ActionQueueAnswer, Fields: fields}), m.refreshQueue())
}

func (m *model) handleQueueKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	q := &m.qp
	n := 0
	if q.ov != nil {
		n = len(q.ov.Entries)
	}
	e, has := q.selectedEntry()
	switch key := k.String(); key {
	case "i", "esc", "q", "ctrl+c":
		q.open = false
	case "up", "k":
		m.queueSelect(max(0, q.sel-1))
	case "down", "j":
		m.queueSelect(min(max(0, n-1), q.sel+1))
	case "home", "g":
		m.queueSelect(0)
	case "end", "G":
		m.queueSelect(max(0, n-1))
	case "pgdown", "ctrl+f":
		m.queuePage(1)
	case "pgup", "ctrl+b":
		m.queuePage(-1)
	case "y":
		if has && e.Kind == queue.KindPermission {
			return m, m.queueAnswer(e, map[string]string{core.SteerDecision: core.SteerAllow})
		}
	case "n":
		if has && e.Kind == queue.KindPermission {
			return m, m.queueAnswer(e, map[string]string{core.SteerDecision: core.SteerDeny})
		}
	case "1", "2", "3", "4", "5", "6", "7", "8", "9":
		// A lone single-choice question answers on one key, like the phone's card.
		if has && e.Kind == queue.KindQuestion && len(e.Questions) == 1 && !e.Questions[0].MultiSelect {
			i := int(key[0] - '1')
			if opts := e.Questions[0].Options; i < len(opts) {
				return m, m.queueAnswer(e, map[string]string{core.QueueChoiceField: opts[i].Label})
			}
		}
	case "r", "enter":
		if has && e.Kind != queue.KindPermission {
			m.openQueueReplyForm(e)
		}
	case "d":
		if has {
			m.status = "dismissing " + e.ID + "…"
			return m, tea.Batch(m.sendCmd(core.Action{Action: core.ActionQueueDismiss, Fields: map[string]string{core.QueueEntryField: e.ID}}), m.refreshQueue())
		}
	case "a":
		if has && e.Kind == queue.KindDecision {
			m.status = "acknowledging item " + e.Item + "…"
			return m, tea.Batch(m.sendCmd(core.Action{Action: core.ActionItemState, ID: items.GlobalInbox(), Fields: map[string]string{
				core.ItemIDField: e.Item, core.ItemStateField: items.VerbAck,
			}}), m.refreshQueue())
		}
	}
	return m, nil
}

// openQueueReplyForm opens a one-field form that answers an entry with text.
func (m *model) openQueueReplyForm(e queue.Entry) {
	m.form = &formState{
		title:  "Answer · " + truncate(e.Title, 60),
		action: core.ActionQueueAnswer,
		submit: "Send answer",
		extra:  map[string]string{core.QueueEntryField: e.ID},
		fields: []*formField{{key: core.QueueTextField, label: "Answer"}},
	}
	m.form.context = queueReplyContext(e)
}

// queueReplyContext is the text a reply answers: each question (with its
// header chip and numbered options), else the entry's body or title.
func queueReplyContext(e queue.Entry) string {
	if e.Kind != queue.KindQuestion || len(e.Questions) == 0 {
		return firstNonEmptyText(e.Body, e.Title)
	}
	var b strings.Builder
	for _, q := range e.Questions {
		b.WriteString(headerChip(q.Header) + cleanText(q.Question) + "\n")
		for i, o := range q.Options {
			fmt.Fprintf(&b, "%d. %s\n", i+1, flat(o.Label))
		}
	}
	return b.String()
}

func firstNonEmptyText(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// queueSummaryLine is the rail footer's queue line ("" when there is nothing
// to say or the daemon cannot answer).
func (m *model) queueSummaryLine() string {
	ov := m.qp.ov
	if ov == nil || (ov.Counts.Total == 0 && ov.Inbox.Open == 0) {
		return ""
	}
	parts := []string{}
	if ov.Counts.Total > 0 {
		parts = append(parts, attnTextStyle.Render(fmt.Sprintf("%d %s", ov.Counts.Total, pluralWord(ov.Counts.Total, "question", "questions"))))
	}
	if ov.Inbox.Open > 0 {
		parts = append(parts, dimStyle.Render(fmt.Sprintf("%d inbox", ov.Inbox.Open)))
	}
	return " " + strings.Join(parts, dimStyle.Render(" · ")) + dimStyle.Render(" · ") + keyStyle.Render("i")
}

func pluralWord(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// renderQueue draws the panel into the main pane.
func (m *model) renderQueue() string {
	w, h := m.mainWidth(), m.paneRows()
	lines := m.queueLines(w, h)
	return lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(strings.Join(lines, "\n"))
}

// queueLayout is the panel laid out at one width: the fixed header rows, the
// scrolling body, each entry's [start, end) span in the body, and the body
// rows available. It is rebuilt on every render and key, never cached, so a
// resize re-wraps everything.
type queueLayout struct {
	head  []string
	body  []string
	spans [][2]int
	avail int
}

// selSpan is the selected entry's body span ({0,0} when there is none).
func (l queueLayout) selSpan(sel int) (int, int) {
	if sel < 0 || sel >= len(l.spans) {
		return 0, 0
	}
	return l.spans[sel][0], l.spans[sel][1]
}

// top is the first body row shown for the desired scroll offset: the
// selected entry always stays in view (see scrollWindow).
func (l queueLayout) top(scroll, sel int) int {
	s, e := l.selSpan(sel)
	return scrollWindow(scroll, len(l.body), l.avail, s, e)
}

func (m *model) queueLayout(w, h int) queueLayout {
	q := &m.qp
	var l queueLayout
	l.head = []string{titleStyle.Render(" Questions for you") + dimStyle.Render(" · ") + keyStyle.Render("i") + dimStyle.Render(" close")}
	if q.ov == nil {
		msg := "loading the question queue…"
		if q.unavailable {
			msg = "the question queue is unavailable"
			if q.reason != "" {
				msg += ": " + q.reason
			}
		}
		l.head = append(l.head, "")
		l.head = append(l.head, hang(" ", msg, w, dimFn, true)...)
		return l
	}
	s := q.ov.Summary
	l.head = append(l.head, hang(" ", fmt.Sprintf("workgroups: %d need you · %d running · %d review/merge · %d done   global inbox: %d open · %d acknowledged",
		s.NeedsYou, s.Running, s.Review, s.Done, q.ov.Inbox.Open, q.ov.Inbox.Acknowledged), w, dimFn, true)...)
	if len(q.ov.Entries) == 0 {
		l.head = append(l.head, "", dimStyle.Render(" nothing is waiting on you"))
		return l
	}
	l.head = append(l.head, "")
	for i, e := range q.ov.Entries {
		start := len(l.body)
		l.body = append(l.body, queueEntryLines(e, i == q.sel, w)...)
		l.spans = append(l.spans, [2]int{start, len(l.body)})
	}
	l.avail = max(1, h-len(l.head))
	return l
}

func (m *model) queueLines(w, h int) []string {
	l := m.queueLayout(w, h)
	if len(l.body) == 0 {
		return l.head
	}
	top := l.top(m.qp.scroll, m.qp.sel)
	end := min(len(l.body), top+l.avail)
	l.head[0] = cellPad(l.head[0]+scrollMark(top, end-top, len(l.body)), w)
	return append(l.head, l.body[top:end]...)
}

// queueSelect moves the selection to entry i and scrolls so its first rows
// (all of it, when it fits) are in view.
func (m *model) queueSelect(i int) {
	q := &m.qp
	q.sel = i
	l := m.queueLayout(m.mainWidth(), m.paneRows())
	s, e := l.selSpan(q.sel)
	q.scroll = scrollWindow(l.top(q.scroll, q.sel), len(l.body), l.avail, s, min(e, s+l.avail))
}

// queuePage scrolls a page down (dir 1) or up (-1). Paging first walks
// through a selected entry taller than the panel; past its end the window
// moves on and the selection follows it to an entry in view, so the selected
// entry never leaves the screen.
func (m *model) queuePage(dir int) {
	q := &m.qp
	l := m.queueLayout(m.mainWidth(), m.paneRows())
	if len(l.body) == 0 {
		return
	}
	page := max(1, l.avail-1)
	top := l.top(q.scroll, q.sel)
	s, e := l.selSpan(q.sel)
	if e-s > l.avail { // a tall selection: page within it first
		if dir > 0 && top < e-l.avail {
			q.scroll = min(top+page, e-l.avail)
			return
		}
		if dir < 0 && top > s {
			q.scroll = max(top-page, s)
			return
		}
	}
	next := min(max(0, top+dir*page), max(0, len(l.body)-l.avail))
	if next == top { // already at the edge: step the selection instead
		m.queueSelect(min(max(0, q.sel+dir), len(l.spans)-1))
		return
	}
	// Select an entry wholly in the new window (the first going down, the
	// last going up), else the one at the window's leading edge.
	pick := -1
	for i, sp := range l.spans {
		if sp[0] >= next && sp[1] <= next+l.avail {
			pick = i
			if dir > 0 {
				break
			}
		}
	}
	if pick < 0 {
		edge := next
		if dir < 0 {
			edge = next + l.avail - 1
		}
		for i, sp := range l.spans {
			if sp[0] <= edge && edge < sp[1] {
				pick = i
				break
			}
		}
	}
	if pick >= 0 {
		q.sel = pick
	}
	q.scroll = next
}

// queueEntryLines renders one entry. Agent text is untrusted and drawn as
// plain text. An unselected entry is two lines, its title cut with an
// ellipsis; the selected entry wraps everything (full question text, header
// chips, options with descriptions) to the width, so the full text is always
// one keypress away.
func queueEntryLines(e queue.Entry, selected bool, w int) []string {
	who := e.Session
	if e.SessionTitle != "" && e.SessionTitle != e.Session {
		who = e.SessionTitle
	}
	if e.Workgroup != "" && e.Workgroup != e.Session {
		who = e.Workgroup + "/" + who
	}
	if !selected {
		head := "  " + attnTextStyle.Render(e.Kind) + dimStyle.Render(" · "+truncate(flat(who), 40)+" · "+e.ID)
		return []string{truncate(head, w), "    " + truncate(flat(e.Title), w-4), ""}
	}
	dim := dimFn
	out := hang(keyStyle.Render("▸ ")+attnTextStyle.Render(e.Kind)+dim(" · "), flat(who)+" · "+e.ID, w, dim, true)
	if e.Kind != queue.KindQuestion || len(e.Questions) == 0 {
		out = append(out, hang("    ", e.Title, w, nil, true)...)
	}
	keys := func(s string) []string { return []string{fitLine("    "+s, w)} }
	switch e.Kind {
	case queue.KindPermission:
		if e.Body != "" {
			out = append(out, hang("    ", e.Body, w, dim, false)...)
		}
		out = append(out, keys(keyStyle.Render("y")+dim(" allow  ")+keyStyle.Render("n")+dim(" deny  ")+keyStyle.Render("d")+dim(" hide"))...)
	case queue.KindQuestion:
		for _, qq := range e.Questions {
			out = append(out, hang("    ", headerChip(qq.Header)+cleanText(qq.Question), w, nil, false)...)
			for oi, o := range qq.Options {
				out = append(out, optionLines(questionOption{Number: oi + 1, Label: o.Label, Description: o.Description}, 6, w, false)...)
			}
		}
		k := keyStyle.Render("r") + dim(" reply  ") + keyStyle.Render("d") + dim(" hide")
		if len(e.Questions) == 1 && !e.Questions[0].MultiSelect && len(e.Questions[0].Options) > 0 {
			k = keyStyle.Render("1-9") + dim(" choose  ") + k
		}
		out = append(out, keys(k)...)
	case queue.KindGoalInput:
		if e.Body != "" && e.Body != e.Title {
			out = append(out, hang("    ", e.Body, w, dim, false)...)
		}
		out = append(out, keys(keyStyle.Render("r")+dim(" reply  ")+keyStyle.Render("d")+dim(" hide"))...)
	case queue.KindDecision:
		if e.Body != "" {
			out = append(out, hang("    ", e.Body, w, dim, false)...)
		}
		if e.Suggest != "" {
			out = append(out, hang(dim("    suggested: "), e.Suggest, w, dim, true)...)
		}
		out = append(out, keys(keyStyle.Render("r")+dim(" answer  ")+keyStyle.Render("a")+dim(" acknowledge  ")+keyStyle.Render("d")+dim(" dismiss"))...)
	}
	return append(out, "")
}

// flat flattens agent text to one display line with no control characters.
func flat(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f:
			return -1
		}
		return r
	}, s))
}
