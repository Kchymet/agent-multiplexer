package runtimeevents

import (
	"encoding/json"
	"testing"
)

// mapCodex is the Codex counterpart of mapOne: it maps one rollout line and
// decodes each event's payload so a test can assert on it directly.
func mapCodex(t *testing.T, st *CodexState, line string) []struct {
	Type    string
	ItemID  string
	Payload map[string]any
} {
	t.Helper()
	if st == nil {
		st = &CodexState{}
	}
	evs := MapCodexLine([]byte(line), st)
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

func TestCodexUserPromptAndInjectedContext(t *testing.T) {
	got := mapCodex(t, nil, `{"type":"response_item","payload":{"type":"message","role":"user",`+
		`"content":[{"type":"input_text","text":"fix the bug"}],`+
		`"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}}`)
	if len(got) != 1 || got[0].Type != TypePrompt || got[0].Payload["text"] != "fix the bug" {
		t.Fatalf("prompt mapping = %+v, want one prompt", got)
	}

	// The environment block Codex injects as a user turn must NOT read as a
	// prompt the human typed; it passes through as raw.
	ctx := mapCodex(t, nil, `{"type":"response_item","payload":{"type":"message","role":"user",`+
		`"content":[{"type":"input_text","text":"<environment_context><cwd>/w</cwd></environment_context>"}],`+
		`"internal_chat_message_metadata_passthrough":{"content_item_kinds":["environments.environment_context"]}}}`)
	if len(ctx) != 1 || ctx[0].Type != TypeRaw || ctx[0].Payload["native_type"] != "response_item/context" {
		t.Fatalf("injected context = %+v, want raw(response_item/context)", ctx)
	}

	// A user message from a rollout predating the kind tagging is still a prompt.
	untagged := mapCodex(t, nil, `{"type":"response_item","payload":{"type":"message","role":"user",`+
		`"content":[{"type":"input_text","text":"hi"}]}}`)
	if len(untagged) != 1 || untagged[0].Type != TypePrompt {
		t.Fatalf("untagged user message = %+v, want prompt", untagged)
	}
}

func TestCodexAssistantTextAndReasoning(t *testing.T) {
	txt := mapCodex(t, nil, `{"type":"response_item","payload":{"type":"message","id":"msg_1",`+
		`"role":"assistant","content":[{"type":"output_text","text":"done"}]}}`)
	if len(txt) != 1 || txt[0].Type != TypeText || txt[0].ItemID != "msg_1" {
		t.Fatalf("assistant message = %+v, want text(msg_1)", txt)
	}
	if txt[0].Payload["text"] != "done" || txt[0].Payload["final"] != true {
		t.Fatalf("text payload = %+v", txt[0].Payload)
	}

	rsn := mapCodex(t, nil, `{"type":"response_item","payload":{"type":"reasoning","id":"rs_1",`+
		`"summary":[{"type":"summary_text","text":"weighing options"}]}}`)
	if len(rsn) != 1 || rsn[0].Type != TypeThinking || rsn[0].Payload["text"] != "weighing options" {
		t.Fatalf("reasoning = %+v, want thinking", rsn)
	}

	// An encrypted-only reasoning item carries nothing to show.
	empty := mapCodex(t, nil, `{"type":"response_item","payload":{"type":"reasoning","id":"rs_2",`+
		`"summary":[],"encrypted_content":"abc"}}`)
	if len(empty) != 0 {
		t.Fatalf("contentless reasoning = %+v, want nothing", empty)
	}
}

func TestCodexToolCallAndResult(t *testing.T) {
	st := &CodexState{}
	call := mapCodex(t, st, `{"type":"response_item","payload":{"type":"function_call","id":"fc_1",`+
		`"call_id":"call_1","name":"exec_command","arguments":"{\"cmd\": \"cat greeting.txt\"}"}}`)
	if len(call) != 1 || call[0].Type != TypeToolCall || call[0].ItemID != "call_1" {
		t.Fatalf("function_call = %+v, want tool_call(call_1)", call)
	}
	if call[0].Payload["kind"] != "execute" || call[0].Payload["title"] != "exec_command" {
		t.Fatalf("tool_call payload = %+v", call[0].Payload)
	}
	// arguments arrive as a JSON string; the summary reads through it.
	if call[0].Payload["input"] != "cat greeting.txt" {
		t.Fatalf("tool_call input = %v, want the command", call[0].Payload["input"])
	}

	res := mapCodex(t, st, `{"type":"response_item","payload":{"type":"function_call_output",`+
		`"call_id":"call_1","output":"Process exited with code 0\nOutput:\nhello\n"}}`)
	if len(res) != 1 || res[0].Type != TypeToolResult || res[0].ItemID != "call_1" {
		t.Fatalf("function_call_output = %+v, want tool_result(call_1)", res)
	}
	if res[0].Payload["status"] != "success" {
		t.Fatalf("status = %v, want success", res[0].Payload["status"])
	}
	// The payload keeps the Claude reader's shape, diffs included.
	if _, ok := res[0].Payload["diffs"].([]any); !ok {
		t.Fatalf("tool_result must carry a diffs list, got %+v", res[0].Payload)
	}
}

func TestCodexToolResultFailureStatus(t *testing.T) {
	// A non-zero exit in the string output is the failure signal.
	bad := mapCodex(t, nil, `{"type":"response_item","payload":{"type":"function_call_output",`+
		`"call_id":"c2","output":"Process exited with code 2\nOutput:\nboom\n"}}`)
	if bad[0].Payload["status"] != "error" {
		t.Fatalf("non-zero exit status = %v, want error", bad[0].Payload["status"])
	}
	// The older wrapped shape reports it as metadata.exit_code instead.
	wrapped := mapCodex(t, nil, `{"type":"response_item","payload":{"type":"function_call_output",`+
		`"call_id":"c3","output":{"output":"boom","metadata":{"exit_code":1}}}}`)
	if wrapped[0].Payload["status"] != "error" || wrapped[0].Payload["output"] != "boom" {
		t.Fatalf("wrapped output = %+v", wrapped[0].Payload)
	}
}

func TestCodexUpdatePlanToPlan(t *testing.T) {
	got := mapCodex(t, nil, `{"type":"response_item","payload":{"type":"function_call","call_id":"c9",`+
		`"name":"update_plan","arguments":"{\"plan\":[{\"step\":\"read the file\",\"status\":\"in_progress\"}]}"}}`)
	if len(got) != 1 || got[0].Type != TypePlan {
		t.Fatalf("update_plan = %+v, want plan", got)
	}
	items, _ := got[0].Payload["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("plan items = %v, want 1", got[0].Payload["items"])
	}
}

func TestCodexTurnBoundariesAndUsage(t *testing.T) {
	start := mapCodex(t, nil, `{"type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`)
	if len(start) != 1 || start[0].Type != TypeTurnStart {
		t.Fatalf("task_started = %+v, want turn_start", start)
	}
	end := mapCodex(t, nil, `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"t1"}}`)
	if len(end) != 1 || end[0].Type != TypeTurnEnd || end[0].Payload["stop_reason"] != "end_turn" {
		t.Fatalf("task_complete = %+v, want turn_end/end_turn", end)
	}
	abort := mapCodex(t, nil, `{"type":"event_msg","payload":{"type":"turn_aborted","reason":"interrupted"}}`)
	if abort[0].Type != TypeTurnEnd || abort[0].Payload["stop_reason"] != "interrupted" {
		t.Fatalf("turn_aborted = %+v", abort[0])
	}

	usage := mapCodex(t, nil, `{"type":"event_msg","payload":{"type":"token_count","info":{`+
		`"last_token_usage":{"total_tokens":1242},"total_token_usage":{"total_tokens":9999},`+
		`"model_context_window":258400}}}`)
	if len(usage) != 1 || usage[0].Type != TypeUsage {
		t.Fatalf("token_count = %+v, want usage", usage)
	}
	if used, _ := usage[0].Payload["used"].(float64); used != 1242 {
		t.Fatalf("usage used = %v, want the turn's own 1242", usage[0].Payload["used"])
	}
	if size, _ := usage[0].Payload["size"].(float64); size != 258400 {
		t.Fatalf("usage size = %v, want the context window", usage[0].Payload["size"])
	}
}

// TestCodexApprovalToPermissionRequest covers the approval prompts, which only
// the interactive Codex TUI can raise (`codex exec` pins approvals to never), so
// they are exercised from hand-written records rather than the captured fixture.
func TestCodexApprovalToPermissionRequest(t *testing.T) {
	exec := mapCodex(t, nil, `{"type":"event_msg","payload":{"type":"exec_approval_request",`+
		`"call_id":"call_7","command":["rm","-rf","build"],"cwd":"/w"}}`)
	if len(exec) != 1 || exec[0].Type != TypePermissionRequest || exec[0].ItemID != "call_7" {
		t.Fatalf("exec_approval_request = %+v, want permission_request(call_7)", exec)
	}
	if exec[0].Payload["action"] != "rm -rf build" || exec[0].Payload["tool"] != "exec_command" {
		t.Fatalf("permission payload = %+v", exec[0].Payload)
	}
	if opts, _ := exec[0].Payload["options"].([]any); len(opts) == 0 {
		t.Fatalf("permission_request must offer options, got %+v", exec[0].Payload)
	}

	patch := mapCodex(t, nil, `{"type":"event_msg","payload":{"type":"apply_patch_approval_request","call_id":"c8"}}`)
	if patch[0].Type != TypePermissionRequest || patch[0].Payload["tool"] != "apply_patch" {
		t.Fatalf("apply_patch_approval_request = %+v", patch[0])
	}
}

func TestCodexNoticesAndDuplicateElision(t *testing.T) {
	meta := mapCodex(t, nil, `{"type":"session_meta","payload":{"cli_version":"0.153.2","originator":"codex_exec"}}`)
	if len(meta) != 1 || meta[0].Type != TypeNotice {
		t.Fatalf("session_meta = %+v, want notice", meta)
	}
	if got, want := meta[0].Payload["text"], "codex session start · 0.153.2 (codex_exec)"; got != want {
		t.Fatalf("session notice = %q, want %q", got, want)
	}

	errEv := mapCodex(t, nil, `{"type":"event_msg","payload":{"type":"stream_error","message":"upstream 429"}}`)
	if errEv[0].Type != TypeNotice || errEv[0].Payload["level"] != "error" || errEv[0].Payload["text"] != "upstream 429" {
		t.Fatalf("stream_error = %+v", errEv[0])
	}

	// event_msg records that restate a response_item are elided, not duplicated.
	for _, dup := range []string{"item_completed", "agent_message", "exec_command_end", "agent_reasoning_delta"} {
		line := `{"type":"event_msg","payload":{"type":"` + dup + `"}}`
		if evs := MapCodexLine([]byte(line), &CodexState{}); len(evs) != 0 {
			t.Fatalf("%s should be elided as a duplicate, got %+v", dup, evs)
		}
	}
	// token_usage_record likewise restates the token_count event.
	if evs := MapCodexLine([]byte(`{"type":"token_usage_record","payload":{}}`), &CodexState{}); len(evs) != 0 {
		t.Fatalf("token_usage_record should be elided, got %+v", evs)
	}
}

func TestCodexUnknownAndUnparsablePassthrough(t *testing.T) {
	// An unmodeled record kind → raw, never dropped.
	got := mapCodex(t, nil, `{"type":"world_state","payload":{"full":true}}`)
	if len(got) != 1 || got[0].Type != TypeRaw || got[0].Payload["native_type"] != "world_state" {
		t.Fatalf("world_state = %+v, want raw(world_state)", got)
	}
	if got[0].Payload["runtime"] != "codex" {
		t.Fatalf("raw runtime = %v, want codex", got[0].Payload["runtime"])
	}
	// An unmodeled event_msg subtype → raw too (only the known duplicates elide).
	ev := mapCodex(t, nil, `{"type":"event_msg","payload":{"type":"entered_review_mode"}}`)
	if len(ev) != 1 || ev[0].Type != TypeRaw || ev[0].Payload["native_type"] != "event_msg/entered_review_mode" {
		t.Fatalf("unknown event_msg = %+v", ev)
	}
	// Invalid JSON → raw(unparsable).
	bad := mapCodex(t, nil, `{not json`)
	if len(bad) != 1 || bad[0].Type != TypeRaw || bad[0].Payload["native_type"] != "unparsable" {
		t.Fatalf("unparsable = %+v", bad)
	}
	// Blank line → nothing.
	if evs := MapCodexLine([]byte("   \n"), &CodexState{}); len(evs) != 0 {
		t.Fatalf("blank line should map to nothing, got %+v", evs)
	}
}
