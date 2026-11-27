package daemon

import (
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/queue"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// The question watcher (docs/question-prompts.md §3.4): after each rail poll
// it diffs the open question entries against the last pass and records, on
// each asking session's own event stream, a question_request when one opens
// and a question_resolved when one goes — answered through amux, answered or
// dismissed in the pane, or gone with its turn — so a transcript card, a queue
// row and a notification clear everywhere at once. It runs off the poll's
// goroutine, at most once at a time and at most once per questionWatchEvery.

const questionWatchEvery = time.Second

// questionWatch is the watcher's memory: what was open on the last pass, and
// how amux resolved the entries it delivered.
type questionWatch struct {
	running atomic.Bool
	mu      sync.Mutex
	last    time.Time
	open    map[string]questionSeen
	// outcomes are the outcomes of answers amux delivered, by entry id.
	outcomes map[string]string
}

// questionSeen is one open question as the watcher last saw it.
type questionSeen struct {
	session, gen string
	structured   bool
}

// noteOutcome records how amux resolved an entry (answered or cancelled).
func (w *questionWatch) noteOutcome(id, outcome string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.outcomes == nil {
		w.outcomes = map[string]string{}
	}
	w.outcomes[id] = outcome
}

// watchQuestions starts one pass unless one is running or ran just now.
func (d *Daemon) watchQuestions() {
	w := &d.questionWatch
	w.mu.Lock()
	if time.Since(w.last) < questionWatchEvery {
		w.mu.Unlock()
		return
	}
	w.last = time.Now()
	w.mu.Unlock()
	if !w.running.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer w.running.Store(false)
		d.questionPass(d.queueSources())
	}()
}

// questionPass is one diff of the open question entries.
func (d *Daemon) questionPass(src queueSources) {
	entries, _, rows, _ := d.deriveQueue(src)
	structured := map[string]bool{}
	for _, r := range rows {
		structured[r.ID] = r.ControlMode == harnessproto.ControlModeStructured
	}
	now := map[string]questionSeen{}
	var opened []queue.Entry
	w := &d.questionWatch
	w.mu.Lock()
	for _, e := range entries {
		if e.Kind != queue.KindQuestion || e.Form == nil || d.queue.wasAnswered(e.ID, time.Now()) {
			continue // an entry amux answered is resolved, recorded or not
		}
		now[e.ID] = questionSeen{session: e.Session, gen: e.RuntimeGeneration, structured: structured[e.Session]}
		if _, was := w.open[e.ID]; !was {
			opened = append(opened, e)
		}
	}
	var closed []struct {
		id      string
		seen    questionSeen
		outcome string
	}
	for id, seen := range w.open {
		if _, still := now[id]; still {
			continue
		}
		outcome, ok := w.outcomes[id]
		delete(w.outcomes, id)
		if !ok {
			outcome = ""
		}
		closed = append(closed, struct {
			id      string
			seen    questionSeen
			outcome string
		}{id, seen, outcome})
	}
	w.open = now
	w.mu.Unlock()

	// On the first pass after a daemon start every open question is
	// announced (a reconnecting client then sees it); nothing is resolved.
	for _, e := range opened {
		d.emitQuestionEvent(e.Session, structured[e.Session], harnessproto.TypeQuestionRequest, "question: "+e.Title,
			harnessproto.QuestionRequestPayload{RequestID: e.ID, RuntimeGeneration: e.RuntimeGeneration, Form: *e.Form})
	}
	for _, c := range closed {
		p := harnessproto.QuestionResolvedPayload{RequestID: c.id, RuntimeGeneration: c.seen.gen, Outcome: c.outcome, Via: harnessproto.ViaAmux}
		if c.outcome == "" {
			// Not amux's answer: answered or dismissed in the pane (or by a
			// peer client), or gone with its turn. Which way it went is not
			// read from the runtime yet, so the resolution is neutral.
			p.Outcome, p.Via = harnessproto.QuestionCleared, harnessproto.ViaUnknown
		}
		d.emitQuestionEvent(c.seen.session, c.seen.structured, harnessproto.TypeQuestionResolved, "question "+p.Outcome, p)
	}
}

// emitQuestionEvent records a question event on the session's own stream
// (its amux journal, and its structured log when that is the stream's
// source), as the PR watcher records PR transitions.
func (d *Daemon) emitQuestionEvent(session string, structured bool, typ, text string, payload any) {
	if d.emitQuestionFn != nil {
		d.emitQuestionFn(session, typ, payload)
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if err := core.AppendJournalRecord(session, core.JournalRecord{Level: core.JournalInfo, Text: text, Type: typ, Payload: b}); err != nil {
		log.Printf("questions: journal %s: %v", session, err)
	}
	if structured {
		if err := codexapp.AppendEvent(session, harnessproto.RuntimeEvent{Type: typ, Direction: harnessproto.DirMeta, Payload: b}); err != nil {
			log.Printf("questions: structured event %s: %v", session, err)
		}
	}
}
