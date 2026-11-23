package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"amux/internal/access"
	"amux/internal/agent"
	"amux/internal/claudegoal"
	"amux/internal/core"
	"amux/internal/items"
	"amux/internal/itemsvc"
	"amux/internal/queue"
	"amux/internal/runtimeevents"
	"amux/internal/sessionrpc"
	"amux/internal/store"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// The user question queue (internal/queue, docs/direct-console.md §4). Live
// entries are derived here at read time from the same signals the rail and the
// phone already use — #327's waiting state and #316's open-question reader for
// AskUserQuestion, the runtime permission records, and a Claude coordinator's
// goal file for `amux agent goal ask` — plus decision_needed items in the
// global inbox. Only the host (the user) answers or dismisses an entry; the
// daemon re-derives the entry before delivering, so an answer never lands on a
// question that went away or was replaced.

// Queue result codes (core.Result.Code).
const (
	codeAskerGone = "asker_gone" // the question is no longer open
	codeAnswered  = "answered"   // someone already answered it
)

// queueAnsweredTTL is how long an answered entry keeps answering "answered"
// (rather than "asker_gone") to a second answer.
const queueAnsweredTTL = 10 * time.Minute

// maxQueueBodyRunes bounds an entry's body (question text, goal note).
const maxQueueBodyRunes = 4000

// queueState is the daemon's only queue memory: entries the user hid and
// entries recently answered. Both are keyed by deterministic entry id, so a new
// question (a new tool call, request or ask) is a new entry.
type queueState struct {
	mu        sync.Mutex
	dismissed map[string]time.Time
	answered  map[string]time.Time
}

func (q *queueState) isDismissed(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.dismissed[id]
	return ok
}

func (q *queueState) dismiss(id string, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.dismissed == nil {
		q.dismissed = map[string]time.Time{}
	}
	q.dismissed[id] = now
}

func (q *queueState) markAnswered(id string, now time.Time) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.answered == nil {
		q.answered = map[string]time.Time{}
	}
	q.answered[id] = now
	for k, at := range q.answered {
		if now.Sub(at) > queueAnsweredTTL {
			delete(q.answered, k)
		}
	}
}

func (q *queueState) wasAnswered(id string, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	at, ok := q.answered[id]
	return ok && now.Sub(at) <= queueAnsweredTTL
}

// prune forgets dismissals of entries no longer derived (the question was
// answered elsewhere or went away), so memory stays bounded by what is open.
func (q *queueState) prune(live map[string]bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for id := range q.dismissed {
		if !live[id] {
			delete(q.dismissed, id)
		}
	}
}

// isQueueVerb reports whether an action is a queue verb.
func isQueueVerb(verb string) bool {
	return verb == core.ActionQueueAnswer || verb == core.ActionQueueDismiss
}

// queueSources is what deriving the queue reads; tests replace it.
type queueSources struct {
	rows      func() []core.Session
	lookup    func(id string) (store.Session, bool, error)
	questions func(h agent.Harness, s store.Session) []openQuestion
	perms     func(id string, s store.Session) []queuePermission
	goal      func(s store.Session) (claudegoal.State, error)
	inbox     func() ([]items.Item, error)
}

// openQuestion is one unanswered AskUserQuestion call.
type openQuestion struct {
	ToolUseID string
	Questions []queue.Question
}

// queuePermission is one open permission request with its bound generation.
type queuePermission struct {
	RequestID, Generation, Tool, Action string
}

func (d *Daemon) queueSources() queueSources {
	if d.queueSourcesFn != nil {
		return d.queueSourcesFn()
	}
	return queueSources{
		rows:      func() []core.Session { return d.snapshot().Sessions },
		lookup:    lookupSession,
		questions: claudeOpenQuestions,
		perms:     d.openPermissionsFor,
		goal:      readHookGoal,
		inbox:     readGlobalInbox,
	}
}

// readGlobalInbox lists the global inbox owner's active items.
func readGlobalInbox() ([]items.Item, error) {
	db, err := store.OpenReadOnly()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	list, _, err := db.Items(store.ItemFilter{Recipient: items.GlobalInbox(), States: []string{items.StateOpen, items.StateAcknowledged}})
	return list, err
}

