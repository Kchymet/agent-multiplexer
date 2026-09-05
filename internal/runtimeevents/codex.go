package runtimeevents

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// codex.go maps Codex CLI's rollout JSONL onto the same vocabulary the Claude
// reader emits, so a Codex session's transcript renders identically downstream.
//
// A rollout line (codex-cli 0.153) is an envelope — {timestamp, ordinal, type,
// payload} — around one of a handful of record kinds. Two of them carry the
// conversation, and they overlap:
//
//   - `response_item` is the durable, model-facing item stream (the OpenAI
//     Responses API item shapes: message / reasoning / function_call /
//     function_call_output). This is the authority for content.
//   - `event_msg` is Codex's UI-facing event stream. Most of it restates a
//     response_item (`item_completed`, `agent_message`, `exec_command_end`, …);
//     the rest carries what no response_item does — turn boundaries, token
//     counts, approval prompts, errors.
//
// So we map response_item for content and event_msg only for the gaps, and elide
// the event_msg subtypes that are known restatements (codexEventDuplicates).
// That elision is deliberate de-duplication of *known* records; the
// never-drop rule still holds for everything unrecognized, which becomes `raw`.
const codexRuntimeName = harnessproto.RuntimeCodex

// CodexState carries decode state across lines of one session's rollout: the
// open function_call inputs, so a later function_call_output can name the tool it
// belongs to. It is NOT safe for concurrent use — one per session tail.
type CodexState struct {
	calls map[string]toolInfo // call_id -> {name,input}
	// approvals are the call_ids of approval prompts announced and not yet
	// resolved, oldest first. Codex records the prompt but not the answer, so the
	// resolution is inferred from what happens next to the same call (below), and
	// this is what makes a `permission_request` retirable rather than open forever.
	approvals []string
}

func (st *CodexState) ensure() {
	if st.calls == nil {
		st.calls = map[string]toolInfo{}
	}
}

// openApproval records that an approval prompt for callID is waiting.
func (st *CodexState) openApproval(callID string) {
	if callID == "" {
		return
	}
	st.approvals = append(st.approvals, callID)
}

// resolveApproval closes the approval waiting on callID, if one is, and returns
// the event retiring it. ok=false when that call never prompted — the common
// case, since most calls run without asking.
func (st *CodexState) resolveApproval(callID, decision string) (harnessproto.RuntimeEvent, bool) {
	for i, id := range st.approvals {
		if id == callID {
			st.approvals = append(st.approvals[:i], st.approvals[i+1:]...)
			return permissionResolved(id, decision), true
		}
	}
	return harnessproto.RuntimeEvent{}, false
}

// clearApprovals retires every approval still open, for a turn boundary that
// proves no prompt survived it. The decision is DecisionCleared: the rollout says
// the turn ended, never how the human answered.
func (st *CodexState) clearApprovals() []harnessproto.RuntimeEvent {
	if len(st.approvals) == 0 {
		return nil
	}
	out := make([]harnessproto.RuntimeEvent, 0, len(st.approvals))
	for _, id := range st.approvals {
		out = append(out, permissionResolved(id, harnessproto.DecisionCleared))
	}
	st.approvals = nil
	return out
}

// codexLine is the outer rollout envelope; only the fields we branch on are
// decoded.
type codexLine struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// MapCodexLine decodes one line of a Codex rollout JSONL into zero or more
// generic structured events. A line it cannot decode as JSON, or whose record
// type has no mapping, becomes a single `raw` event — never dropped
// (docs/remote-provider-sessions.md §4).
func MapCodexLine(line []byte, st *CodexState) []harnessproto.RuntimeEvent {
	st.ensure()
	if strings.TrimSpace(string(line)) == "" {
		return nil
	}
	var l codexLine
	if err := json.Unmarshal(line, &l); err != nil || l.Type == "" {
		return []harnessproto.RuntimeEvent{codexRaw("unparsable", json.RawMessage(line))}
	}
	switch l.Type {
	case "response_item":
		return mapCodexItem(l.Payload, st)
	case "event_msg":
		return mapCodexEvent(l.Payload, st)
	case "session_meta":
		return []harnessproto.RuntimeEvent{notice("info", codexSessionText(l.Payload))}
	case "compacted":
		return []harnessproto.RuntimeEvent{notice("info", "context compacted")}
	case "token_usage_record":
		// Per-response bookkeeping; the `token_count` event_msg carries the same
		// numbers and is what we map to `usage`.
		return nil
	default:
		// turn_context, world_state, and any record kind a later Codex adds.
		return []harnessproto.RuntimeEvent{codexRaw(l.Type, json.RawMessage(line))}
	}
}

