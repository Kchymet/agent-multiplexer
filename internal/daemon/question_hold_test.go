package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/queue"
	"amux/internal/store"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

const holdInput = `{"questions":[
 {"header":"Layout","question":"Which layout?","multiSelect":false,"options":[{"label":"Tabs","description":"t","preview":"[tabs]"},{"label":"Page","description":"p"}]},
 {"header":"Extras","question":"Which extras?","multiSelect":true,"options":[{"label":"Search","description":"s"},{"label":"Dark","description":"d"}]}],
 "metadata":{"source":"skill"}}`

func TestHoldResultForAnswer(t *testing.T) {
	f, _ := queue.ClaudeForm(json.RawMessage(holdInput))
	other := "export"
	a := harnessproto.FormAnswer{Version: 2, Action: harnessproto.FormActionAnswer, Answers: []harnessproto.QuestionAnswer{
		{ID: "q0", Selected: []int{0}, Note: "keep icons"},
		{ID: "q1", Selected: []int{0, 1}, Other: &other},
	}}
	r, err := holdResultFor(json.RawMessage(holdInput), f, a)
	if err != nil || r.Action != core.HoldAllow {
		t.Fatalf("result = %+v, %v", r, err)
	}
	var got struct {
		Questions   []json.RawMessage `json:"questions"`
		Metadata    map[string]string `json:"metadata"`
		Answers     map[string]string `json:"answers"`
		Annotations map[string]struct {
			Notes, Preview string
		} `json:"annotations"`
	}
	if err := json.Unmarshal(r.UpdatedInput, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Questions) != 2 || got.Metadata["source"] != "skill" {
		t.Fatalf("original input not echoed: %s", r.UpdatedInput)
	}
	if got.Answers["Which layout?"] != "Tabs" || got.Answers["Which extras?"] != "Search, Dark, export" {
		t.Fatalf("answers = %v", got.Answers)
	}
	if ann := got.Annotations["Which layout?"]; ann.Notes != "keep icons" || ann.Preview != "[tabs]" {
		t.Fatalf("annotations = %+v", got.Annotations)
	}
	if _, ok := got.Annotations["Which extras?"]; ok {
		t.Fatal("annotation without a note or preview")
	}

	cancel, _ := holdResultFor(json.RawMessage(holdInput), f, harnessproto.FormAnswer{Version: 2, Action: harnessproto.FormActionCancel})
	reply, _ := holdResultFor(json.RawMessage(holdInput), f, harnessproto.FormAnswer{Version: 2, Action: harnessproto.FormActionCancel, Message: "talk first"})
	if cancel.Action != core.HoldDeny || cancel.Reason != holdDismissed || reply.Reason != holdDismissedReplied+"talk first" {
		t.Fatalf("cancel = %+v, reply = %+v", cancel, reply)
	}
	if _, err := holdResultFor(json.RawMessage(holdInput), f, harnessproto.FormAnswer{Version: 2, Action: harnessproto.FormActionAnswer}); err == nil {
		t.Fatal("an invalid answer built a result")
	}
	dup := f
	dup.Questions = []harnessproto.FormQuestion{f.Questions[0], f.Questions[0]}
	dup.Questions[1].ID = "q1"
	if _, err := holdResultFor(json.RawMessage(holdInput), dup, harnessproto.FormAnswer{Version: 2, Action: harnessproto.FormActionAnswer,
		Answers: []harnessproto.QuestionAnswer{{ID: "q0", Selected: []int{0}}, {ID: "q1", Selected: []int{1}}}}); err == nil {
		t.Fatal("duplicate question texts answered structurally")
	}
}

func TestHoldDeadline(t *testing.T) {
	now := time.Unix(1000, 0)
	if got := holdDeadline(now, 0, false).Sub(now); got != 29*time.Minute {
		t.Fatalf("default deadline = %v", got)
	}
	if got := holdDeadline(now, time.Minute, true).Sub(now); got != 45*time.Second {
		t.Fatalf("60s askUserQuestionTimeout deadline = %v", got)
	}
	if got := holdDeadline(now, 10*time.Minute, true).Sub(now); got != 9*time.Minute {
		t.Fatalf("10m askUserQuestionTimeout deadline = %v", got)
	}
}

