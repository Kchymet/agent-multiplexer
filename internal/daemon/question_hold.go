package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"amux/internal/amuxcfg"
	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/queue"
	"amux/internal/store"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// The Claude question hold (docs/question-prompts.md §4.1). Claude Code runs
// `amux agent question hold` as a blocking PreToolUse hook on AskUserQuestion.
// The hook registers the call here (QueryQuestionHold) and long-polls; while
// it is held, the queue entry's delivery is structured, and an answer from any
// amux surface resolves the hold: the hook then allows the call with the
// answers as updatedInput, or denies it with the dismissal as its reason.
//
// The hold never costs the user Claude's own dialog. It is released — the
// hook prints nothing and Claude draws the dialog — when someone types into
// the agent's pane, when the user releases it (`amux inbox release`, the
// TUI focusing the pane, "answer in pane" in a client), at its deadline (below
// both the hook's timeout and the user's askUserQuestionTimeout), when the
// switch questions.claude_hold is off, or when the call cannot be held (an
// input too large or not a question). The hook itself fails open: any error
// reaching the daemon releases at once.

// holdPoll is how often a waiting hold re-checks its release conditions.
const holdPoll = 250 * time.Millisecond

// holdForget is how long a finished hold is kept for an idempotent re-poll,
// and how long a hold whose hook stopped polling is kept before it is
// forgotten.
const holdForget = 2 * time.Minute

// errHoldGone: the call is no longer held (released, answered, or never was).
var errHoldGone = errors.New("the question is no longer held for an answer")

// heldQuestion is one held AskUserQuestion call.
type heldQuestion struct {
	session, toolUseID string
	input              json.RawMessage
	form               harnessproto.Form
	registered         time.Time
	deadline           time.Time
	humanMark          time.Time
	lastPoll           time.Time
	result             *core.QuestionHoldResult
	finished           time.Time
	changed            chan struct{} // closed when result is set
}

func (h *heldQuestion) held() bool { return h.result == nil }

// questionHolds is the daemon's hold registry, keyed by session and call.
type questionHolds struct {
	mu sync.Mutex
	m  map[string]*heldQuestion
}

func holdKey(session, toolUseID string) string { return session + "\x00" + toolUseID }

// get returns the hold for a call, if any.
func (q *questionHolds) get(session, toolUseID string) (*heldQuestion, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	h, ok := q.m[holdKey(session, toolUseID)]
	return h, ok
}

// isHeld reports whether a call is held right now.
func (q *questionHolds) isHeld(session, toolUseID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	h, ok := q.m[holdKey(session, toolUseID)]
	return ok && h.held()
}

// heldFor lists a session's held calls, oldest first.
func (q *questionHolds) heldFor(session string) []*heldQuestion {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []*heldQuestion
	for _, h := range q.m {
		if h.session == session && h.held() {
			out = append(out, h)
		}
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].registered.Before(out[j-1].registered); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// finish sets a held call's result once; false when it was not held.
func (q *questionHolds) finish(session, toolUseID string, r core.QuestionHoldResult, now time.Time) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	h, ok := q.m[holdKey(session, toolUseID)]
	if !ok || !h.held() {
		return false
	}
	h.result, h.finished = &r, now
	close(h.changed)
	return true
}

// register adds a call (idempotent: a re-poll finds the first registration)
// and forgets stale holds.
func (q *questionHolds) register(h *heldQuestion, now time.Time) *heldQuestion {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.m == nil {
		q.m = map[string]*heldQuestion{}
	}
	for k, old := range q.m {
		if (!old.held() && now.Sub(old.finished) > holdForget) || (old.held() && now.Sub(old.lastPoll) > holdForget) {
			delete(q.m, k)
		}
	}
	k := holdKey(h.session, h.toolUseID)
	if cur, ok := q.m[k]; ok {
		cur.lastPoll = now
		return cur
	}
	h.lastPoll = now
	h.changed = make(chan struct{})
	q.m[k] = h
	return h
}

// holdDeadline is when a hold registered at now is released: before the
// hook's own timeout and before the user's askUserQuestionTimeout, each by a
// margin.
func holdDeadline(now time.Time, askTimeout time.Duration, askSet bool) time.Time {
	d := time.Duration(claudecfg.QuestionHoldHookTimeout)*time.Second - claudecfg.QuestionHoldMargin
	if askSet {
		margin := min(claudecfg.QuestionHoldMargin, askTimeout/4)
		d = min(d, askTimeout-margin)
	}
	return now.Add(max(d, time.Second))
}

// holdEnabled reads the questions.claude_hold switch (tests replace it).
func (d *Daemon) holdEnabled() bool {
	if d.holdEnabledFn != nil {
		return d.holdEnabledFn()
	}
	return amuxcfg.ResolveQuestionHold()
}