// ── response_item: the conversation ─────────────────────────────────────────

// codexItem is a rollout response_item payload — the union of the item shapes we
// map, decoded leniently so an unknown field never costs us the ones we need.
type codexItem struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // message: [{type,text}]; reasoning: [{type,text}]
	Summary json.RawMessage `json:"summary"` // reasoning: [{type:summary_text,text}]

	// function_call / custom_tool_call / local_shell_call
	Name      string          `json:"name"`
	CallID    string          `json:"call_id"`
	Arguments json.RawMessage `json:"arguments"` // JSON *string* holding the call's JSON args
	Input     json.RawMessage `json:"input"`     // custom_tool_call / local_shell_call action
	Action    json.RawMessage `json:"action"`

	// function_call_output / custom_tool_call_output
	Output json.RawMessage `json:"output"` // string, or {output, metadata:{exit_code}}

	Meta struct {
		Kinds []string `json:"content_item_kinds"`
	} `json:"internal_chat_message_metadata_passthrough"`
}

func mapCodexItem(payload json.RawMessage, st *CodexState) []harnessproto.RuntimeEvent {
	var it codexItem
	if json.Unmarshal(payload, &it) != nil || it.Type == "" {
		return []harnessproto.RuntimeEvent{codexRaw("response_item", payload)}
	}
	switch it.Type {
	case "message":
		return mapCodexMessage(it, payload)
	case "reasoning":
		text := extractText(it.Summary)
		if text == "" {
			text = extractText(it.Content)
		}
		if text == "" {
			return nil // an encrypted-only reasoning item has nothing to show
		}
		return []harnessproto.RuntimeEvent{{
			Type: TypeThinking, ItemID: it.ID, Direction: dirOut,
			Payload: mustMarshal(map[string]any{"text": text}),
		}}
	case "function_call", "custom_tool_call", "local_shell_call":
		return mapCodexCall(it, st)
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
		return mapCodexCallOutput(it, st)
	case "web_search_call":
		return []harnessproto.RuntimeEvent{{
			Type: TypeToolCall, ItemID: it.ID, Direction: dirOut,
			Payload: mustMarshal(map[string]any{
				"item_id":   it.ID,
				"title":     "web_search",
				"kind":      "fetch",
				"status":    "completed",
				"input":     codexSummarizeInput(it.Action),
				"raw_input": rawOrNull(it.Action),
			}),
		}}
	default:
		return []harnessproto.RuntimeEvent{codexRaw("response_item/"+it.Type, payload)}
	}
}

// mapCodexMessage splits the message stream three ways. An assistant message is
// `text`. A user message is the human prompt — except for the context Codex
// injects as a user turn (the environment block, user instructions, …), which
// `content_item_kinds` marks with a namespaced kind rather than `user.text`;
// those pass through as `raw` so they never render as something the user typed.
// A developer/system message is instruction preamble, likewise `raw`.
func mapCodexMessage(it codexItem, payload json.RawMessage) []harnessproto.RuntimeEvent {
	text := extractText(it.Content)
	switch it.Role {
	case "assistant":
		return []harnessproto.RuntimeEvent{{
			Type: TypeText, ItemID: it.ID, Direction: dirOut,
			Payload: mustMarshal(map[string]any{"text": text, "final": true}),
		}}
	case "user":
		if !codexUserAuthored(it.Meta.Kinds) {
			return []harnessproto.RuntimeEvent{codexRaw("response_item/context", payload)}
		}
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []harnessproto.RuntimeEvent{{
			Type: TypePrompt, Direction: dirIn,
			Payload: mustMarshal(map[string]any{"text": text}),
		}}
	default:
		return []harnessproto.RuntimeEvent{codexRaw("response_item/message/"+it.Role, payload)}
	}
}

// codexUserAuthored reports whether a user-role message is something the human
// typed. Codex tags every content item with a kind: `user.text` for a real
// prompt, a namespaced one (e.g. `environments.environment_context`) for injected
// context. An item with no kinds at all predates the tagging, so we take it as
// user-authored rather than hiding a real prompt.
func codexUserAuthored(kinds []string) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if k == "user.text" {
			return true
		}
	}
	return false
}