// openPermissionsFor lists a session's open permission requests, each bound to
// the current runtime generation (the binding the `permission` verb checks).
// A request that cannot be bound (no live runtime) is listed without one and
// cannot be answered until it can.
func (d *Daemon) openPermissionsFor(id string, s store.Session) []queuePermission {
	var out []queuePermission
	seen := map[string]bool{}
	add := func(p queuePermission) {
		if p.RequestID == "" || seen[p.RequestID] {
			return
		}
		seen[p.RequestID] = true
		if gen, err := d.bindPermissionRequest(id, p.RequestID); err == nil {
			p.Generation = gen
		}
		out = append(out, p)
	}
	if d.codex != nil {
		if sup, ok := d.codex.Get(id); ok {
			for _, rid := range sup.OpenApprovals() {
				add(queuePermission{RequestID: rid})
			}
		}
	}
	rec := permissionRecord(agent.HarnessFor(s.Agent), s)
	if rec.Path != "" {
		for _, p := range runtimeevents.OpenPermissions(rec) {
			add(queuePermission{RequestID: p.RequestID, Tool: p.Tool, Action: p.Action})
		}
	}
	return out
}

// claudeOpenQuestions returns a live Claude session's unanswered
// AskUserQuestion calls with their questions (the same tail reading as
// claudeQuestionOpen, #316).
func claudeOpenQuestions(h agent.Harness, sess store.Session) []openQuestion {
	if agent.Canonical(sess.Agent) != harnessproto.RuntimeClaude {
		return nil
	}
	switch h.RailState(sess) {
	case core.StateRunning, core.StateWaiting:
	default:
		return nil
	}
	path, ok := h.RuntimeTranscriptPath(sess)
	if !ok {
		return nil
	}
	return claudeOpenQuestionsIn(path)
}

// claudeOpenQuestionsIn reads a transcript tail's open AskUserQuestion calls.
func claudeOpenQuestionsIn(path string) []openQuestion {
	lines, ok := transcriptTail(path)
	if !ok {
		return nil
	}
	st := &runtimeevents.ClaudeState{}
	var open []openQuestion
	for _, line := range lines {
		for _, ev := range runtimeevents.MapClaudeLine([]byte(line), st) {
			switch ev.Type {
			case harnessproto.TypeToolCall:
				var p struct {
					Title    string          `json:"title"`
					RawInput json.RawMessage `json:"raw_input"`
				}
				if json.Unmarshal(ev.Payload, &p) != nil || p.Title != claudeQuestionTool || ev.ItemID == "" {
					continue
				}
				qs, ok := queue.ParseQuestions(p.RawInput)
				if !ok {
					continue
				}
				open = append(open, openQuestion{ToolUseID: ev.ItemID, Questions: qs})
			case harnessproto.TypeToolResult:
				for i := range open {
					if open[i].ToolUseID == ev.ItemID {
						open = append(open[:i], open[i+1:]...)
						break
					}
				}
			case harnessproto.TypeTurnStart:
				open = nil
			}
		}
	}
	return open
}