// lastHumanInput is when someone last typed into the session's agent pane
// (zero when unknown).
func (d *Daemon) lastHumanInput(session string) time.Time {
	if d.lastHumanInputFn != nil {
		return d.lastHumanInputFn(session)
	}
	if d.engine == nil {
		return time.Time{}
	}
	in, ok := d.engine.Lookup(engine.Key{AgentID: session, Tab: panespec.TabAgent})
	if !ok || in == nil {
		return time.Time{}
	}
	obs, ok := in.(paneObserver)
	if !ok {
		return time.Time{}
	}
	return obs.LastHumanInput()
}

// askTimeout reads the session's askUserQuestionTimeout.
func (d *Daemon) askTimeout(session string) (time.Duration, bool) {
	sess, found, err := lookupSession(session)
	if err != nil || !found {
		return 0, false
	}
	return claudecfg.At(claudecfg.AgentHome(sess.Dir)).AskUserQuestionTimeout()
}

func release() core.QuestionHoldResult { return core.QuestionHoldResult{Action: core.HoldRelease} }

// holdQuestion serves QueryQuestionHold: register the caller's call, then wait
// up to timeout for its outcome. It never holds an admission lock.
func (d *Daemon) holdQuestion(ctx context.Context, session string, fields map[string]string) core.QuestionHoldResult {
	toolUseID := strings.TrimSpace(fields[core.HoldToolUseField])
	input := json.RawMessage(fields[core.HoldInputField])
	timeout, err := waitTimeout(map[string]string{core.WaitTimeoutField: firstNonEmpty(fields[core.HoldTimeoutField], fmt.Sprint(core.MaxWaitSeconds))})
	if err != nil || toolUseID == "" || len(input) > core.MaxHoldInputBytes || !json.Valid(input) {
		return release()
	}
	now := time.Now()
	h, found := d.holds.get(session, toolUseID)
	if !found {
		if !d.holdEnabled() {
			return release()
		}
		form, ok := queue.ClaudeForm(input)
		if !ok {
			return release()
		}
		ask, askSet := d.askTimeout(session)
		h = &heldQuestion{session: session, toolUseID: toolUseID, input: input, form: form,
			registered: now, deadline: holdDeadline(now, ask, askSet), humanMark: d.lastHumanInput(session)}
	}
	h = d.holds.register(h, now)
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(holdPoll)
	defer tick.Stop()
	for {
		d.holds.mu.Lock()
		res := h.result
		d.holds.mu.Unlock()
		if res != nil {
			return *res
		}
		now := time.Now()
		switch {
		case !now.Before(h.deadline):
			d.holds.finish(session, toolUseID, release(), now)
			continue
		case !d.lastHumanInput(session).Equal(h.humanMark):
			// Someone is at the pane: hand them Claude's own dialog.
			d.holds.finish(session, toolUseID, release(), now)
			continue
		case !now.Before(deadline):
			return core.QuestionHoldResult{Action: core.HoldWait, Deadline: h.deadline.UnixMilli()}
		}
		select {
		case <-ctx.Done():
			return core.QuestionHoldResult{Action: core.HoldWait, Deadline: h.deadline.UnixMilli()}
		case <-h.changed:
		case <-tick.C:
		}
	}
}

// releaseHold hands a held call back to Claude's dialog.
func (d *Daemon) releaseHold(session, toolUseID string) bool {
	return d.holds.finish(session, toolUseID, release(), time.Now())
}

// holdAnswer resolves a held call with a form answer: allow with the answers
// (and annotations) merged into the call's original input, or deny with the
// dismissal as the reason.
func (d *Daemon) holdAnswer(session, toolUseID string, a harnessproto.FormAnswer) error {
	h, ok := d.holds.get(session, toolUseID)
	if !ok {
		return errHoldGone
	}
	r, err := holdResultFor(h.input, h.form, a)
	if err != nil {
		return err
	}
	if !d.holds.finish(session, toolUseID, r, time.Now()) {
		return errHoldGone
	}
	return nil
}

// Dismissal reasons a held call is denied with (the agent reads them).
const (
	holdDismissed        = "The user dismissed these questions without answering."
	holdDismissedReplied = "The user dismissed these questions and replied: "
)