// mapCodexCall maps a tool invocation. `update_plan` becomes a `plan` event —
// the same treatment the Claude reader gives `TodoWrite`, so a plan renders as a
// plan on either runtime rather than as an opaque tool call.
func mapCodexCall(it codexItem, st *CodexState) []harnessproto.RuntimeEvent {
	args := codexCallArgs(it)
	id := it.CallID
	if id == "" {
		id = it.ID
	}
	if id != "" {
		st.calls[id] = toolInfo{name: it.Name, input: args}
	}
	if it.Name == "update_plan" {
		return []harnessproto.RuntimeEvent{codexPlanEvent(args)}
	}
	return []harnessproto.RuntimeEvent{{
		Type: TypeToolCall, ItemID: id, Direction: dirOut,
		Payload: mustMarshal(map[string]any{
			"item_id":   id,
			"title":     it.Name,
			"kind":      kindForCodexTool(it.Name),
			"status":    "in_progress",
			"input":     codexSummarizeInput(args),
			"raw_input": rawOrNull(args),
		}),
	}}
}

func mapCodexCallOutput(it codexItem, st *CodexState) []harnessproto.RuntimeEvent {
	id := it.CallID
	if id == "" {
		id = it.ID
	}
	delete(st.calls, id)
	// An approval still open when the call reports its output means the tool never
	// started: the prompt was declined. Retire the request with that answer before
	// the result, so a consumer never sees a result for a request it still shows as
	// waiting.
	var out []harnessproto.RuntimeEvent
	if ev, ok := st.resolveApproval(id, harnessproto.DecisionDeny); ok {
		out = append(out, ev)
	}
	text, status := codexOutputText(it.Output)
	return append(out, harnessproto.RuntimeEvent{
		Type: TypeToolResult, ItemID: id, Direction: dirOut,
		Payload: mustMarshal(map[string]any{
			"item_id": id,
			"status":  status,
			"output":  text,
			// Codex records a tool's effect only as the tool's own output — it has
			// no per-call before/after the way Claude's Edit/Write inputs do — so the
			// shape is kept and left empty rather than guessed at.
			"diffs":      []map[string]any{},
			"raw_output": rawOrNull(it.Output),
		}),
	})
}

// codexCallArgs normalizes a call's arguments to raw JSON. `function_call`
// carries them as a JSON *string*; the custom/local-shell shapes carry an object
// directly.
func codexCallArgs(it codexItem) json.RawMessage {
	for _, v := range []json.RawMessage{it.Arguments, it.Input, it.Action} {
		if len(v) == 0 || string(v) == "null" {
			continue
		}
		var s string
		if json.Unmarshal(v, &s) == nil {
			if json.Valid([]byte(s)) {
				return json.RawMessage(s)
			}
			return mustMarshal(map[string]any{"input": s})
		}
		return v
	}
	return nil
}

// codexOutputText flattens a tool output to display text plus a status. Codex
// 0.153 writes a plain string whose preamble reports the exit code; older
// rollouts wrap it as {output, metadata:{exit_code}}. Both are read.
func codexOutputText(v json.RawMessage) (string, string) {
	if len(v) == 0 || string(v) == "null" {
		return "", "success"
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s, codexStatusFromText(s)
	}
	var wrapped struct {
		Output   string `json:"output"`
		Success  *bool  `json:"success"`
		Metadata struct {
			ExitCode *int `json:"exit_code"`
		} `json:"metadata"`
	}
	if json.Unmarshal(v, &wrapped) == nil {
		status := codexStatusFromText(wrapped.Output)
		if wrapped.Metadata.ExitCode != nil && *wrapped.Metadata.ExitCode != 0 {
			status = "error"
		}
		if wrapped.Success != nil && !*wrapped.Success {
			status = "error"
		}
		return wrapped.Output, status
	}
	return extractText(v), "success"
}

// codexExitedPrefix is the line Codex's exec output leads with; a non-zero code
// after it is the only in-band failure signal a string output carries.
const codexExitedPrefix = "Process exited with code "

func codexStatusFromText(s string) string {
	i := strings.Index(s, codexExitedPrefix)
	if i < 0 {
		return "success"
	}
	rest := s[i+len(codexExitedPrefix):]
	if end := strings.IndexAny(rest, "\r\n"); end >= 0 {
		rest = rest[:end]
	}
	if strings.TrimSpace(rest) == "0" {
		return "success"
	}
	return "error"
}

