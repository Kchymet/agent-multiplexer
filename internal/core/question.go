package core

import "encoding/json"

// The Claude question hold (docs/question-prompts.md §4.1). Claude Code runs
// `amux agent question hold` as a blocking PreToolUse hook for
// AskUserQuestion; the hook registers the call with the daemon and long-polls
// QueryQuestionHold until the user answers from any amux surface (the hook
// then allows the call with the answers as updatedInput), dismisses it (deny
// with a reason), or the daemon releases it so Claude draws its own dialog.
const (
	// QueryQuestionHold registers (idempotently) and waits on the caller's own
	// held AskUserQuestion call. ID is the caller. Fields: HoldToolUseField,
	// HoldInputField (the call's tool_input JSON), HoldTimeoutField (whole
	// seconds, 0..MaxWaitSeconds). Answers a QuestionHoldResult.
	QueryQuestionHold = "question-hold"
	// ActionQueueRelease releases a held question to the agent's pane (the
	// user chose to answer there). Host only. Fields: QueueEntryField.
	ActionQueueRelease = "queue-release"
)

// QueryQuestionHold fields.
const (
	HoldToolUseField = "tool_use_id"
	HoldInputField   = "input"
	HoldTimeoutField = "timeout"
)

// MaxHoldInputBytes bounds a held call's input on the session RPC (its body is
// bounded at 64 KiB); a larger call is not held (the hook releases at once).
const MaxHoldInputBytes = 48 << 10

// Hold outcomes (QuestionHoldResult.Action).
const (
	HoldWait    = "wait"    // still held: call again
	HoldRelease = "release" // let Claude draw its dialog: print nothing
	HoldAllow   = "allow"   // answered: allow with UpdatedInput
	HoldDeny    = "deny"    // dismissed: deny with Reason
)

// QuestionHoldResult is QueryQuestionHold's answer.
type QuestionHoldResult struct {
	Action       string          `json:"action"`
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	// Deadline is when the daemon releases the hold (Unix ms); the hook stops
	// polling and releases by then even if the daemon goes quiet.
	Deadline int64 `json:"deadline,omitempty"`
}