// holdResultFor builds the hook's answer for a form answer. Claude keys
// answers and annotations by question text, joins a multi-select's labels
// (and any free text) with ", ", and takes the chosen option's preview with
// a note on a preview question. updatedInput replaces the whole input, so the
// original keys (questions, metadata, ...) are kept.
func holdResultFor(input json.RawMessage, f harnessproto.Form, a harnessproto.FormAnswer) (core.QuestionHoldResult, error) {
	if err := harnessproto.ValidateAnswer(f, a); err != nil {
		return core.QuestionHoldResult{}, err
	}
	if a.Action == harnessproto.FormActionCancel {
		reason := holdDismissed
		if m := strings.TrimSpace(a.Message); m != "" {
			reason = holdDismissedReplied + m
		}
		return core.QuestionHoldResult{Action: core.HoldDeny, Reason: reason}, nil
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(input, &in); err != nil || in == nil {
		return core.QuestionHoldResult{}, fmt.Errorf("the held call's input is not an object")
	}
	seen := map[string]bool{}
	for _, q := range f.Questions {
		if seen[q.Question] {
			return core.QuestionHoldResult{}, fmt.Errorf("two questions share the text %q; answer in the pane", q.Question)
		}
		seen[q.Question] = true
	}
	answers := map[string]string{}
	type annotation struct {
		Notes   string `json:"notes,omitempty"`
		Preview string `json:"preview,omitempty"`
	}
	annotations := map[string]annotation{}
	for _, qa := range a.Answers {
		for _, q := range f.Questions {
			if q.ID != qa.ID {
				continue
			}
			var parts []string
			for _, i := range qa.Selected {
				parts = append(parts, q.Options[i].Label)
			}
			if qa.Other != nil {
				parts = append(parts, strings.TrimSpace(*qa.Other))
			}
			answers[q.Question] = strings.Join(parts, ", ")
			ann := annotation{Notes: strings.TrimSpace(qa.Note)}
			if !q.MultiSelect && len(qa.Selected) == 1 {
				ann.Preview = q.Options[qa.Selected[0]].Preview
			}
			if ann.Notes != "" || ann.Preview != "" {
				annotations[q.Question] = ann
			}
		}
	}
	in["answers"], _ = json.Marshal(answers)
	if len(annotations) > 0 {
		in["annotations"], _ = json.Marshal(annotations)
	} else {
		delete(in, "annotations")
	}
	out, err := json.Marshal(in)
	if err != nil {
		return core.QuestionHoldResult{}, err
	}
	return core.QuestionHoldResult{Action: core.HoldAllow, UpdatedInput: out}, nil
}

// heldQuestionEntries lists a session's held calls the transcript has not
// shown yet (Claude may write the call after its PreToolUse hook starts), so
// a held question is in the queue either way.
func heldQuestionEntries(holds []*heldQuestion, base queue.Entry, gen string, seen map[string]bool) []queue.Entry {
	var out []queue.Entry
	for _, h := range holds {
		if seen[h.toolUseID] {
			continue
		}
		e := base
		e.Kind = queue.KindQuestion
		e.ID = queue.EntryID(queue.KindQuestion, base.Session, gen, h.toolUseID)
		e.ToolUseID = h.toolUseID
		f := h.form
		f.Delivery = harnessproto.DeliveryStructured
		f.Deadline = h.deadline.UnixMilli()
		e.Form = &f
		e.Title = boundRunes(f.Questions[0].Question, 200)
		if len(f.Questions) > 1 {
			e.Title += fmt.Sprintf(" (+%d more)", len(f.Questions)-1)
		}
		e.Created = h.registered.UnixMilli()
		queue.ProjectLegacy(&e)
		out = append(out, e)
	}
	return out
}

// steerHeldQuestion handles typed steering to a session whose question is
// held: there is no dialog drawn to dismiss (Esc would interrupt the turn),
// so a person's text answers the hold instead — as the answer when it is
// exactly the legacy rendering of one (#73 questionReply), otherwise as a
// dismissal carrying the text as the user's reply. amux's own text (wake-ups,
// nudges) never touches the user's question. handled is false when nothing
// is held.
func (d *Daemon) steerHeldQuestion(sess store.Session, text string, automated bool) (handled bool, err error) {
	held := d.holds.heldFor(sess.ID)
	if len(held) == 0 {
		return false, nil
	}
	if automated {
		return true, &composerBusyError{reason: "the agent is waiting on the user's answer to its question"}
	}
	h := held[0]
	a, ok := queue.MatchReply(h.form, text)
	if !ok {
		a = harnessproto.FormAnswer{Version: harnessproto.FormVersion, Action: harnessproto.FormActionCancel, Message: text}
	}
	if err := d.holdAnswer(sess.ID, h.toolUseID, a); err != nil {
		return true, err
	}
	id := queue.EntryID(queue.KindQuestion, sess.ID, "", h.toolUseID)
	d.queue.markAnswered(id, time.Now())
	outcome := harnessproto.QuestionAnswered
	if a.Action == harnessproto.FormActionCancel {
		outcome = harnessproto.QuestionCancelled
	}
	d.questionWatch.noteOutcome(id, outcome)
	return true, nil
}
