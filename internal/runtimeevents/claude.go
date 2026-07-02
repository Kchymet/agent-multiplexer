// Package runtimeevents is the daemon-side reader of a local agent runtime's
// on-disk session record: it tails the record and maps each entry to the generic,
// orchestrator-agnostic structured-event vocabulary the "runtime-events" feature
// carries (docs/remote-provider-sessions.md §4). It is read-only — there is no
// input path — and claude-first: Claude Code writes a per-session JSONL under its
// projects dir, keyed by the conversation id amux pins per session; this package
// maps those entries to events. Anything it can't map becomes a `raw` event so no
// data is ever dropped.
//
// amux stays orchestrator-agnostic: the Type strings below are a stable,
// documented vocabulary a producer emits (the orchestrator maps them onto its own
// contract). They are NOT specific to any orchestrator.
package runtimeevents

import (
	"encoding/json"
	"fmt"
	"strings"

	"amux/internal/harnessproto"
)

// The generic structured-event vocabulary (docs/remote-provider-sessions.md §4).
// A consumer MUST pass an unknown type through rather than dropping it.
const (
	TypePrompt            = "prompt"             // in:  {text}
	TypeTurnStart         = "turn_start"         // out: {}
	TypeText              = "text"               // out: {text, final?}
	TypeThinking          = "thinking"           // out: {text}
	TypeToolCall          = "tool_call"          // out: {item_id, title, kind, status, input, raw_input?}
	TypeToolResult        = "tool_result"        // out: {item_id, status, output, diffs?, raw_output?}
	TypePlan              = "plan"               // out: {items:[{text,status}]}
	TypeUsage             = "usage"              // out: {used, size, cost?}
	TypePermissionRequest = "permission_request" // out: {request_id, tool, action, options}
	TypeNotice            = "notice"             // out: {level, text}
	TypeTurnEnd           = "turn_end"           // out: {stop_reason}
	TypeRaw               = "raw"                // out: {runtime, native_type, body}  (never dropped)
)

const (
	dirIn  = "in"
	dirOut = "out"
	dirMeta = "meta"
)

// runtimeName labels `raw` and usage events with the producing runtime.
const runtimeName = "claude"

// ClaudeState carries decode state across lines of one session's JSONL: the open
// tool_use inputs, so a later tool_result (in a following `user` entry) can
// recover the file diffs its tool call produced. It is NOT safe for concurrent
// use — one per session tail.
type ClaudeState struct {
	tools map[string]toolInfo // tool_use id -> {name,input}
}

type toolInfo struct {
	name  string
	input json.RawMessage
}

func (st *ClaudeState) ensure() {
	if st.tools == nil {
		st.tools = map[string]toolInfo{}
	}
}

// entry is the outer Claude Code JSONL record; only the fields we branch on are
// decoded. The on-disk shape differs from the headless stream-json surface: each
// line is a durable record with a top-level `type` and an embedded Anthropic
// `message` for user/assistant lines.
type entry struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype"`
	Message json.RawMessage `json:"message"`
	Level   string          `json:"level"`
	Summary string          `json:"summary"`
}

// MapClaudeLine decodes one line of a Claude Code session JSONL into zero or more
// generic structured events (already normalized). A line it cannot decode as
// JSON, or whose `type` has no mapping, becomes a single `raw` event — never
// dropped (docs/remote-provider-sessions.md §4).
func MapClaudeLine(line []byte, st *ClaudeState) []harnessproto.RuntimeEvent {
	st.ensure()
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return nil
	}
	var e entry
	if err := json.Unmarshal(line, &e); err != nil || e.Type == "" {
		return []harnessproto.RuntimeEvent{rawEvent("unparsable", json.RawMessage(line))}
	}
	switch e.Type {
	case "user":
		return mapUser(e.Message, st)
	case "assistant":
		return mapAssistant(e.Message, st)
	case "system":
		text := "system"
		if e.Subtype != "" {
			text = "system: " + e.Subtype
		}
		level := e.Level
		if level == "" {
			level = "info"
		}
		return []harnessproto.RuntimeEvent{notice(level, text)}
	case "summary":
		if e.Summary != "" {
			return []harnessproto.RuntimeEvent{notice("info", "summary: "+e.Summary)}
		}
		return []harnessproto.RuntimeEvent{rawEvent("summary", json.RawMessage(line))}
	default:
		// Unknown record type (mode, permission-mode, ai-title, attachment,
		// file-history-snapshot, pr-link, …): passthrough, never dropped.
		return []harnessproto.RuntimeEvent{rawEvent(e.Type, json.RawMessage(line))}
	}
}

// ── message content blocks ──────────────────────────────────────────────────

