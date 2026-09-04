package runtimeevents

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"
)

// TestClaudeTranscriptContract exercises the mapper against a versioned, recorded
// Claude Code session transcript (testdata/claude_session.jsonl) and pins the
// contract amux depends on. Claude Code's on-disk JSONL shape is unversioned; if
// an upstream release changes it, re-recording the fixture makes this test fail
// loudly — surfacing the break instead of runtime events silently degrading. It
// also guards the mapper itself against regressions.
func TestClaudeTranscriptContract(t *testing.T) {
	f, err := os.Open("testdata/claude_session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	counts := map[string]int{}
	var order []string
	toolNames := map[string]bool{}
	st := &ClaudeState{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		for _, e := range MapClaudeLine(sc.Bytes(), st) {
			counts[e.Type]++
			order = append(order, e.Type)
			if e.Type == TypeToolCall {
				var p struct {
					Title string `json:"title"` // the tool name
				}
				_ = json.Unmarshal(e.Payload, &p)
				toolNames[p.Title] = true
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	// The contract, keyed to the recorded fixture:
	// - a user prompt yields turn_start then prompt
	if counts[TypeTurnStart] != 1 || counts[TypePrompt] != 1 {
		t.Errorf("turn_start=%d prompt=%d, want 1 and 1", counts[TypeTurnStart], counts[TypePrompt])
	}
	// - assistant content blocks map to thinking / text / tool_call
	if counts[TypeThinking] != 1 {
		t.Errorf("thinking=%d, want 1", counts[TypeThinking])
	}
	if counts[TypeText] != 2 { // one alongside the tool_use, one in the final turn
		t.Errorf("text=%d, want 2", counts[TypeText])
	}
	if counts[TypeToolCall] != 1 || !toolNames["Bash"] {
		t.Errorf("tool_call=%d names=%v, want 1 Bash", counts[TypeToolCall], toolNames)
	}
	// - a tool_result line maps to a tool_result event
	if counts[TypeToolResult] != 1 {
		t.Errorf("tool_result=%d, want 1", counts[TypeToolResult])
	}
	// - exactly ONE turn_end: the end_turn assistant, NOT the tool_use one (the
	//   stop_reason=="tool_use" sentinel suppresses a turn boundary mid-tool).
	if counts[TypeTurnEnd] != 1 {
		t.Errorf("turn_end=%d, want exactly 1 (tool_use must not end a turn)", counts[TypeTurnEnd])
	}
	// - usage is captured
	if counts[TypeUsage] == 0 {
		t.Error("usage not captured from the transcript")
	}
	// - an unmodeled record type is preserved as raw, never dropped
	if counts[TypeRaw] != 1 {
		t.Errorf("raw=%d, want 1 (the unknown record type must pass through)", counts[TypeRaw])
	}
	// - ordering sanity: the first event is the turn_start
	if len(order) == 0 || order[0] != TypeTurnStart {
		t.Errorf("first event = %v, want turn_start", order)
	}
}

// TestCodexTranscriptContract is the Claude contract test's counterpart for
// Codex: it maps a recorded Codex CLI rollout and pins the same contract. Codex's
// rollout shape is unversioned too, so an upstream change that breaks the mapping
// fails here loudly instead of degrading the web UI's transcript to silence.
//
// testdata/codex_session.jsonl is a real rollout written by codex-cli 0.153.2
// (driven against a stub model endpoint so the conversation is fixed), with paths
// rewritten and three bulky verbatim bodies shortened — the base instructions,
// the skills preamble, and the world_state snapshot. Every record kind, envelope
// field and payload shape the mapper reads is as Codex wrote it. Re-record by
// running a Codex session and sanitizing the same way.
func TestCodexTranscriptContract(t *testing.T) {
	f, err := os.Open("testdata/codex_session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	counts := map[string]int{}
	var order []string
	toolNames := map[string]bool{}
	var toolResultStatus string
	st := &CodexState{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		for _, e := range MapCodexLine(sc.Bytes(), st) {
			counts[e.Type]++
			order = append(order, e.Type)
			switch e.Type {
			case TypeToolCall:
				var p struct {
					Title string `json:"title"` // the tool name
				}
				_ = json.Unmarshal(e.Payload, &p)
				toolNames[p.Title] = true
			case TypeToolResult:
				var p struct {
					Status string `json:"status"`
				}
				_ = json.Unmarshal(e.Payload, &p)
				toolResultStatus = p.Status
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	// The contract, keyed to the recorded fixture — the same shape the Claude
	// reader produces for the same conversation:
	// - the turn opens with turn_start and the human's prompt (once each; the
	//   context Codex injects as a user turn must not read as a prompt)
	if counts[TypeTurnStart] != 1 || counts[TypePrompt] != 1 {
		t.Errorf("turn_start=%d prompt=%d, want 1 and 1", counts[TypeTurnStart], counts[TypePrompt])
	}
	// - a reasoning item maps to thinking
	if counts[TypeThinking] != 1 {
		t.Errorf("thinking=%d, want 1", counts[TypeThinking])
	}
	// - both assistant messages map to text (one alongside the tool call, one final)
	if counts[TypeText] != 2 {
		t.Errorf("text=%d, want 2", counts[TypeText])
	}
	// - the function call maps to tool_call, its output to a successful tool_result
	if counts[TypeToolCall] != 1 || !toolNames["exec_command"] {
		t.Errorf("tool_call=%d names=%v, want 1 exec_command", counts[TypeToolCall], toolNames)
	}
	if counts[TypeToolResult] != 1 || toolResultStatus != "success" {
		t.Errorf("tool_result=%d status=%q, want 1 success", counts[TypeToolResult], toolResultStatus)
	}
	// - exactly ONE turn_end: the event_msg stream is the turn authority, and the
	//   item_completed records that mirror response_items must not add their own.
	if counts[TypeTurnEnd] != 1 {
		t.Errorf("turn_end=%d, want exactly 1", counts[TypeTurnEnd])
	}
	// - usage is captured (twice: Codex reports token_count per model response)
	if counts[TypeUsage] == 0 {
		t.Error("usage not captured from the rollout")
	}
	// - session_meta announces the session
	if counts[TypeNotice] != 1 {
		t.Errorf("notice=%d, want 1 (the session_meta record)", counts[TypeNotice])
	}
	// - unmodeled records (world_state, turn_context, the injected context and
	//   developer preamble) pass through as raw rather than being dropped
	if counts[TypeRaw] != 4 {
		t.Errorf("raw=%d, want 4 (unmodeled records must pass through)", counts[TypeRaw])
	}
	// - ordering sanity: the session notice opens the stream (session_meta is the
	//   rollout's first record), then the turn opens before the prompt lands.
	if len(order) == 0 || order[0] != TypeNotice {
		t.Errorf("first event = %v, want the session notice", order)
	}
	if indexOf(order, TypeTurnStart) > indexOf(order, TypePrompt) {
		t.Errorf("turn_start must precede prompt, got %v", order)
	}
}

// indexOf returns the position of the first event of type want, or len(order)
// when it never appears (so an absent event never sorts before a present one).
func indexOf(order []string, want string) int {
	for i, t := range order {
		if t == want {
			return i
		}
	}
	return len(order)
}