// codexPlanEvent maps `update_plan` arguments — {plan:[{step,status}]} — onto the
// same {items:[{text,status}]} payload the Claude reader emits for TodoWrite.
func codexPlanEvent(args json.RawMessage) harnessproto.RuntimeEvent {
	var p struct {
		Plan []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	_ = json.Unmarshal(args, &p)
	items := make([]map[string]any, 0, len(p.Plan))
	for _, s := range p.Plan {
		status := s.Status
		if status == "" {
			status = "pending"
		}
		items = append(items, map[string]any{"text": s.Step, "status": status})
	}
	return harnessproto.RuntimeEvent{Type: TypePlan, Direction: dirOut,
		Payload: mustMarshal(map[string]any{"items": items})}
}

// ── event_msg: turn boundaries, usage, approvals, errors ────────────────────

// codexEventDuplicates are the event_msg subtypes that restate a response_item
// we already mapped (streaming deltas, the item_* lifecycle, the begin/end pairs
// around a tool call). Mapping them too would double every message, every
// reasoning block and every command in the transcript, so they are elided here
// rather than passed through as `raw`. Everything NOT in this set and not mapped
// below still becomes `raw`.
//
// The `*_begin` records are intercepted by the switch before it reaches this map,
// because a tool starting is the only evidence Codex's rollout gives that an
// approval prompt for it was allowed. They still contribute nothing of their own:
// the interception emits a `permission_resolved` or nothing at all.
var codexEventDuplicates = map[string]bool{
	"item_started": true, "item_updated": true, "item_completed": true,
	"user_message": true, "agent_message": true, "agent_message_delta": true,
	"agent_message_content_delta": true,
	"agent_reasoning":             true, "agent_reasoning_delta": true,
	"agent_reasoning_raw_content": true, "agent_reasoning_raw_content_delta": true,
	"agent_reasoning_section_break": true,
	"reasoning_content_delta":       true, "reasoning_raw_content_delta": true,
	"plan_update": true, "plan_delta": true,
	"exec_command_begin": true, "exec_command_output_delta": true, "exec_command_end": true,
	"patch_apply_begin": true, "patch_apply_updated": true, "patch_apply_end": true,
	"mcp_tool_call_begin": true, "mcp_tool_call_end": true,
	"web_search_begin": true, "web_search_end": true,
	"image_generation_begin": true, "image_generation_end": true,
	"view_image_tool_call": true,
	"raw_response_item":    true, "raw_response_completed": true,
}

// codexEvent is a rollout event_msg payload; the union of the fields the mapped
// subtypes need.
type codexEvent struct {
	Type string `json:"type"`

	// error / stream_error / warning / deprecation_notice
	Message string `json:"message"`

	// turn_aborted
	Reason string `json:"reason"`

	// token_count
	Info struct {
		LastTokenUsage  *codexUsage `json:"last_token_usage"`
		TotalTokenUsage *codexUsage `json:"total_token_usage"`
		ContextWindow   int64       `json:"model_context_window"`
	} `json:"info"`

	// exec_approval_request / apply_patch_approval_request / request_user_input
	CallID   string   `json:"call_id"`
	Command  []string `json:"command"`
	Cwd      string   `json:"cwd"`
	Question string   `json:"question"`
	Prompt   string   `json:"prompt"`
}

type codexUsage struct {
	Total int64 `json:"total_tokens"`
}

func mapCodexEvent(payload json.RawMessage, st *CodexState) []harnessproto.RuntimeEvent {
	var e codexEvent
	if json.Unmarshal(payload, &e) != nil || e.Type == "" {
		return []harnessproto.RuntimeEvent{codexRaw("event_msg", payload)}
	}
	switch e.Type {
	case "task_started", "turn_started":
		return []harnessproto.RuntimeEvent{{
			Type: TypeTurnStart, Direction: dirOut, Payload: mustMarshal(map[string]any{}),
		}}
	case "task_complete", "turn_complete":
		// A turn cannot end with a prompt still up, so any approval still open was
		// answered while we weren't looking: retire it before the boundary.
		return append(st.clearApprovals(), codexTurnEnd("end_turn"))
	case "turn_aborted":
		reason := e.Reason
		if reason == "" {
			reason = "aborted"
		}
		return append(st.clearApprovals(), codexTurnEnd(reason))
	case "token_count":
		return []harnessproto.RuntimeEvent{codexUsageEvent(e)}
	case "error", "stream_error":
		return []harnessproto.RuntimeEvent{notice("error", codexNoticeText(e, "error"))}
	case "warning", "deprecation_notice":
		return []harnessproto.RuntimeEvent{notice("warn", codexNoticeText(e, "warning"))}
	case "exec_approval_request":
		st.openApproval(e.CallID)
		return []harnessproto.RuntimeEvent{codexPermission(e, "exec_command", strings.Join(e.Command, " "))}
	case "apply_patch_approval_request":
		st.openApproval(e.CallID)
		return []harnessproto.RuntimeEvent{codexPermission(e, "apply_patch", "apply patch")}
	case "request_user_input", "elicitation_request":
		action := e.Question
		if action == "" {
			action = e.Prompt
		}
		st.openApproval(e.CallID)
		return []harnessproto.RuntimeEvent{codexPermission(e, e.Type, action)}
	case "exec_command_begin", "patch_apply_begin", "mcp_tool_call_begin":
		// These restate a response_item and are elided like the rest of the
		// begin/end pairs — but the tool having *started* is the one signal Codex's
		// rollout gives that an approval prompt for it was allowed, so a pending one
		// is retired here.
		if ev, ok := st.resolveApproval(e.CallID, harnessproto.DecisionAllow); ok {
			return []harnessproto.RuntimeEvent{ev}
		}
		return nil
	default:
		if codexEventDuplicates[e.Type] {
			return nil
		}
		return []harnessproto.RuntimeEvent{codexRaw("event_msg/"+e.Type, payload)}
	}
}

func codexTurnEnd(reason string) harnessproto.RuntimeEvent {
	return harnessproto.RuntimeEvent{Type: TypeTurnEnd, Direction: dirOut,
		Payload: mustMarshal(map[string]any{"stop_reason": reason})}
}

// codexUsageEvent reports the turn's own token spend against the model's context
// window. Claude has no window figure on the record and reports size 0; Codex
// does, so `size` is populated here.
func codexUsageEvent(e codexEvent) harnessproto.RuntimeEvent {
	u := e.Info.LastTokenUsage
	if u == nil {
		u = e.Info.TotalTokenUsage
	}
	var used int64
	if u != nil {
		used = u.Total
	}
	return harnessproto.RuntimeEvent{Type: TypeUsage, Direction: dirMeta,
		Payload: mustMarshal(map[string]any{"used": used, "size": e.Info.ContextWindow})}
}

// codexPermission maps an approval prompt. Codex resolves these in its own TUI —
// amux is read-only here — so the event announces that the session is blocked
// and on what; the options list is Codex's own answer set.
func codexPermission(e codexEvent, tool, action string) harnessproto.RuntimeEvent {
	if action == "" {
		action = tool
	}
	return harnessproto.RuntimeEvent{
		Type: TypePermissionRequest, ItemID: e.CallID, Direction: dirOut,
		Payload: mustMarshal(map[string]any{
			"request_id": e.CallID,
			"tool":       tool,
			"action":     action,
			"options":    []string{"approve", "approve_for_session", "deny", "abort"},
		}),
	}
}

func codexNoticeText(e codexEvent, fallback string) string {
	if e.Message != "" {
		return e.Message
	}
	if e.Reason != "" {
		return e.Reason
	}
	return fallback
}

// ── small helpers ───────────────────────────────────────────────────────────

// codexSessionText labels the session-start notice with the CLI version that
// wrote the rollout, so a format drift is visible in the transcript itself.
func codexSessionText(payload json.RawMessage) string {
	var m struct {
		CLIVersion string `json:"cli_version"`
		Originator string `json:"originator"`
	}
	_ = json.Unmarshal(payload, &m)
	if m.CLIVersion == "" {
		return "codex session start"
	}
	if m.Originator == "" {
		return "codex session start · " + m.CLIVersion
	}
	return fmt.Sprintf("codex session start · %s (%s)", m.CLIVersion, m.Originator)
}

func codexRaw(nativeType string, body json.RawMessage) harnessproto.RuntimeEvent {
	return rawEventFor(codexRuntimeName, nativeType, body)
}

// kindForCodexTool classifies a Codex tool name into the vocabulary's kinds, the
// counterpart of kindForTool for Claude's tool names.
func kindForCodexTool(name string) string {
	switch name {
	case "exec_command", "shell", "local_shell", "unified_exec", "execve", "write_stdin", "kill_command":
		return "execute"
	case "apply_patch", "edit_file", "write_file", "create_file", "str_replace_editor":
		return "edit"
	case "read_file", "view_image":
		return "read"
	case "grep", "file_search", "tool_search", "list_dir":
		return "search"
	case "web_search", "fetch":
		return "fetch"
	default:
		return "other"
	}
}

// codexSummarizeInput picks the one field of a call's arguments worth showing in
// a collapsed transcript row, mirroring summarizeInput for Claude's tool inputs.
// `cmd` comes first: it is what Codex's own exec tool takes.
func codexSummarizeInput(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var generic map[string]any
	if err := json.Unmarshal(args, &generic); err != nil {
		return ""
	}
	for _, k := range []string{"cmd", "command", "file_path", "path", "pattern", "query", "url", "prompt", "input", "justification"} {
		v, ok := generic[k]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			if t != "" {
				return t
			}
		case []any:
			parts := make([]string, 0, len(t))
			for _, e := range t {
				if s, ok := e.(string); ok {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, " ")
			}
		}
	}
	return string(args)
}
