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
