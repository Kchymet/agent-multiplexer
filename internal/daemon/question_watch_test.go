package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"amux/internal/core"
	"amux/internal/queue"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

type emitted struct {
	session, typ string
	payload      []byte
}

type emitRecorder struct {
	mu  sync.Mutex
	evs []emitted
}

func (r *emitRecorder) fn(session, typ string, payload any) {
	b, _ := json.Marshal(payload)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evs = append(r.evs, emitted{session, typ, b})
}

func (r *emitRecorder) take() []emitted {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.evs
	r.evs = nil
	return out
}

// TestQuestionWatcherOpensAndClears pins §3.4: an opened question is
// announced once with its form; one answered through amux resolves with that
// outcome (via amux); one that goes without amux — answered in the pane, or
// gone with its turn — resolves neutrally.
func TestQuestionWatcherOpensAndClears(t *testing.T) {
	rows := []core.Session{{ID: "m1", RootID: "wg1", State: core.StateWaiting}}
	qs := map[string][]openQuestion{"m1": {{ToolUseID: "tu1", Questions: []queue.Question{{Question: "Which?", Options: []queue.Option{{Label: "A"}, {Label: "B"}}}}}}}
	d := fakeQueueDaemon(t, rows, nil, qs, nil, nil)
	rec := &emitRecorder{}
	d.emitQuestionFn = rec.fn
	d.queuePromptFn = (&promptRecorder{}).fn

	d.questionPass(d.queueSources())
	evs := rec.take()
	if len(evs) != 1 || evs[0].typ != harnessproto.TypeQuestionRequest || evs[0].session != "m1" {
		t.Fatalf("open events = %+v", evs)
	}
	var req harnessproto.QuestionRequestPayload
	if err := json.Unmarshal(evs[0].payload, &req); err != nil || req.Form.Questions[0].Question != "Which?" || req.RequestID == "" {
		t.Fatalf("request payload = %s", evs[0].payload)
	}
	d.questionPass(d.queueSources())
	if evs := rec.take(); len(evs) != 0 {
		t.Fatalf("an open question announced twice: %+v", evs)
	}

	// Answered in the pane: it goes, neutrally.
	delete(qs, "m1")
	d.questionPass(d.queueSources())
	evs = rec.take()
	var res harnessproto.QuestionResolvedPayload
	if len(evs) != 1 || evs[0].typ != harnessproto.TypeQuestionResolved || json.Unmarshal(evs[0].payload, &res) != nil ||
		res.RequestID != req.RequestID || res.Outcome != harnessproto.QuestionCleared || res.Via != harnessproto.ViaUnknown {
		t.Fatalf("pane resolution = %+v %s", evs, evs[0].payload)
	}

	// Answered through amux (the legacy typed reply declines the selector).
	qs["m1"] = []openQuestion{{ToolUseID: "tu2", Questions: []queue.Question{{Question: "Again?", Options: []queue.Option{{Label: "A"}, {Label: "B"}}}}}}
	d.questionPass(d.queueSources())
	rec.take()
	id := d.queueOverview(nil).Entries[0].ID
	if r := d.handle(context.Background(), core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: id, core.QueueChoiceField: "B"}}); !r.OK {
		t.Fatalf("answer = %+v", r)
	}
	// The answered entry leaves the queue at once (markAnswered), even before
	// the runtime records it.
	d.questionPass(d.queueSources())
	evs = rec.take()
	if len(evs) != 1 || json.Unmarshal(evs[0].payload, &res) != nil || res.RequestID != id ||
		res.Outcome != harnessproto.QuestionCancelled || res.Via != harnessproto.ViaAmux {
		t.Fatalf("amux resolution = %+v", evs)
	}
}