type message struct {
	ID         string          `json:"id"`
	Content    json.RawMessage `json:"content"` // string OR []contentBlock
	Usage      json.RawMessage `json:"usage"`
	StopReason string          `json:"stop_reason"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`          // tool_use id
	Name      string          `json:"name"`        // tool name
	Input     json.RawMessage `json:"input"`       // tool_use input
	ToolUseID string          `json:"tool_use_id"` // tool_result link
	Content   json.RawMessage `json:"content"`     // tool_result content
	IsError   bool            `json:"is_error"`    // tool_result error flag
}

// mapUser maps a `user` entry. A string content is the human prompt (turn_start +
// prompt); an array carries tool_result blocks (and, rarely, text/image).
func mapUser(raw json.RawMessage, st *ClaudeState) []harnessproto.RuntimeEvent {
	var m message
	_ = json.Unmarshal(raw, &m)

	// content may be a bare string (the prompt) or an array of blocks.
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []harnessproto.RuntimeEvent{
			{Type: TypeTurnStart, Direction: dirOut, Payload: mustMarshal(map[string]any{})},
			{Type: TypePrompt, Direction: dirIn, Payload: mustMarshal(map[string]any{"text": s})},
		}
	}

	var blocks []contentBlock
	if json.Unmarshal(m.Content, &blocks) != nil {
		return []harnessproto.RuntimeEvent{rawEvent("user/content", m.Content)}
	}
	out := make([]harnessproto.RuntimeEvent, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			status := "success"
			if b.IsError {
				status = "error"
			}
			info := st.tools[b.ToolUseID]
			delete(st.tools, b.ToolUseID)
			out = append(out, harnessproto.RuntimeEvent{
				Type: TypeToolResult, ItemID: b.ToolUseID, Direction: dirOut,
				Payload: mustMarshal(map[string]any{
					"item_id":    b.ToolUseID,
					"status":     status,
					"output":     extractText(b.Content),
					"diffs":      diffsForTool(info),
					"raw_output": rawOrNull(b.Content),
				}),
			})
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				out = append(out, harnessproto.RuntimeEvent{
					Type: TypePrompt, Direction: dirIn,
					Payload: mustMarshal(map[string]any{"text": b.Text}),
				})
			}
		default:
			out = append(out, rawEvent("user/"+b.Type, mustMarshal(b)))
		}
	}
	return out
}

// mapAssistant maps an `assistant` entry's content blocks, then its usage and a
// terminal turn_end when the model stopped for a non-tool reason.
func mapAssistant(raw json.RawMessage, st *ClaudeState) []harnessproto.RuntimeEvent {
	var m message
	_ = json.Unmarshal(raw, &m)
	var blocks []contentBlock
	_ = json.Unmarshal(m.Content, &blocks)

	out := make([]harnessproto.RuntimeEvent, 0, len(blocks)+2)
	for i, b := range blocks {
		itemID := blockItemID(m.ID, i)
		switch b.Type {
		case "text":
			out = append(out, harnessproto.RuntimeEvent{
				Type: TypeText, ItemID: itemID, Direction: dirOut,
				Payload: mustMarshal(map[string]any{"text": b.Text, "final": true}),
			})
		case "thinking":
			out = append(out, harnessproto.RuntimeEvent{
				Type: TypeThinking, ItemID: itemID, Direction: dirOut,
				Payload: mustMarshal(map[string]any{"text": b.Thinking}),
			})
		case "tool_use":
			out = append(out, mapToolUse(b, st))
		default:
			out = append(out, rawEvent("assistant/"+b.Type, mustMarshal(b)))
		}
	}
	if len(m.Usage) > 0 {
		out = append(out, usageEvent(m.Usage))
	}
	if r := stopReason(m.StopReason); r != "" {
		out = append(out, harnessproto.RuntimeEvent{
			Type: TypeTurnEnd, Direction: dirOut,
			Payload: mustMarshal(map[string]any{"stop_reason": r}),
		})
	}
	return out
}

func mapToolUse(b contentBlock, st *ClaudeState) harnessproto.RuntimeEvent {
	if b.ID != "" {
		st.tools[b.ID] = toolInfo{name: b.Name, input: b.Input}
	}
	if b.Name == "TodoWrite" {
		return planEvent(b.Input)
	}
	return harnessproto.RuntimeEvent{
		Type: TypeToolCall, ItemID: b.ID, Direction: dirOut,
		Payload: mustMarshal(map[string]any{
			"item_id":   b.ID,
			"title":     b.Name,
			"kind":      kindForTool(b.Name),
			"status":    "in_progress",
			"input":     summarizeInput(b.Input),
			"raw_input": rawOrNull(b.Input),
		}),
	}
}

// ── small helpers (ported from the harness Claude adapter, ADR 005 §4.2) ──────

// stopReason returns "" for a turn that stopped to run a tool (no turn_end yet),
// otherwise the model's stop reason.
func stopReason(r string) string {
	switch r {
	case "", "tool_use", "null":
		return ""
	default:
		return r
	}
}

