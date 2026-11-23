package codexapp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// sendUserInput writes a request-user-input under method and returns the
// channel its response arrives on.
func sendUserInput(fs *fakeServer, method, id string, params map[string]any) chan incoming {
	rawID, _ := json.Marshal(id)
	ch := make(chan incoming, 1)
	fs.mu.Lock()
	fs.respByID[string(rawID)] = ch
	fs.mu.Unlock()
	fs.write(map[string]any{"method": method, "id": id, "params": params})
	return ch
}

func userInputParamsFixture(thread string) map[string]any {
	return map[string]any{
		"threadId": thread, "turnId": "turn_1", "itemId": "item_9",
		"questions": []map[string]any{
			{"id": "env", "header": "Env", "question": "Which environment?", "isOther": true,
				"options": []map[string]any{{"label": "staging", "description": "safe"}, {"label": "production"}}},
			{"id": "token", "header": "Token", "question": "Deploy token?", "isSecret": true},
		},
	}
}

func waitOpenInputs(t *testing.T, sup *Supervisor, n int) []UserInput {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := sup.OpenUserInputs()
		if len(got) == n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("open user inputs = %+v, want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestUserInputBothMethodNames pins the method-name fix: Codex's
// "item/tool/requestUserInput" and the legacy "tool/requestUserInput" both
// register an answerable form; neither is refused with -32601.
func TestUserInputBothMethodNames(t *testing.T) {
	for _, method := range []string{"item/tool/requestUserInput", "tool/requestUserInput"} {
		t.Run(method, func(t *testing.T) {
			sup, fs, client := newFakePair(t)
			defer fs.close()
			defer sup.Close()
			attach(t, sup, client)
			ch := sendUserInput(fs, method, "ui1", userInputParamsFixture("thr_1"))
			open := waitOpenInputs(t, sup, 1)
			f := open[0].Form
			if open[0].RequestID != "ui1" || f.Source != harnessproto.FormSourceCodexUserInput || f.Delivery != harnessproto.DeliveryStructured {
				t.Fatalf("open = %+v", open[0])
			}
			if len(f.Questions) != 2 || !f.Questions[0].Other || f.Questions[0].Options[1].Label != "production" ||
				!f.Questions[1].Secret || !f.Questions[1].Other || f.Reply || !f.Cancel {
				t.Fatalf("form = %+v", f)
			}
			select {
			case r := <-ch:
				t.Fatalf("request answered before anyone answered it: %+v", r)
			case <-time.After(100 * time.Millisecond):
			}
			other := " s3cr3t "
			a := harnessproto.FormAnswer{Version: 2, Action: harnessproto.FormActionAnswer, Answers: []harnessproto.QuestionAnswer{
				{ID: "env", Selected: []int{1}}, {ID: "token", Other: &other},
			}}
			if err := sup.AnswerUserInput("ui1", a); err != nil {
				t.Fatal(err)
			}
			var r incoming
			select {
			case r = <-ch:
			case <-time.After(2 * time.Second):
				t.Fatal("no response")
			}
			if r.Error != nil || string(r.Result) != `{"answers":{"env":{"answers":["production"]},"token":{"answers":["s3cr3t"]}}}` {
				t.Fatalf("response = %s / %+v", r.Result, r.Error)
			}
			if len(sup.OpenUserInputs()) != 0 {
				t.Fatal("an answered request is still open")
			}
			if err := sup.AnswerUserInput("ui1", a); !IsDuplicate(err) {
				t.Fatalf("second answer = %v, want duplicate", err)
			}
		})
	}
}

func TestUserInputCancelAndValidation(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)
	ch := sendUserInput(fs, "item/tool/requestUserInput", "ui2", userInputParamsFixture("thr_1"))
	waitOpenInputs(t, sup, 1)
	bad := harnessproto.FormAnswer{Version: 2, Action: harnessproto.FormActionAnswer, Answers: []harnessproto.QuestionAnswer{{ID: "env", Selected: []int{7}}}}
	if err := sup.AnswerUserInput("ui2", bad); err == nil {
		t.Fatal("an invalid answer was sent")
	}
	if len(sup.OpenUserInputs()) != 1 {
		t.Fatal("a refused answer consumed the request")
	}
	if err := sup.AnswerUserInput("ui2", harnessproto.FormAnswer{Version: 2, Action: harnessproto.FormActionCancel}); err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if string(r.Result) != `{"answers":{"env":{"answers":[]},"token":{"answers":[]}}}` {
		t.Fatalf("cancel response = %s", r.Result)
	}
	if err := sup.AnswerUserInput("nope", bad); !IsStale(err) {
		t.Fatalf("unknown request = %v, want stale", err)
	}
}

// TestUserInputForeignThreadNotAnswerable: a request naming another thread (or
// none) is surfaced but never becomes answerable here.
func TestUserInputForeignThreadNotAnswerable(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)
	col := subscribeCollector(context.Background(), sup)
	sendUserInput(fs, "item/tool/requestUserInput", "ui3", userInputParamsFixture("thr_other"))
	col.waitFor(t, harnessproto.TypeNotice)
	if got := sup.OpenUserInputs(); len(got) != 0 {
		t.Fatalf("foreign request answerable: %+v", got)
	}
}

// TestUserInputClearedByServerAndTurnEnd: the native TUI answering (the
// server's serverRequest/resolved) or the turn ending removes the request, so
// the queue entry clears everywhere.
func TestUserInputClearedByServerAndTurnEnd(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)
	sendUserInput(fs, "item/tool/requestUserInput", "ui4", userInputParamsFixture("thr_1"))
	waitOpenInputs(t, sup, 1)
	fs.pushNotify("serverRequest/resolved", map[string]any{"threadId": "thr_1", "requestId": "ui4"})
	waitOpenInputs(t, sup, 0)
	if err := sup.AnswerUserInput("ui4", harnessproto.FormAnswer{Version: 2, Action: harnessproto.FormActionCancel}); !IsDuplicate(err) {
		t.Fatalf("answer after the TUI answered = %v, want duplicate", err)
	}

	sendUserInput(fs, "item/tool/requestUserInput", "ui5", userInputParamsFixture("thr_1"))
	waitOpenInputs(t, sup, 1)
	sup.clearApprovalsForTurn("turn_1")
	waitOpenInputs(t, sup, 0)
}

// TestApprovalAllowForSession: the approval options offer acceptForSession and
// an allow_session decision answers with it.
func TestApprovalAllowForSession(t *testing.T) {
	if got := decisionToResult(harnessproto.DecisionAllowSession); got != "acceptForSession" {
		t.Fatalf("allow_session -> %q", got)
	}
	if decisionToResult("allow") != "accept" || decisionToResult("deny") != "decline" || decisionToResult("bogus") != "decline" {
		t.Fatal("decision mapping changed")
	}
}
