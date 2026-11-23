package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"amux/internal/claudecfg"
	"amux/internal/core"
)

func holdPayload() claudecfg.HookPayload {
	return claudecfg.HookPayload{HookEventName: "PreToolUse", ToolName: "AskUserQuestion", ToolUseID: "toolu_1",
		ToolInput: json.RawMessage(`{"questions":[{"question":"Q?","header":"H","options":[{"label":"A"},{"label":"B"}]}]}`)}
}

func selfOK() (string, error) { return "m1", nil }

// TestQuestionHoldHookOutput pins the hook's stdout per daemon outcome: the
// PreToolUse decision Claude reads, or nothing at all.
func TestQuestionHoldHookOutput(t *testing.T) {
	cases := []struct {
		name    string
		results []core.QuestionHoldResult
		err     error
		want    string
	}{
		{"allow", []core.QuestionHoldResult{{Action: core.HoldWait}, {Action: core.HoldAllow, UpdatedInput: json.RawMessage(`{"questions":[],"answers":{"Q?":"A"}}`)}}, nil,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"questions":[],"answers":{"Q?":"A"}}}}` + "\n"},
		{"deny", []core.QuestionHoldResult{{Action: core.HoldDeny, Reason: "The user dismissed these questions without answering."}}, nil,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"The user dismissed these questions without answering."}}` + "\n"},
		{"release", []core.QuestionHoldResult{{Action: core.HoldRelease}}, nil, ""},
		{"unknown action releases", []core.QuestionHoldResult{{Action: "future"}}, nil, ""},
		{"daemon unreachable releases", nil, errors.New("no daemon"), ""},
		{"bad updated input releases", []core.QuestionHoldResult{{Action: core.HoldAllow, UpdatedInput: json.RawMessage(`{`)}}, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			i := 0
			query := func(id string, f map[string]string) (core.QuestionHoldResult, error) {
				if id != "m1" || f[core.HoldToolUseField] != "toolu_1" || f[core.HoldInputField] == "" {
					t.Fatalf("query %s %v", id, f)
				}
				if c.err != nil {
					return core.QuestionHoldResult{}, c.err
				}
				r := c.results[i]
				i++
				return r, nil
			}
			writeQuestionHold(&out, holdPayload(), selfOK, query, time.Now)
			if out.String() != c.want {
				t.Fatalf("stdout = %q, want %q", out.String(), c.want)
			}
		})
	}
}

// TestQuestionHoldHookFailsOpen: anything that is not a holdable question, or
// no session context, releases without asking the daemon.
func TestQuestionHoldHookFailsOpen(t *testing.T) {
	never := func(string, map[string]string) (core.QuestionHoldResult, error) {
		t.Fatal("the daemon was asked")
		return core.QuestionHoldResult{}, nil
	}
	for name, p := range map[string]claudecfg.HookPayload{
		"other tool":    {ToolName: "Bash", ToolUseID: "x", ToolInput: json.RawMessage(`{}`)},
		"no call id":    {ToolName: "AskUserQuestion", ToolInput: json.RawMessage(`{}`)},
		"no input":      {ToolName: "AskUserQuestion", ToolUseID: "x"},
		"input too big": {ToolName: "AskUserQuestion", ToolUseID: "x", ToolInput: json.RawMessage(`"` + string(bytes.Repeat([]byte("a"), core.MaxHoldInputBytes)) + `"`)},
	} {
		var out bytes.Buffer
		writeQuestionHold(&out, p, selfOK, never, time.Now)
		if out.Len() != 0 {
			t.Errorf("%s: stdout %q", name, out.String())
		}
	}
	var out bytes.Buffer
	writeQuestionHold(&out, holdPayload(), func() (string, error) { return "", errors.New("not an agent") }, never, time.Now)
	if out.Len() != 0 {
		t.Errorf("no context: stdout %q", out.String())
	}
}

// TestQuestionHoldHookStopsAtDeadline: the hook stops on the daemon's
// deadline (and its own hard stop) even if the daemon keeps saying wait.
func TestQuestionHoldHookStopsAtDeadline(t *testing.T) {
	calls := 0
	past := time.Now().Add(-time.Second).UnixMilli()
	query := func(string, map[string]string) (core.QuestionHoldResult, error) {
		calls++
		return core.QuestionHoldResult{Action: core.HoldWait, Deadline: past}, nil
	}
	var out bytes.Buffer
	writeQuestionHold(&out, holdPayload(), selfOK, query, time.Now)
	if calls != 1 || out.Len() != 0 {
		t.Fatalf("calls = %d, stdout %q", calls, out.String())
	}
	clock := time.Now()
	tick := func() time.Time { clock = clock.Add(10 * time.Minute); return clock }
	calls = 0
	wait := func(string, map[string]string) (core.QuestionHoldResult, error) {
		calls++
		return core.QuestionHoldResult{Action: core.HoldWait}, nil
	}
	writeQuestionHold(&out, holdPayload(), selfOK, wait, tick)
	if calls == 0 || calls > 4 || out.Len() != 0 {
		t.Fatalf("hard stop: calls = %d, stdout %q", calls, out.String())
	}
}
