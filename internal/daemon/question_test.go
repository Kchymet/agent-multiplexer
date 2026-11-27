package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/claudegoal"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/queue"
	"amux/internal/store"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// promptRecorder captures the typed replies a queue answer delivers.
type promptRecorder struct {
	mu    sync.Mutex
	sent  []string
	block chan struct{}
}

func (p *promptRecorder) fn(_ context.Context, id, text string) core.Result {
	if p.block != nil {
		<-p.block
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, id+": "+text)
	return core.Result{OK: true}
}

func formJSON(t *testing.T, a harnessproto.FormAnswer) string {
	t.Helper()
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const previewAsk = `{"questions":[{"header":"Layout","question":"Which layout?","options":[{"label":"Tabs","preview":"[tabs]"},{"label":"Page"}]}]}`

func TestQueueEntryCarriesForm(t *testing.T) {
	rows := []core.Session{{ID: "m1", RootID: "wg1", State: core.StateWaiting}}
	f, _ := queue.ClaudeForm(json.RawMessage(previewAsk))
	qs := map[string][]openQuestion{"m1": {{ToolUseID: "tu1", Questions: queue.LegacyQuestions(f), Form: &f}}}
	d := fakeQueueDaemon(t, rows, nil, qs, nil, nil)
	e := d.queueOverview(nil).Entries[0]
	if e.Form == nil || e.Form.Questions[0].Options[0].Preview != "[tabs]" || e.Form.Delivery != harnessproto.DeliveryPane {
		t.Fatalf("form = %+v", e.Form)
	}
	if len(e.Questions) != 1 || !strings.Contains(e.Body, "previews are hidden") {
		t.Fatalf("legacy projection = %+v / %q", e.Questions, e.Body)
	}
	// A source without a form (legacy questions only) still gets one.
	qs["m1"] = []openQuestion{{ToolUseID: "tu1", Questions: []queue.Question{{Question: "Which?", Options: []queue.Option{{Label: "A"}, {Label: "B"}}}}}}
	if e := d.queueOverview(nil).Entries[0]; e.Form == nil || e.Form.Questions[0].ID != "q0" || !e.Form.Questions[0].Other {
		t.Fatalf("derived form = %+v", e.Form)
	}
}

func TestQueueLegacyAnswerKeepsTypedReply(t *testing.T) {
	rows := []core.Session{{ID: "m1", RootID: "wg1", State: core.StateWaiting}}
	qs := map[string][]openQuestion{"m1": {{ToolUseID: "tu1", Questions: []queue.Question{
		{Header: "Scope", Question: "Scope?", MultiSelect: true, Options: []queue.Option{{Label: "api"}, {Label: "web"}}},
		{Header: "Ship", Question: "Ship today?", Options: []queue.Option{{Label: "yes"}, {Label: "no"}}},
	}}}}
	d := fakeQueueDaemon(t, rows, nil, qs, nil, nil)
	rec := &promptRecorder{}
	d.queuePromptFn = rec.fn
	ctx := context.Background()
	id := d.queueOverview(nil).Entries[0].ID

	// A partial legacy answer is refused rather than guessed at.
	if res := d.handle(ctx, core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: id, core.QueueChoiceField: "api|web"}}); res.OK {
		t.Fatal("a partial multi-question answer was accepted")
	}
	res := d.handle(ctx, core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: id, core.QueueChoiceField: "api|web\nyes"}})
	if !res.OK || len(rec.sent) != 1 || rec.sent[0] != "m1: Scope: api, web\nShip: yes" {
		t.Fatalf("legacy answer = %+v, sent %q", res, rec.sent)
	}
	// Answered: a second answer reports it.
	res = d.handle(ctx, core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: id, core.QueueTextField: "x"}})
	if res.OK {
		t.Fatalf("second answer = %+v", res)
	}
}

