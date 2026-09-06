package codexapp

import (
	"encoding/json"
	"testing"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func mapOne(method, params string) ([]harnessproto.RuntimeEvent, *turnResult) {
	return mapNotification(method, json.RawMessage(params), &streamState{})
}

func TestMapThreadStartedIsNotice(t *testing.T) {
	evs, res := mapOne("thread/started", `{"thread":{"id":"t1"}}`)
	if res != nil || len(evs) != 1 || evs[0].Type != harnessproto.TypeNotice {
		t.Fatalf("thread/started → %+v res=%v", evs, res)
	}
}

func TestMapTurnStartedEmitsTurnStart(t *testing.T) {
	evs, res := mapOne("turn/started", `{"threadId":"t","turn":{"id":"tr1"}}`)
	if res != nil || len(evs) != 1 || evs[0].Type != harnessproto.TypeTurnStart {
		t.Fatalf("turn/started → %+v res=%v", evs, res)
	}
	// Correlation invariant: turn_start carries the wire thread_id + turn_id.
	var p struct {
		ThreadID string `json:"thread_id"`
		TurnID   string `json:"turn_id"`
	}
	_ = json.Unmarshal(evs[0].Payload, &p)
	if p.ThreadID != "t" || p.TurnID != "tr1" {
		t.Fatalf("turn_start missing correlation ids: %s", evs[0].Payload)
	}
}

// The {turnId} form is honored when the turn object omits id.
func TestMapTurnStartedFlatTurnID(t *testing.T) {
	evs, _ := mapOne("turn/started", `{"threadId":"t2","turnId":"flat1"}`)
	var p struct {
		ThreadID string `json:"thread_id"`
		TurnID   string `json:"turn_id"`
	}
	_ = json.Unmarshal(evs[0].Payload, &p)
	if p.ThreadID != "t2" || p.TurnID != "flat1" {
		t.Fatalf("turn_start flat turnId not preserved: %s", evs[0].Payload)
	}
}

func TestMapTurnCompletedSignalsResultAndEnd(t *testing.T) {
	evs, res := mapOne("turn/completed", `{"threadId":"thr_x","turn":{"id":"tr1","status":"completed"}}`)
	if res == nil || res.StopReason != "completed" || res.IsError {
		t.Fatalf("turn/completed res = %+v", res)
	}
	// A completed turn emits turn_end (observed), no error notice.
	if len(evs) != 1 || evs[0].Type != harnessproto.TypeTurnEnd {
		t.Fatalf("completed turn events = %+v", evs)
	}
	var ep struct {
		StopReason string `json:"stop_reason"`
		ThreadID   string `json:"thread_id"`
		TurnID     string `json:"turn_id"`
	}
	_ = json.Unmarshal(evs[0].Payload, &ep)
	if ep.StopReason != "completed" {
		t.Fatalf("turn_end stop_reason = %q", ep.StopReason)
	}
	// Correlation invariant: turn_end carries the same thread_id + turn_id, so a
	// consumer can pair it with turn_start.
	if ep.ThreadID != "thr_x" || ep.TurnID != "tr1" {
		t.Fatalf("turn_end missing correlation ids: %s", evs[0].Payload)
	}
}

func TestMapTurnFailedEmitsErrorNoticeAndEnd(t *testing.T) {
	evs, res := mapOne("turn/completed", `{"turn":{"id":"tr1","status":"failed","error":{"message":"boom"}}}`)
	if res == nil || !res.IsError {
		t.Fatalf("failed turn res = %+v", res)
	}
	// notice(error) then turn_end.
	if len(evs) != 2 || evs[0].Type != harnessproto.TypeNotice || evs[1].Type != harnessproto.TypeTurnEnd {
		t.Fatalf("failed turn events = %+v", evs)
	}
	var p struct{ Level, Text string }
	_ = json.Unmarshal(evs[0].Payload, &p)
	if p.Level != "error" || p.Text != "boom" {
		t.Fatalf("error notice payload = %+v", p)
	}
}

func TestMapAgentMessageDeltaThenComplete(t *testing.T) {
	st := &streamState{}
	evs, _ := mapNotification("item/agentMessage/delta", json.RawMessage(`{"itemId":"m1","text":"he"}`), st)
	if len(evs) != 1 || evs[0].Type != harnessproto.TypeText || evs[0].ItemID != "m1" {
		t.Fatalf("delta → %+v", evs)
	}
	// The completion for a streamed item closes the group with empty text (no dup).
	evs, _ = mapNotification("item/completed", json.RawMessage(`{"item":{"id":"m1","type":"agentMessage","text":"hello"}}`), st)
	if len(evs) != 1 || evs[0].Type != harnessproto.TypeText {
		t.Fatalf("completion → %+v", evs)
	}
	var p struct {
		Text  string `json:"text"`
		Final bool   `json:"final"`
	}
	_ = json.Unmarshal(evs[0].Payload, &p)
	if p.Text != "" || !p.Final {
		t.Fatalf("streamed completion should be empty+final, got %+v", p)
	}
}

func TestMapAgentMessageCompleteWithoutDelta(t *testing.T) {
	st := &streamState{}
	evs, _ := mapNotification("item/completed", json.RawMessage(`{"item":{"id":"m2","type":"agentMessage","text":"whole"}}`), st)
	var p struct {
		Text  string `json:"text"`
		Final bool   `json:"final"`
	}
	_ = json.Unmarshal(evs[0].Payload, &p)
	if p.Text != "whole" || !p.Final {
		t.Fatalf("un-streamed completion should carry text, got %+v", p)
	}
}

func TestMapCommandExecution(t *testing.T) {
	st := &streamState{}
	evs, _ := mapNotification("item/started", json.RawMessage(`{"item":{"id":"c1","type":"commandExecution","command":"ls"}}`), st)
	if evs[0].Type != harnessproto.TypeToolCall {
		t.Fatalf("started command → %+v", evs)
	}
	evs, _ = mapNotification("item/completed", json.RawMessage(`{"item":{"id":"c1","type":"commandExecution","status":"completed","output":"a\nb"}}`), st)
	if evs[0].Type != harnessproto.TypeToolResult {
		t.Fatalf("completed command → %+v", evs)
	}
}

func TestMapUserMessageDropped(t *testing.T) {
	evs, _ := mapNotification("item/completed", json.RawMessage(`{"item":{"id":"u1","type":"userMessage"}}`), &streamState{})
	if len(evs) != 0 {
		t.Fatalf("userMessage should be dropped, got %+v", evs)
	}
}

func TestMapUnknownIsRawNeverDropped(t *testing.T) {
	evs, res := mapOne("some/futureNotification", `{"a":1}`)
	if res != nil || len(evs) != 1 || evs[0].Type != harnessproto.TypeRaw {
		t.Fatalf("unknown → %+v res=%v", evs, res)
	}
	var p struct {
		Runtime    string          `json:"runtime"`
		NativeType string          `json:"native_type"`
		Body       json.RawMessage `json:"body"`
	}
	_ = json.Unmarshal(evs[0].Payload, &p)
	if p.Runtime != harnessproto.RuntimeCodex || p.NativeType != "some/futureNotification" {
		t.Fatalf("raw payload = %+v", p)
	}
}

func TestMapPlan(t *testing.T) {
	evs, _ := mapOne("turn/plan/updated", `{"plan":[{"step":"a","status":"completed"},{"step":"b","status":"inProgress"}]}`)
	if evs[0].Type != harnessproto.TypePlan {
		t.Fatalf("plan → %+v", evs)
	}
	var p struct {
		Items []struct{ Text, Status string } `json:"items"`
	}
	_ = json.Unmarshal(evs[0].Payload, &p)
	if len(p.Items) != 2 || p.Items[0].Status != "completed" || p.Items[1].Status != "in_progress" {
		t.Fatalf("plan items = %+v", p.Items)
	}
}
