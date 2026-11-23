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
		q.sel = max(0, q.sel-1)
	case "down", "j":
		q.sel = min(max(0, n-1), q.sel+1)
	case "home", "g":
		q.sel = 0
	case "end", "G":
		q.sel = max(0, n-1)
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

func (m *model) queueLines(w, h int) []string {
	q := &m.qp
	title := titleStyle.Render(" Questions for you") + dimStyle.Render(" · ") + keyStyle.Render("i") + dimStyle.Render(" close")
	lines := []string{cellPad(title, w)}
	if q.ov == nil {
		msg := "loading the question queue…"
		if q.unavailable {
			msg = "the question queue is unavailable"
			if q.reason != "" {
				msg += ": " + q.reason
			}
		}
		return append(lines, "", dimStyle.Render(" "+truncate(msg, w-1)))
	}
	s := q.ov.Summary
	lines = append(lines, dimStyle.Render(truncate(fmt.Sprintf(" workgroups: %d need you · %d running · %d review/merge · %d done   global inbox: %d open · %d acknowledged",
		s.NeedsYou, s.Running, s.Review, s.Done, q.ov.Inbox.Open, q.ov.Inbox.Acknowledged), w)))
	if len(q.ov.Entries) == 0 {
		return append(lines, "", dimStyle.Render(" nothing is waiting on you"))
	}
	lines = append(lines, "")
	var body []string
	selStart, selEnd := 0, 0
	for i, e := range q.ov.Entries {
		if i == q.sel {
			selStart = len(body)
		}
		body = append(body, queueEntryLines(e, i == q.sel, w)...)
		if i == q.sel {
			selEnd = len(body)
		}
	}
	avail := max(1, h-len(lines))
	start := 0
	if selEnd > avail {
		start = min(selStart, selEnd-avail)
	}
	end := min(len(body), start+avail)
	return append(lines, body[start:end]...)
}

// queueEntryLines renders one entry; agent text is flattened to plain lines.
func queueEntryLines(e queue.Entry, selected bool, w int) []string {
	cursor := "  "
	if selected {
		cursor = keyStyle.Render("▸ ")
	}
	who := e.Session
	if e.SessionTitle != "" && e.SessionTitle != e.Session {
		who = e.SessionTitle
	}
	if e.Workgroup != "" && e.Workgroup != e.Session {
		who = e.Workgroup + "/" + who
	}
	head := cursor + attnTextStyle.Render(e.Kind) + dimStyle.Render(" · "+truncate(flat(who), 40)+" · "+e.ID)
	out := []string{truncate(head, w), "    " + truncate(flat(e.Title), w-4)}
	if !selected {
		return out
	}
	switch e.Kind {
	case queue.KindPermission:
		if e.Body != "" {
			out = append(out, dimStyle.Render("    "+truncate(flat(e.Body), w-4)))
		}
		out = append(out, "    "+keyStyle.Render("y")+dimStyle.Render(" allow  ")+keyStyle.Render("n")+dimStyle.Render(" deny  ")+keyStyle.Render("d")+dimStyle.Render(" hide"))
	case queue.KindQuestion:
		for qi, qq := range e.Questions {
			if qi > 0 || len(e.Questions) > 1 {
				out = append(out, "    "+truncate(flat(qq.Question), w-4))
			}
			for oi, o := range qq.Options {
				label := fmt.Sprintf("      %d %s", oi+1, flat(o.Label))
				if o.Description != "" {
					label += dimStyle.Render("  " + flat(o.Description))
				}
				out = append(out, truncate(label, w))
			}
		}
		keys := keyStyle.Render("r") + dimStyle.Render(" reply  ") + keyStyle.Render("d") + dimStyle.Render(" hide")
		if len(e.Questions) == 1 && !e.Questions[0].MultiSelect && len(e.Questions[0].Options) > 0 {
			keys = keyStyle.Render("1-9") + dimStyle.Render(" choose  ") + keys
		}
		out = append(out, "    "+keys)
	case queue.KindGoalInput:
		if e.Body != "" && e.Body != e.Title {
			out = append(out, dimStyle.Render("    "+truncate(flat(e.Body), w-4)))
		}
		out = append(out, "    "+keyStyle.Render("r")+dimStyle.Render(" reply  ")+keyStyle.Render("d")+dimStyle.Render(" hide"))
	case queue.KindDecision:
		if e.Body != "" {
			out = append(out, dimStyle.Render("    "+truncate(flat(e.Body), w-4)))
		}
		if e.Suggest != "" {
			out = append(out, dimStyle.Render("    suggested: "+truncate(flat(e.Suggest), w-16)))
		}
		out = append(out, "    "+keyStyle.Render("r")+dimStyle.Render(" answer  ")+keyStyle.Render("a")+dimStyle.Render(" acknowledge  ")+keyStyle.Render("d")+dimStyle.Render(" dismiss"))
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