func TestQueueFormAnswerRefusedWithoutChannel(t *testing.T) {
	rows := []core.Session{{ID: "m1", RootID: "wg1", State: core.StateWaiting}}
	qs := map[string][]openQuestion{"m1": {{ToolUseID: "tu1", Questions: []queue.Question{{Question: "Which?", Options: []queue.Option{{Label: "A"}, {Label: "B"}}}}}}}
	d := fakeQueueDaemon(t, rows, nil, qs, nil, nil)
	rec := &promptRecorder{}
	d.queuePromptFn = rec.fn
	id := d.queueOverview(nil).Entries[0].ID
	a := harnessproto.FormAnswer{Version: 2, Action: harnessproto.FormActionAnswer, Answers: []harnessproto.QuestionAnswer{{ID: "q0", Selected: []int{1}}}}
	res := d.handle(context.Background(), core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: id, core.QueueFormField: formJSON(t, a)}})
	if res.OK || res.Code != harnessproto.CodeAnswerInPane || len(rec.sent) != 0 {
		t.Fatalf("v2 answer without a channel = %+v (sent %q)", res, rec.sent)
	}
	// Invalid and unknown-key answers are refused before delivery.
	for _, raw := range []string{`{"version":2,"action":"answer"}`, `{"version":2,"action":"cancel","x":1}`, `not json`} {
		if res := d.handle(context.Background(), core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: id, core.QueueFormField: raw}}); res.OK || res.Code == harnessproto.CodeAnswerInPane {
			t.Errorf("%s: %+v", raw, res)
		}
	}
	// The entry stays open after every refusal (no claim left behind).
	if c, held := d.queue.claimOf(id); held {
		t.Fatalf("claim left behind: %+v", c)
	}
}

func TestQueueFirstAnswerWins(t *testing.T) {
	rows := []core.Session{{ID: "m1", RootID: "wg1", State: core.StateWaiting}}
	qs := map[string][]openQuestion{"m1": {{ToolUseID: "tu1", Questions: []queue.Question{{Question: "Which?", Options: []queue.Option{{Label: "A"}, {Label: "B"}}}}}}}
	d := fakeQueueDaemon(t, rows, nil, qs, nil, nil)
	rec := &promptRecorder{block: make(chan struct{})}
	d.queuePromptFn = rec.fn
	ctx := context.Background()
	id := d.queueOverview(nil).Entries[0].ID

	first := make(chan core.Result, 1)
	go func() {
		first <- d.handle(ctx, core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: id, core.QueueChoiceField: "A", core.QueueByField: "direct:cl_1"}})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, held := d.queue.claimOf(id); held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first answer never claimed the entry")
		}
		time.Sleep(time.Millisecond)
	}
	// The overview shows who is answering.
	if e := d.queueOverview(nil).Entries[0]; e.Form == nil || e.Form.Claim == nil || e.Form.Claim.By != "direct:cl_1" {
		t.Fatalf("claim not shown: %+v", e.Form)
	}
	second := d.handle(ctx, core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: id, core.QueueChoiceField: "B"}})
	if second.OK || second.Code != codeAnswered {
		t.Fatalf("second answer = %+v, want %s", second, codeAnswered)
	}
	close(rec.block)
	if r := <-first; !r.OK {
		t.Fatalf("first answer = %+v", r)
	}
	if len(rec.sent) != 1 || rec.sent[0] != "m1: A" {
		t.Fatalf("delivered %q", rec.sent)
	}
}

func TestSteerAnswerBindsSession(t *testing.T) {
	rows := []core.Session{{ID: "m1", RootID: "wg1", State: core.StateWaiting}, {ID: "m2", RootID: "wg1", State: core.StateWaiting}}
	qs := map[string][]openQuestion{"m1": {{ToolUseID: "tu1", Questions: []queue.Question{{Question: "Which?", Options: []queue.Option{{Label: "A"}, {Label: "B"}}}}}}}
	d := fakeQueueDaemon(t, rows, nil, qs, nil, nil)
	id := d.queueOverview(nil).Entries[0].ID
	cancel := `{"version":2,"action":"cancel"}`
	// Sent to another session: that session has no such question.
	res := d.handle(context.Background(), core.Action{Action: core.ActionSteer, ID: "m2", Fields: map[string]string{
		core.SteerVerb: core.SteerAnswer, core.SteerRequestID: id, core.SteerAnswerField: cancel}})
	if res.OK || res.Code != codeAskerGone {
		t.Fatalf("cross-session answer = %+v", res)
	}
	// Sent to its own session it reaches the queue (and, without a channel,
	// is refused as answer_in_pane rather than typed).
	res = d.handle(context.Background(), core.Action{Action: core.ActionSteer, ID: "m1", Fields: map[string]string{
		core.SteerVerb: core.SteerAnswer, core.SteerRequestID: id, core.SteerAnswerField: cancel}})
	if res.OK || res.Code != harnessproto.CodeAnswerInPane {
		t.Fatalf("own-session answer = %+v", res)
	}
}