// deriveQueue builds every live entry and every decision_needed entry. A
// source that fails is reported in errs and the rest of the queue stands.
func (d *Daemon) deriveQueue(src queueSources) (entries []queue.Entry, inbox queue.InboxCounts, rows []core.Session, errs []string) {
	rows = src.rows()
	for _, row := range rows {
		if row.Archived || row.Section == core.SectionArchived || row.Kind == "repo" && row.Role == "" {
			continue
		}
		goalInput := row.Supervision != nil && row.Supervision.Goal == claudegoal.StatusInput
		structured := row.ControlMode == harnessproto.ControlModeStructured
		if row.State != core.StateWaiting && !goalInput && !structured {
			continue
		}
		sess, found, err := src.lookup(row.ID)
		if err != nil || !found || sess.Archived {
			continue
		}
		workgroup := row.RootID
		if row.IsRoot && row.Role == store.RoleCoordinator {
			workgroup = row.ID
		}
		base := queue.Entry{Session: row.ID, SessionTitle: row.Title, Workgroup: workgroup}
		h := agent.HarnessFor(sess.Agent)
		gen := ""
		perms := src.perms(row.ID, sess)
		for _, p := range perms {
			if p.Generation != "" {
				gen = p.Generation
			}
			e := base
			e.Kind = queue.KindPermission
			e.ID = queue.EntryID(queue.KindPermission, row.ID, p.Generation, p.RequestID)
			e.RequestID, e.RuntimeGeneration, e.Tool, e.Action = p.RequestID, p.Generation, p.Tool, p.Action
			e.Title = "permission: " + firstNonEmpty(p.Tool, "tool call")
			e.Body = boundRunes(p.Action, maxQueueBodyRunes)
			entries = append(entries, e)
		}
		asked := false
		if row.State == core.StateWaiting {
			for _, q := range src.questions(h, sess) {
				asked = true
				e := base
				e.Kind = queue.KindQuestion
				e.ID = queue.EntryID(queue.KindQuestion, row.ID, gen, q.ToolUseID)
				e.ToolUseID, e.Questions = q.ToolUseID, q.Questions
				e.Title = boundRunes(q.Questions[0].Question, 200)
				if len(q.Questions) > 1 {
					e.Title += fmt.Sprintf(" (+%d more)", len(q.Questions)-1)
				}
				entries = append(entries, e)
			}
		}
		// A coordinator wrapping up runs `goal ask` and then AskUserQuestion for
		// the same question (wrapUpAsk): the question, with its options, is the
		// one entry; answering it resumes the goal too.
		if goalInput && !asked && agent.HookGoals(sess) {
			st, err := src.goal(sess)
			if err != nil {
				errs = append(errs, fmt.Sprintf("goal %s: %v", row.ID, err))
			} else if st.Status == claudegoal.StatusInput {
				e := base
				e.Kind = queue.KindGoalInput
				e.ID = queue.EntryID(queue.KindGoalInput, row.ID, fmt.Sprint(st.Created), fmt.Sprint(st.Updated), st.Note)
				e.Title = boundRunes(firstNonEmpty(st.Note, "the coordinator needs your input"), 200)
				e.Body = boundRunes(st.Note, maxQueueBodyRunes)
				e.Created = st.Updated
				entries = append(entries, e)
			}
		}
	}
	list, err := src.inbox()
	if err != nil {
		errs = append(errs, fmt.Sprintf("global inbox: %v", err))
	}
	for _, it := range list {
		switch it.State {
		case items.StateOpen:
			inbox.Open++
		case items.StateAcknowledged:
			inbox.Acknowledged++
		}
		if it.Kind != items.KindDecisionNeeded {
			continue
		}
		entries = append(entries, queue.Entry{
			ID: queue.EntryID(queue.KindDecision, it.ID), Kind: queue.KindDecision,
			Session: it.Source, Workgroup: it.Workgroup, Item: it.ID,
			Title: it.Title, Body: boundRunes(it.Detail, maxQueueBodyRunes), Suggest: it.Suggest,
			PRs: it.PRs, Links: it.Links, Created: it.Created,
		})
	}
	return entries, inbox, rows, errs
}

// queueOverview is the QueryQueue read: entries (dismissed ones only when
// asked), counts, the global inbox counts and the workgroup summary.
func (d *Daemon) queueOverview(fields map[string]string) queue.Overview {
	entries, inbox, rows, errs := d.deriveQueue(d.queueSources())
	live := make(map[string]bool, len(entries))
	all := fields[core.QueueAllField] == "1"
	out := entries[:0]
	for _, e := range entries {
		live[e.ID] = true
		e.Dismissed = d.queue.isDismissed(e.ID)
		if e.Dismissed && !all {
			continue
		}
		out = append(out, e)
	}
	d.queue.prune(live)
	queue.Sort(out)
	return queue.Overview{
		Entries: out, Counts: queue.Count(out), Inbox: inbox,
		Summary: queue.Summarize(rows, out), Errors: errs,
	}
}

// findQueueEntry re-derives the queue and returns the entry with id.
func (d *Daemon) findQueueEntry(id string) (queue.Entry, bool) {
	entries, _, _, _ := d.deriveQueue(d.queueSources())
	for _, e := range entries {
		if e.ID == id {
			return e, true
		}
	}
	return queue.Entry{}, false
}

// handleQueueAction serves ActionQueueAnswer and ActionQueueDismiss on the
// host path (the user). Every effect is an existing verb the daemon already
// guards: a steer (prompt or permission) to the asking session, or an item
// state change recorded as the host's.
func (d *Daemon) handleQueueAction(ctx context.Context, a core.Action) core.Result {
	id := strings.TrimSpace(a.Fields[core.QueueEntryField])
	if id == "" {
		return fail("%s needs -f %s=<entry id> (amux queue list)", a.Action, core.QueueEntryField)
	}
	now := time.Now()
	e, found := d.findQueueEntry(id)
	if !found {
		if d.queue.wasAnswered(id, now) {
			return core.Result{Type: "result", Code: codeAnswered, Error: fmt.Sprintf("queue entry %s was already answered", id)}
		}
		return core.Result{Type: "result", Code: codeAskerGone, Error: fmt.Sprintf("queue entry %s is no longer open (the question was answered, replaced or its session went away); `amux queue list` shows what is open, and you can still prompt the session directly", id)}
	}
	if a.Action == core.ActionQueueDismiss {
		if e.Kind == queue.KindDecision {
			return d.queueItemState(ctx, e, items.VerbDismiss, "dismissed from the queue")
		}
		d.queue.dismiss(id, now)
		return ok()
	}
	res := d.answerQueueEntry(ctx, e, a.Fields)
	if res.OK {
		d.queue.markAnswered(id, now)
	}
	return res
}