// holdDaemon is a queue daemon with the hold's clocks under test control.
type holdClock struct {
	mu    sync.Mutex
	human time.Time
}

func (c *holdClock) at(string) time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.human }
func (c *holdClock) typed()              { c.mu.Lock(); c.human = time.Now(); c.mu.Unlock() }

func holdDaemon(t *testing.T) (*Daemon, *holdClock) {
	t.Helper()
	rows := []core.Session{{ID: "m1", RootID: "wg1", State: core.StateWaiting}}
	d := fakeQueueDaemon(t, rows, nil, nil, nil, nil)
	clock := &holdClock{}
	d.lastHumanInputFn = clock.at
	d.holdEnabledFn = func() bool { return true }
	return d, clock
}

func holdFields(timeout string) map[string]string {
	return map[string]string{core.HoldToolUseField: "tu1", core.HoldInputField: holdInput, core.HoldTimeoutField: timeout}
}

// startHold registers a hold and returns a channel with its first outcome.
func startHold(d *Daemon, timeout string) chan core.QuestionHoldResult {
	ch := make(chan core.QuestionHoldResult, 1)
	go func() { ch <- d.holdQuestion(context.Background(), "m1", holdFields(timeout)) }()
	return ch
}

func waitHeld(t *testing.T, d *Daemon) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !d.holds.isHeld("m1", "tu1") {
		if time.Now().After(deadline) {
			t.Fatal("the call was never held")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHeldQuestionAnsweredFromQueue(t *testing.T) {
	d, _ := holdDaemon(t)
	res := startHold(d, "5")
	waitHeld(t, d)
	// The transcript has not shown the call yet: the hold alone lists it.
	ov := d.queueOverview(nil)
	if len(ov.Entries) != 1 || ov.Entries[0].Form == nil || ov.Entries[0].Form.Delivery != harnessproto.DeliveryStructured || ov.Entries[0].Form.Deadline == 0 {
		t.Fatalf("held entry = %+v", ov.Entries)
	}
	e := ov.Entries[0]
	if e.ID != queue.EntryID(queue.KindQuestion, "m1", "", "tu1") {
		t.Fatalf("held entry id %s differs from the transcript-derived id", e.ID)
	}
	a := harnessproto.FormAnswer{Version: 2, Action: harnessproto.FormActionAnswer, Answers: []harnessproto.QuestionAnswer{
		{ID: "q0", Selected: []int{1}}, {ID: "q1", Selected: []int{0}},
	}}
	r := d.handle(context.Background(), core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: e.ID, core.QueueFormField: formJSON(t, a)}})
	if !r.OK {
		t.Fatalf("answer = %+v", r)
	}
	got := <-res
	if got.Action != core.HoldAllow || !strings.Contains(string(got.UpdatedInput), `"answers":{"Which extras?":"Search","Which layout?":"Page"}`) {
		t.Fatalf("hook outcome = %+v %s", got, got.UpdatedInput)
	}
	// A re-poll (the hook retrying) sees the same outcome; the entry is gone.
	if again := d.holdQuestion(context.Background(), "m1", holdFields("0")); again.Action != core.HoldAllow {
		t.Fatalf("re-poll = %+v", again)
	}
	if ov := d.queueOverview(nil); len(ov.Entries) != 0 {
		t.Fatalf("answered entry still listed: %+v", ov.Entries)
	}
}

func TestHeldQuestionLegacyAnswerIsStructured(t *testing.T) {
	d, _ := holdDaemon(t)
	rec := &promptRecorder{}
	d.queuePromptFn = rec.fn
	res := startHold(d, "5")
	waitHeld(t, d)
	id := d.queueOverview(nil).Entries[0].ID
	r := d.handle(context.Background(), core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: id, core.QueueChoiceField: "Tabs\nSearch|Dark"}})
	if !r.OK || len(rec.sent) != 0 {
		t.Fatalf("legacy answer = %+v, typed %q", r, rec.sent)
	}
	if got := <-res; got.Action != core.HoldAllow || !strings.Contains(string(got.UpdatedInput), `"Which extras?":"Search, Dark"`) {
		t.Fatalf("hook outcome = %+v %s", got, got.UpdatedInput)
	}
}