func usageEvent(usage json.RawMessage) harnessproto.RuntimeEvent {
	var u struct {
		Input       int64 `json:"input_tokens"`
		Output      int64 `json:"output_tokens"`
		CacheRead   int64 `json:"cache_read_input_tokens"`
		CacheCreate int64 `json:"cache_creation_input_tokens"`
	}
	_ = json.Unmarshal(usage, &u)
	return harnessproto.RuntimeEvent{
		Type: TypeUsage, Direction: dirMeta,
		Payload: mustMarshal(map[string]any{
			"used": u.Input + u.Output + u.CacheRead + u.CacheCreate,
			"size": int64(0),
		}),
	}
}

func notice(level, text string) harnessproto.RuntimeEvent {
	return harnessproto.RuntimeEvent{Type: TypeNotice, Direction: dirMeta,
		Payload: mustMarshal(map[string]any{"level": level, "text": text})}
}

func rawEvent(nativeType string, body json.RawMessage) harnessproto.RuntimeEvent {
	// body must be valid JSON to embed as-is; an unparsable line is preserved as a
	// JSON string so the raw event never loses the original bytes.
	var bodyVal any
	if len(body) == 0 {
		bodyVal = json.RawMessage(`{}`)
	} else if json.Valid(body) {
		bodyVal = body
	} else {
		bodyVal = string(body)
	}
	return harnessproto.RuntimeEvent{Type: TypeRaw, Direction: dirOut,
		Payload: mustMarshal(map[string]any{
			"runtime":     runtimeName,
			"native_type": nativeType,
			"body":        bodyVal,
		})}
}

func planEvent(input json.RawMessage) harnessproto.RuntimeEvent {
	var p struct {
		Todos []struct {
			Content    string `json:"content"`
			ActiveForm string `json:"activeForm"`
			Status     string `json:"status"`
		} `json:"todos"`
	}
	_ = json.Unmarshal(input, &p)
	items := make([]map[string]any, 0, len(p.Todos))
	for _, t := range p.Todos {
		text := t.Content
		if text == "" {
			text = t.ActiveForm
		}
		status := t.Status
		if status == "" {
			status = "pending"
		}
		items = append(items, map[string]any{"text": text, "status": status})
	}
	return harnessproto.RuntimeEvent{Type: TypePlan, Direction: dirOut,
		Payload: mustMarshal(map[string]any{"items": items})}
}

// blockItemID mints the coalescing key for a text/thinking block: the message id
// for the first block, message-id:index for later ones.
func blockItemID(msgID string, index int) string {
	if index == 0 {
		return msgID
	}
	return fmt.Sprintf("%s:%d", msgID, index)
}

func kindForTool(name string) string {
	switch name {
	case "Read", "NotebookRead":
		return "read"
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		return "edit"
	case "Bash", "BashOutput", "KillShell":
		return "execute"
	case "Glob", "Grep", "LS":
		return "search"
	case "WebFetch", "WebSearch":
		return "fetch"
	default:
		return "other"
	}
}

func diffsForTool(info toolInfo) []map[string]any {
	out := []map[string]any{}
	if len(info.input) == 0 {
		return out
	}
	switch info.name {
	case "Edit":
		var e struct {
			Path string  `json:"file_path"`
			Old  *string `json:"old_string"`
			New  string  `json:"new_string"`
		}
		if json.Unmarshal(info.input, &e) == nil && e.Path != "" {
			out = append(out, map[string]any{"path": e.Path, "old": e.Old, "new": e.New})
		}
	case "MultiEdit":
		var e struct {
			Path  string `json:"file_path"`
			Edits []struct {
				Old *string `json:"old_string"`
				New string  `json:"new_string"`
			} `json:"edits"`
		}
		if json.Unmarshal(info.input, &e) == nil && e.Path != "" {
			for _, ed := range e.Edits {
				out = append(out, map[string]any{"path": e.Path, "old": ed.Old, "new": ed.New})
			}
		}
	case "Write":
		var e struct {
			Path    string `json:"file_path"`
			Content string `json:"content"`
		}
		if json.Unmarshal(info.input, &e) == nil && e.Path != "" {
			out = append(out, map[string]any{"path": e.Path, "old": nil, "new": e.Content})
		}
	}
	return out
}

func summarizeInput(input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var generic map[string]any
	if err := json.Unmarshal(input, &generic); err != nil {
		return ""
	}
	for _, k := range []string{"command", "file_path", "path", "pattern", "query", "url", "prompt", "description"} {
		if v, ok := generic[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return string(input)
}

func extractText(v json.RawMessage) string {
	if len(v) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var arr []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(v, &arr) == nil {
		parts := make([]string, 0, len(arr))
		for _, b := range arr {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
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
