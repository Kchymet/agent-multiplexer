package harnessproto

import "encoding/json"

// RuntimeEvent is one structured transcript event derived from a runtime's
// on-disk session record (docs/remote-provider-sessions.md §4). The envelope is
// intentionally generic — a stable, documented vocabulary of Type strings, an
// optional coalescing ItemID, an optional Direction, and an opaque Payload — so
// amux carries no orchestrator-specific schema. Consumers MUST pass an unknown
// Type through rather than dropping it.
type RuntimeEvent struct {
	Type      string          `json:"type"`
	ItemID    string          `json:"item_id,omitempty"`
	Direction string          `json:"direction,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// RuntimeEventBatch groups a seq-ordered slice of RuntimeEvents with the ordinal
// of its last event. It is the internal handoff a runtime-events source hands the
// provider to frame — not itself a wire type (the provider unpacks it into a
// runtime-events HarnessMsg). Seq is per-session monotonic.
type RuntimeEventBatch struct {
	Seq int64
	// Runtime names the agent runtime whose record produced these events
	// (Runtime* below). The provider stamps it on the runtime-events frame so a
	// consumer never has to assume a runtime.
	Runtime string
	Events  []RuntimeEvent
}

// Runtime names for RuntimeEventBatch.Runtime / HarnessMsg.Runtime and the
// `runtime` field of a `raw` event payload. The set is open — a consumer that
// meets an unknown runtime renders the events generically rather than dropping
// them — but these are the runtimes amux reads today.
const (
	RuntimeClaude = "claude" // Claude Code session JSONL
	RuntimeCodex  = "codex"  // Codex CLI rollout JSONL
)

// The generic structured-event vocabulary (docs/remote-provider-sessions.md §4).
// This is the published set of RuntimeEvent.Type strings: amux emits them and the
// harness orchestrator maps them onto its own contract instead of re-declaring the
// literals. They are NOT specific to any orchestrator. A consumer MUST pass an
// unknown type through rather than dropping it.
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

// Event directions (RuntimeEvent.Direction).
const (
	DirIn   = "in"
	DirOut  = "out"
	DirMeta = "meta"
)
