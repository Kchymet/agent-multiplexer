package runtimeevents

import (
	"encoding/json"
	"testing"
)

// mapLine is a test helper: fresh state per call unless st is passed.
func mapOne(t *testing.T, st *ClaudeState, line string) []struct {
	Type    string
	ItemID  string
	Payload map[string]any
} {
	t.Helper()
	if st == nil {
		st = &ClaudeState{}
	}
	evs := MapClaudeLine([]byte(line), st)
	out := make([]struct {
		Type    string
		ItemID  string
		Payload map[string]any
	}, len(evs))
	for i, e := range evs {
		var p map[string]any
		_ = json.Unmarshal(e.Payload, &p)
		out[i] = struct {
			Type    string
			ItemID  string
			Payload map[string]any
		}{e.Type, e.ItemID, p}
	}
	return out
}

func TestMapUserPrompt(t *testing.T) {
	got := mapOne(t, nil, `{"type":"user","message":{"role":"user","content":"fix the bug"}}`)
	if len(got) != 2 || got[0].Type != TypeTurnStart || got[1].Type != TypePrompt {
		t.Fatalf("prompt mapping = %+v, want [turn_start, prompt]", got)
	}
	if got[1].Payload["text"] != "fix the bug" {
		t.Fatalf("prompt text = %v", got[1].Payload["text"])
	}
}

func TestMapAssistantTextThinkingUsageTurnEnd(t *testing.T) {
	line := `{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[` +
		`{"type":"thinking","thinking":"hmm"},{"type":"text","text":"done"}],` +
		`"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}}`
	got := mapOne(t, nil, line)
	// thinking, text, usage, turn_end
	if len(got) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(got), got)
	}
	if got[0].Type != TypeThinking || got[1].Type != TypeText {
		t.Fatalf("blocks = %s,%s", got[0].Type, got[1].Type)
	}
	if got[1].ItemID != "msg_1:1" {
		t.Fatalf("text item_id = %q, want msg_1:1", got[1].ItemID)
	}
	if got[2].Type != TypeUsage {
		t.Fatalf("expected usage, got %s", got[2].Type)
	}
	if used, _ := got[2].Payload["used"].(float64); used != 15 {
		t.Fatalf("usage used = %v, want 15", got[2].Payload["used"])
	}
	if got[3].Type != TypeTurnEnd || got[3].Payload["stop_reason"] != "end_turn" {
		t.Fatalf("turn_end = %+v", got[3])
	}
}

func TestMapAssistantToolUseNoTurnEndOnToolStop(t *testing.T) {
	line := `{"type":"assistant","message":{"id":"m2","role":"assistant","content":[` +
		`{"type":"tool_use","id":"tool_9","name":"Edit","input":{"file_path":"/a.go","old_string":"x","new_string":"y"}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}}`
	st := &ClaudeState{}
	got := mapOne(t, st, line)
	// tool_call, usage — NO turn_end (stopped to run a tool)
	if len(got) != 2 || got[0].Type != TypeToolCall {
		t.Fatalf("got %+v, want [tool_call, usage]", got)
	}
	if got[0].ItemID != "tool_9" || got[0].Payload["kind"] != "edit" {
		t.Fatalf("tool_call = %+v", got[0])
	}
	for _, e := range got {
		if e.Type == TypeTurnEnd {
			t.Fatal("tool_use stop must not emit turn_end")
		}
	}

	// The following tool_result recovers the diff from the remembered tool input.
	res := mapOne(t, st, `{"type":"user","message":{"role":"user","content":[`+
		`{"type":"tool_result","tool_use_id":"tool_9","content":"ok"}]}}`)
	if len(res) != 1 || res[0].Type != TypeToolResult || res[0].ItemID != "tool_9" {
		t.Fatalf("tool_result = %+v", res)
	}
	diffs, _ := res[0].Payload["diffs"].([]any)
	if len(diffs) != 1 {
		t.Fatalf("expected 1 recovered diff, got %v", res[0].Payload["diffs"])
	}
}

func TestMapTodoWriteToPlan(t *testing.T) {
	line := `{"type":"assistant","message":{"id":"m3","content":[` +
		`{"type":"tool_use","id":"t1","name":"TodoWrite","input":{"todos":[{"content":"step","status":"pending"}]}}]}}`
	got := mapOne(t, nil, line)
	if got[0].Type != TypePlan {
		t.Fatalf("TodoWrite should map to plan, got %s", got[0].Type)
	}
}

func TestUnknownAndUnparsablePassthrough(t *testing.T) {
	// Unknown top-level type → raw, never dropped.
	got := mapOne(t, nil, `{"type":"file-history-snapshot","messageId":"x"}`)
	if len(got) != 1 || got[0].Type != TypeRaw {
		t.Fatalf("unknown type should map to raw, got %+v", got)
	}
	if got[0].Payload["native_type"] != "file-history-snapshot" {
		t.Fatalf("raw native_type = %v", got[0].Payload["native_type"])
	}
	// Invalid JSON → raw(unparsable).
	bad := mapOne(t, nil, `{not json`)
	if len(bad) != 1 || bad[0].Type != TypeRaw || bad[0].Payload["native_type"] != "unparsable" {
		t.Fatalf("unparsable should map to raw/unparsable, got %+v", bad)
	}
	// Blank line → nothing.
	if evs := MapClaudeLine([]byte("   \n"), &ClaudeState{}); len(evs) != 0 {
		t.Fatalf("blank line should map to nothing, got %+v", evs)
	}
}

func TestSystemAndSummaryNotice(t *testing.T) {
	got := mapOne(t, nil, `{"type":"system","subtype":"hook","level":"warn"}`)
	if got[0].Type != TypeNotice || got[0].Payload["level"] != "warn" {
		t.Fatalf("system = %+v", got[0])
	}
	sum := mapOne(t, nil, `{"type":"summary","summary":"did stuff"}`)
	if sum[0].Type != TypeNotice {
		t.Fatalf("summary = %+v", sum[0])
	}
}