// TestCoordinatorArchiveQuestionStillAnswers pins the coordinator wrap-up
// flow (wrapUpAsk): a Claude coordinator in goal input runs `goal ask` and
// then AskUserQuestion "Archive this workgroup?". The queue lists the question
// once (the goal ask collapses into it) and a legacy answer — the phone's
// one-tap label, or free text — still reaches the coordinator as its reply.
func TestCoordinatorArchiveQuestionStillAnswers(t *testing.T) {
	rows := []core.Session{{ID: "wg2", IsRoot: true, Role: store.RoleCoordinator, State: core.StateWaiting,
		Supervision: &harnessproto.Supervision{Goal: claudegoal.StatusInput}}}
	archive := openQuestion{ToolUseID: "tu9", Questions: []queue.Question{{Header: "Wrap up", Question: "Archive this workgroup?",
		Options: []queue.Option{{Label: "Archive it", Description: "every PR is merged"}, {Label: "Keep it open"}}}}}
	goals := map[string]claudegoal.State{"wg2": {Status: claudegoal.StatusInput, Note: "Archive this workgroup?"}}
	for _, tc := range []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"label", map[string]string{core.QueueChoiceField: "Archive it"}, "wg2: Archive it"},
		{"free text", map[string]string{core.QueueTextField: "not yet, wait for #12"}, "wg2: not yet, wait for #12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := fakeQueueDaemon(t, rows, nil, map[string][]openQuestion{"wg2": {archive}}, goals, nil)
			rec := &promptRecorder{}
			d.queuePromptFn = rec.fn
			ov := d.queueOverview(nil)
			if len(ov.Entries) != 1 || ov.Entries[0].Kind != queue.KindQuestion || ov.Entries[0].Workgroup != "wg2" {
				t.Fatalf("entries = %+v", ov.Entries)
			}
			e := ov.Entries[0]
			if len(e.Questions) != 1 || e.Questions[0].Options[0].Label != "Archive it" || e.Form == nil {
				t.Fatalf("archive question = %+v", e)
			}
			tc.fields[core.QueueEntryField] = e.ID
			res := d.handle(context.Background(), core.Action{Action: core.ActionQueueAnswer, Fields: tc.fields})
			if !res.OK || len(rec.sent) != 1 || rec.sent[0] != tc.want {
				t.Fatalf("answer = %+v, sent %q", res, rec.sent)
			}
		})
	}
}

// TestCodexUserInputEntries: a structured Codex session's open
// request-user-input is a question entry with a structured form; a secret
// question keeps it from legacy clients, which are refused, not misdelivered.
func TestCodexUserInputEntries(t *testing.T) {
	rows := []core.Session{{ID: "c1", RootID: "wg1", State: core.StateRunning, ControlMode: harnessproto.ControlModeStructured}}
	d := fakeQueueDaemon(t, rows, nil, nil, nil, nil)
	form := harnessproto.Form{Version: 2, Source: harnessproto.FormSourceCodexUserInput, Cancel: true, Delivery: harnessproto.DeliveryStructured,
		Questions: []harnessproto.FormQuestion{{ID: "token", Question: "Deploy token?", Other: true, Secret: true}}}
	base := d.queueSourcesFn
	d.queueSourcesFn = func() queueSources {
		src := base()
		src.inputs = func(id string) []codexapp.UserInput {
			if id != "c1" {
				return nil
			}
			return []codexapp.UserInput{{RequestID: "ui7", Form: form}}
		}
		return src
	}
	ov := d.queueOverview(nil)
	if len(ov.Entries) != 1 {
		t.Fatalf("entries = %+v", ov.Entries)
	}
	e := ov.Entries[0]
	if e.Kind != queue.KindQuestion || e.RequestID != "ui7" || e.Form == nil || e.Form.Delivery != harnessproto.DeliveryStructured ||
		e.Questions != nil || !strings.HasPrefix(e.Title, queue.LegacyOmittedTitle) {
		t.Fatalf("entry = %+v", e)
	}
	ctx := context.Background()
	res := d.handle(ctx, core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: e.ID, core.QueueTextField: "s3cr3t"}})
	if res.OK || res.Code != harnessproto.CodeNeedsNewerClient {
		t.Fatalf("legacy answer to a secret = %+v", res)
	}
	// A v2 answer goes to the supervisor; with no live runtime it is gone.
	other := "s3cr3t"
	a := harnessproto.FormAnswer{Version: 2, Action: harnessproto.FormActionAnswer, Answers: []harnessproto.QuestionAnswer{{ID: "token", Other: &other}}}
	res = d.handle(ctx, core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: e.ID, core.QueueFormField: formJSON(t, a)}})
	if res.OK || res.Code != codeAskerGone {
		t.Fatalf("v2 answer without a runtime = %+v", res)
	}
}