func TestHeldQuestionReleases(t *testing.T) {
	t.Run("human input", func(t *testing.T) {
		d, clock := holdDaemon(t)
		res := startHold(d, "5")
		waitHeld(t, d)
		clock.typed()
		if got := <-res; got.Action != core.HoldRelease {
			t.Fatalf("outcome = %+v", got)
		}
	})
	t.Run("release action", func(t *testing.T) {
		d, _ := holdDaemon(t)
		res := startHold(d, "5")
		waitHeld(t, d)
		id := d.queueOverview(nil).Entries[0].ID
		if r := d.handle(context.Background(), core.Action{Action: core.ActionQueueRelease, Fields: map[string]string{core.QueueEntryField: id}}); !r.OK {
			t.Fatalf("release = %+v", r)
		}
		if got := <-res; got.Action != core.HoldRelease {
			t.Fatalf("outcome = %+v", got)
		}
		// Released: it is the pane's now, and an answer from amux is refused
		// rather than lost.
		if r := d.handle(context.Background(), core.Action{Action: core.ActionQueueRelease, Fields: map[string]string{core.QueueEntryField: id}}); r.OK {
			t.Fatal("a released question released twice")
		}
	})
	t.Run("switch off", func(t *testing.T) {
		d, _ := holdDaemon(t)
		d.holdEnabledFn = func() bool { return false }
		if got := d.holdQuestion(context.Background(), "m1", holdFields("1")); got.Action != core.HoldRelease {
			t.Fatalf("outcome = %+v", got)
		}
	})
	t.Run("not a question", func(t *testing.T) {
		d, _ := holdDaemon(t)
		f := holdFields("1")
		f[core.HoldInputField] = `{"command":"ls"}`
		if got := d.holdQuestion(context.Background(), "m1", f); got.Action != core.HoldRelease {
			t.Fatalf("outcome = %+v", got)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		d, _ := holdDaemon(t)
		res := startHold(d, "5")
		waitHeld(t, d)
		h, _ := d.holds.get("m1", "tu1")
		d.holds.mu.Lock()
		h.deadline = time.Now()
		d.holds.mu.Unlock()
		if got := <-res; got.Action != core.HoldRelease {
			t.Fatalf("outcome = %+v", got)
		}
	})
	t.Run("wait round", func(t *testing.T) {
		d, _ := holdDaemon(t)
		if got := d.holdQuestion(context.Background(), "m1", holdFields("0")); got.Action != core.HoldWait || got.Deadline == 0 {
			t.Fatalf("outcome = %+v", got)
		}
	})
}

// TestTypedSteerToHeldQuestion: with no dialog drawn, a person's typed reply
// answers the held call (exact label rendering) or dismisses it with the text
// as the reply; amux's own text never touches the user's question.
func TestTypedSteerToHeldQuestion(t *testing.T) {
	for _, tc := range []struct {
		text   string
		action string
		want   string
	}{
		{"Layout: Page\nExtras: Dark", core.HoldAllow, `"Which extras?":"Dark"`},
		{"hold on, why are you asking?", core.HoldDeny, holdDismissedReplied + "hold on, why are you asking?"},
	} {
		d, _ := holdDaemon(t)
		res := startHold(d, "5")
		waitHeld(t, d)
		if handled, err := d.steerHeldQuestion(store.Session{ID: "m1"}, "a wake-up", true); !handled || err == nil {
			t.Fatalf("automated text: handled=%v err=%v", handled, err)
		}
		handled, err := d.steerHeldQuestion(store.Session{ID: "m1"}, tc.text, false)
		if !handled || err != nil {
			t.Fatalf("%q: handled=%v err=%v", tc.text, handled, err)
		}
		got := <-res
		if got.Action != tc.action || !strings.Contains(string(got.UpdatedInput)+got.Reason, tc.want) {
			t.Fatalf("%q: outcome = %+v %s", tc.text, got, got.UpdatedInput)
		}
		if handled, _ := d.steerHeldQuestion(store.Session{ID: "m1"}, "next", false); handled {
			t.Fatal("nothing held, yet the steer was taken")
		}
	}
}