// answerQueueEntry delivers one answer per the entry's kind.
func (d *Daemon) answerQueueEntry(ctx context.Context, e queue.Entry, f map[string]string) core.Result {
	text := strings.TrimSpace(f[core.QueueTextField])
	switch e.Kind {
	case queue.KindPermission:
		decision := f[core.SteerDecision]
		if decision != core.SteerAllow && decision != core.SteerDeny {
			return fail("a permission answer needs -f %s=allow|deny", core.SteerDecision)
		}
		if e.RuntimeGeneration == "" {
			return core.Result{Type: "result", Code: codeAskerGone, Error: fmt.Sprintf("permission request %s has no live runtime to answer", e.RequestID)}
		}
		return d.handle(ctx, core.Action{Action: core.ActionSteer, ID: e.Session, Fields: map[string]string{
			core.SteerVerb: core.SteerPermission, core.SteerDecision: decision,
			core.SteerRequestID: e.RequestID, access.RuntimeGenerationField: e.RuntimeGeneration,
		}})
	case queue.KindQuestion:
		choices := queue.ParseChoices(f[core.QueueChoiceField])
		if !queue.ValidChoices(e.Questions, choices) {
			return fail("choice must name the question's option labels, one line per question (| between labels of a multi-select)")
		}
		reply := queue.FormatAnswer(e.Questions, choices, text)
		if reply == "" {
			return fail("an answer needs -f %s=<label> or -f %s=<text>", core.QueueChoiceField, core.QueueTextField)
		}
		return d.queuePrompt(ctx, e.Session, reply)
	case queue.KindGoalInput:
		if text == "" {
			return fail("an answer needs -f %s=<text>", core.QueueTextField)
		}
		return d.queuePrompt(ctx, e.Session, text)
	case queue.KindDecision:
		if text == "" {
			return fail("an answer needs -f %s=<text>", core.QueueTextField)
		}
		res := d.queueItemState(ctx, e, items.VerbDone, "answered: "+boundRunes(text, items.MaxNoteRunes-len("answered: ")))
		if !res.OK {
			return res
		}
		// Deliver the answer to the source when it can still act on it; the
		// item records the answer either way.
		if src, found, err := lookupSession(e.Session); err == nil && found && !src.Archived {
			msg := fmt.Sprintf("The user answered your decision item %s %q:\n\n%s", e.Item, e.Title, text)
			if r := d.queuePrompt(ctx, e.Session, msg); !r.OK {
				return core.Result{Type: "result", OK: true, Error: "the item is done, but the answer was not delivered: " + r.Error}
			}
		}
		return ok()
	}
	return fail("unknown queue entry kind %q", e.Kind)
}

// queuePrompt delivers text to a session as the user's own prompt. A prompt
// to a Claude session sitting on an AskUserQuestion selector first dismisses it
// (#316), so the text lands as the answer; a stopped session is started by it.
func (d *Daemon) queuePrompt(ctx context.Context, id, text string) core.Result {
	return d.handle(ctx, core.Action{Action: core.ActionSteer, ID: id, Fields: map[string]string{
		core.SteerVerb: core.SteerPrompt, core.SteerText: text,
	}})
}

// queueItemState moves a decision item as the host (the user).
func (d *Daemon) queueItemState(ctx context.Context, e queue.Entry, verb, note string) core.Result {
	return d.handleItemAction(itemsvc.WithHost(ctx), core.Action{Action: core.ActionItemState, ID: items.GlobalInbox(), Fields: map[string]string{
		core.ItemIDField: e.Item, core.ItemStateField: verb, core.ItemNoteField: note,
	}})
}

// boundedOverview keeps an overview within the session RPC response bound by
// dropping the newest-ranked entries (counts and summary stay whole).
func boundedOverview(o queue.Overview) queue.Overview {
	truncated := false
	for len(o.Entries) > 0 {
		b, err := json.Marshal(o)
		if err == nil && len(b) <= sessionrpc.MaxResponseBody-256 {
			break
		}
		o.Entries = o.Entries[:len(o.Entries)*3/4]
		truncated = true
	}
	if truncated {
		o.Errors = append(append([]string(nil), o.Errors...), "entries truncated to the response bound")
	}
	return o
}

func boundRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
