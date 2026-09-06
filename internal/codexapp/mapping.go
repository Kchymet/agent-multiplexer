package codexapp

import (
	"encoding/json"
	"fmt"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// mapping.go turns one App Server notification (method + params) into the shared
// harnessproto runtime-event vocabulary (docs/remote-provider-sessions.md §4) —
// the SAME events the on-disk rollout tailer (internal/runtimeevents/codex.go)
// produces, so a consumer sees one story whether a codex session runs in pty or
// structured control mode. It is pure aside from streamState (per-session decode
// state), so every branch is unit-tested, including the unknown→`raw` rule.
//
// App Server notification → event mapping (per https://learn.chatgpt.com/docs/app-server):
//
//	notification                                   | event(s)
//	-----------------------------------------------|------------------------------------------
//	thread/started {thread}                         | notice (session established; id captured by the supervisor)
//	turn/started                                    | (none — the supervisor brackets turn_start)
//	turn/completed {turn.status,error}              | notice(error) if failed (+ turn_end from the supervisor)
//	turn/plan/updated {plan[]}                      | plan
//	turn/diff/updated {diff}                        | raw (no turn-level-diff type)
//	item/agentMessage/delta {itemId,delta}          | text  (chunk, coalesced by itemId)
//	item/reasoning/{summaryTextDelta,textDelta} {itemId,delta} | thinking (chunk, coalesced by itemId)
//	item/started agentMessage|reasoning             | (none — deltas carry the text)
//	item/completed agentMessage {text}              | text (final; closes coalesced group)
//	item/completed reasoning {content|summary}      | thinking (final)
//	item/{started,completed} commandExecution       | tool_call (started) / tool_result (completed)
//	item/completed fileChange {changes[]}           | tool_call(edit) + tool_result(diffs)
//	item/{started,completed} mcpToolCall            | tool_call / tool_result
//	item/completed functionCallOutput {output}      | tool_result
//	item/completed userMessage                      | (none — echo of our own prompt)
//	anything else                                   | raw (§4 — never dropped)
//
// Server-initiated approval / user-input *requests* are NOT handled here — they
// arrive as JSON-RPC requests (with an id) and are handled in supervisor.onRequest.

// streamState carries per-session decode state across notifications: which item
// ids already streamed chunk text/thinking, so a whole-item completion closes the
// coalesced group rather than duplicating it. NOT concurrency-safe — the
// supervisor owns one and only the read loop touches it.
type streamState struct {
	streamed map[string]bool // item_id -> chunk text/thinking already emitted
}

func (st *streamState) ensure() {
	if st.streamed == nil {
		st.streamed = map[string]bool{}
	}
}

// turnResult is the decoded terminal turn signal; the supervisor uses it to
// unblock a pending Prompt and bracket turn_end.
type turnResult struct {
	TurnID     string
	StopReason string
	IsError    bool
}

// itemPayload is one App Server thread item (the `item` field of
// item/{started,completed}). Only fields across the documented item types are
// decoded.
type itemPayload struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Text    string          `json:"text"`    // agentMessage
	Summary string          `json:"summary"` // reasoning
	Content json.RawMessage `json:"content"` // reasoning (string) OR userMessage (array)
	Command string          `json:"command"` // commandExecution
	Status  string          `json:"status"`  // commandExecution / fileChange / mcpToolCall
	Output  string          `json:"output"`  // commandExecution / functionCallOutput
	Changes []struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
		Diff string `json:"diff"`
	} `json:"changes"` // fileChange
	Server    string          `json:"server"`    // mcpToolCall
	Tool      string          `json:"tool"`      // mcpToolCall
	Arguments json.RawMessage `json:"arguments"` // mcpToolCall
	Result    json.RawMessage `json:"result"`    // mcpToolCall
	Err       *struct {
		Message string `json:"message"`
	} `json:"error"` // mcpToolCall
}

// mapNotification decodes one App Server notification into events. It returns
// res!=nil when the notification terminates a turn (turn/completed), which the
// supervisor uses to bracket turn_end. An unknown method (or undecodable params)
// becomes a single `raw` event — never dropped (§4). Capturing the thread id from
// thread/started is the supervisor's job (onNotify), not this pure mapper's.
func mapNotification(method string, params json.RawMessage, st *streamState) (events []harnessproto.RuntimeEvent, res *turnResult) {
	st.ensure()
	switch method {
	case "thread/started":
		return []harnessproto.RuntimeEvent{notice("info", "session established")}, nil

	case "turn/started":
		// Emit turn_start from the OBSERVED notification (any origin), not from the
		// supervisor's own Prompt — so a turn a native TUI started is bracketed too.
		return []harnessproto.RuntimeEvent{turnStartEvent()}, nil

	case "turn/completed":
		var p struct {
			Turn struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Error  *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"turn"`
		}
		_ = json.Unmarshal(params, &p)
		stop := p.Turn.Status
		if stop == "" {
			stop = "completed"
		}
		isErr := p.Turn.Status == "failed"
		var evs []harnessproto.RuntimeEvent
		if isErr {
			msg := "turn failed"
			if p.Turn.Error != nil && p.Turn.Error.Message != "" {
				msg = p.Turn.Error.Message
			}
			evs = append(evs, notice("error", msg))
		}
		// turn_end is emitted here (observed), bracketing every turn regardless of
		// which client started it.
		evs = append(evs, turnEndEvent(stop))
		return evs, &turnResult{TurnID: p.Turn.ID, StopReason: stop, IsError: isErr}

	case "turn/plan/updated":
		return []harnessproto.RuntimeEvent{planFromTurn(params)}, nil

	case "item/agentMessage/delta":
		return []harnessproto.RuntimeEvent{deltaChunk(harnessproto.TypeText, params, st)}, nil

	case "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
		return []harnessproto.RuntimeEvent{deltaChunk(harnessproto.TypeThinking, params, st)}, nil

	case "item/started":
		return mapItem("item/started", params, st), nil

	case "item/completed":
		return mapItem("item/completed", params, st), nil

	case "turn/diff/updated", "item/plan/delta", "item/commandExecution/outputDelta":
		return []harnessproto.RuntimeEvent{rawEvent(method, params)}, nil

	default:
		return []harnessproto.RuntimeEvent{rawEvent(method, params)}, nil
	}
}

// ── item events ─────────────────────────────────────────────────────────────

func mapItem(phase string, params json.RawMessage, st *streamState) []harnessproto.RuntimeEvent {
	var wrap struct {
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(params, &wrap); err != nil || len(wrap.Item) == 0 {
		return []harnessproto.RuntimeEvent{rawEvent(phase, params)}
	}
	var it itemPayload
	if err := json.Unmarshal(wrap.Item, &it); err != nil || it.Type == "" {
		return []harnessproto.RuntimeEvent{rawEvent(phase+"/item", wrap.Item)}
	}
	completed := phase == "item/completed"
	switch it.Type {
	case "userMessage":
		return nil // echo of the prompt we sent
	case "agentMessage":
		if !completed {
			return nil // deltas carry the streaming text
		}
		return []harnessproto.RuntimeEvent{closeOrEmitText(harnessproto.TypeText, it.ID, it.Text, true, st)}
	case "reasoning":
		if !completed {
			return nil
		}
		txt := it.Summary
		var s string
		if err := json.Unmarshal(it.Content, &s); err == nil && s != "" {
			txt = s
		}
		return []harnessproto.RuntimeEvent{closeOrEmitText(harnessproto.TypeThinking, it.ID, txt, true, st)}
	case "commandExecution":
		if completed {
			return []harnessproto.RuntimeEvent{toolResult(it.ID, statusFor(it.Status), it.Output, nil, rawOrNull(wrap.Item))}
		}
		return []harnessproto.RuntimeEvent{toolCall(it.ID, it.Command, "execute", it.Command)}
	case "mcpToolCall":
		if completed {
			out := ""
			if it.Err != nil {
				out = it.Err.Message
			}
			return []harnessproto.RuntimeEvent{toolResult(it.ID, statusFor(it.Status), out, nil, rawOrNull(it.Result))}
		}
		return []harnessproto.RuntimeEvent{toolCall(it.ID, it.Server+"/"+it.Tool, "other", string(it.Arguments))}
	case "fileChange":
		diffs := make([]map[string]any, 0, len(it.Changes))
		for _, c := range it.Changes {
			var newSide any
			if c.Diff != "" {
				newSide = c.Diff
			}
			diffs = append(diffs, map[string]any{"path": c.Path, "kind": c.Kind, "old": nil, "new": newSide})
		}
		call := toolCall(it.ID, "apply_patch", "edit", fmt.Sprintf("%d file(s)", len(it.Changes)))
		if !completed {
			return []harnessproto.RuntimeEvent{call}
		}
		return []harnessproto.RuntimeEvent{call, toolResult(it.ID, statusFor(it.Status), "", diffs, rawOrNull(wrap.Item))}
	case "functionCallOutput":
		if !completed {
			return nil
		}
		return []harnessproto.RuntimeEvent{toolResult(it.ID, "success", it.Output, nil, rawOrNull(wrap.Item))}
	default:
		return []harnessproto.RuntimeEvent{rawEvent("item/"+it.Type, wrap.Item)}
	}
}

// deltaChunk emits one streaming text/thinking chunk from a delta notification,
// marking the item id so the eventual item/completed closes the coalesced group.
//
// The streamed text field is `delta` (pinned Codex 0.153.4: AgentMessageDeltaNotification /
// ReasoningTextDeltaNotification / ReasoningSummaryTextDeltaNotification all carry
// {delta,itemId,threadId,turnId}). This mapper previously read `text`, which is absent on
// these notifications, so streamed assistant text was silently DROPPED against the real
// binary — only item/completed carried the whole text. Because the user-facing provider
// bridge consumes THIS supervisor mapper, that dropped real remote-session content (harness
// PR #138 fixed the parallel copy; this is the companion fix here).
func deltaChunk(typ string, params json.RawMessage, st *streamState) harnessproto.RuntimeEvent {
	var p struct {
		ItemID string `json:"itemId"`
		Delta  string `json:"delta"`
	}
	_ = json.Unmarshal(params, &p)
	st.streamed[p.ItemID] = true
	payload := map[string]any{"text": p.Delta}
	if typ == harnessproto.TypeText {
		payload["final"] = false
	}
	return harnessproto.RuntimeEvent{Type: typ, ItemID: p.ItemID, Direction: harnessproto.DirOut, Payload: mustMarshal(payload)}
}

// closeOrEmitText emits a whole-item agentMessage/reasoning completion,
// coalescing by item id: if chunks already streamed for this id it closes the
// group with empty text (final:true) rather than duplicating; otherwise it
// carries the full text.
func closeOrEmitText(typ, itemID, text string, completed bool, st *streamState) harnessproto.RuntimeEvent {
	if completed && st.streamed[itemID] {
		text = ""
	}
	payload := map[string]any{"text": text}
	if typ == harnessproto.TypeText {
		payload["final"] = completed
	}
	return harnessproto.RuntimeEvent{Type: typ, ItemID: itemID, Direction: harnessproto.DirOut, Payload: mustMarshal(payload)}
}

func toolCall(itemID, title, kind, input string) harnessproto.RuntimeEvent {
	return harnessproto.RuntimeEvent{
		Type:      harnessproto.TypeToolCall,
		ItemID:    itemID,
		Direction: harnessproto.DirOut,
		Payload: mustMarshal(map[string]any{
			"item_id": itemID, "title": title, "kind": kind, "status": "in_progress", "input": input,
		}),
	}
}

func toolResult(itemID, status, output string, diffs []map[string]any, rawOut any) harnessproto.RuntimeEvent {
	if diffs == nil {
		diffs = []map[string]any{}
	}
	return harnessproto.RuntimeEvent{
		Type:      harnessproto.TypeToolResult,
		ItemID:    itemID,
		Direction: harnessproto.DirOut,
		Payload: mustMarshal(map[string]any{
			"item_id": itemID, "status": status, "output": output, "diffs": diffs, "raw_output": rawOut,
		}),
	}
}

func planFromTurn(params json.RawMessage) harnessproto.RuntimeEvent {
	var p struct {
		Plan []struct {
			Step   string `json:"step"`
			Status string `json:"status"`
		} `json:"plan"`
	}
	_ = json.Unmarshal(params, &p)
	items := make([]map[string]any, 0, len(p.Plan))
	for _, s := range p.Plan {
		items = append(items, map[string]any{"text": s.Step, "status": planStatus(s.Status)})
	}
	return harnessproto.RuntimeEvent{Type: harnessproto.TypePlan, Direction: harnessproto.DirOut,
		Payload: mustMarshal(map[string]any{"items": items})}
}

// ── small helpers ────────────────────────────────────────────────────────────

// statusFor maps an App Server item status to the tool_result vocabulary.
func statusFor(s string) string {
	switch s {
	case "completed", "success":
		return "success"
	case "failed", "error":
		return "error"
	case "", "inProgress", "in_progress":
		return "in_progress"
	default:
		return s
	}
}

// planStatus maps an App Server plan step status to the plan vocabulary.
func planStatus(s string) string {
	switch s {
	case "completed":
		return "completed"
	case "inProgress", "in_progress":
		return "in_progress"
	default:
		return "pending"
	}
}

func notice(level, text string) harnessproto.RuntimeEvent {
	return harnessproto.RuntimeEvent{Type: harnessproto.TypeNotice, Direction: harnessproto.DirMeta,
		Payload: mustMarshal(map[string]any{"level": level, "text": text})}
}

func turnStartEvent() harnessproto.RuntimeEvent {
	return harnessproto.RuntimeEvent{Type: harnessproto.TypeTurnStart, Direction: harnessproto.DirMeta,
		Payload: mustMarshal(map[string]any{})}
}

func turnEndEvent(stopReason string) harnessproto.RuntimeEvent {
	return harnessproto.RuntimeEvent{Type: harnessproto.TypeTurnEnd, Direction: harnessproto.DirMeta,
		Payload: mustMarshal(map[string]any{"stop_reason": stopReason})}
}

func rawEvent(nativeType string, body json.RawMessage) harnessproto.RuntimeEvent {
	if len(body) == 0 {
		body = json.RawMessage(`{}`)
	}
	return harnessproto.RuntimeEvent{Type: harnessproto.TypeRaw, Direction: harnessproto.DirOut,
		Payload: mustMarshal(map[string]any{
			"runtime": harnessproto.RuntimeCodex, "native_type": nativeType, "body": body,
		})}
}

func rawOrNull(v json.RawMessage) any {
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	return v
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
