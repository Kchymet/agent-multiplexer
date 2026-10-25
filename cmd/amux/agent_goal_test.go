package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/claudegoal"
	"amux/internal/core"
	"amux/internal/sessionreport"
)

// The goal loop end to end through the generated hook commands: a submitted
// task becomes the goal, Stop blocks with a continuation while it is active and
// reports the turn still running, and the model's own `complete` lets the next
// Stop end the turn as ready.
func TestAgentGoalHookLoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".amux", "goal.json")
	t.Setenv(claudegoal.EnvFile, path)
	rpc := successfulReportRPC(t)

	withHookStdin(t, `{"hook_event_name":"UserPromptSubmit","prompt":"fix the flaky deploy"}`, func() {
		if err := cmdAgentGoal([]string{"prompt", "--hook"}); err != nil {
			t.Fatal(err)
		}
	})
	s := readGoalFile(t, path)
	if s.Status != claudegoal.StatusActive || s.Objective != "fix the flaky deploy" {
		t.Fatalf("goal after prompt: %+v", s)
	}

	out := runStopHook(t)
	var decision struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &decision); err != nil {
		t.Fatalf("stop hook output %q: %v", out, err)
	}
	if decision.Decision != "block" || !strings.Contains(decision.Reason, "fix the flaky deploy") {
		t.Fatalf("stop decision = %+v", decision)
	}
	if got := lastActivity(t, rpc); got != core.StateRunning {
		t.Fatalf("continued turn reported %q, want running", got)
	}

	if _, err := captureOutput(t, func() error { return cmdAgentGoal([]string{"complete", "deployed and verified"}) }); err != nil {
		t.Fatal(err)
	}
	if s := readGoalFile(t, path); s.Status != claudegoal.StatusComplete || s.Note != "deployed and verified" {
		t.Fatalf("goal after complete: %+v", s)
	}
	if out := runStopHook(t); strings.TrimSpace(out) != "" {
		t.Fatalf("stop hook continued a complete goal: %q", out)
	}
	if got := lastActivity(t, rpc); got != core.StateReady {
		t.Fatalf("ended turn reported %q, want ready", got)
	}
}

// Asking the user ends the turn; the reply resumes the same goal.
func TestAgentGoalAskWaitsForReply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goal.json")
	t.Setenv(claudegoal.EnvFile, path)
	successfulReportRPC(t)
	if err := writeGoal(path, claudegoal.State{Objective: "rotate keys", Status: claudegoal.StatusActive}); err != nil {
		t.Fatal(err)
	}
	if _, err := captureOutput(t, func() error { return cmdAgentGoal([]string{"ask"}) }); err == nil {
		t.Fatal("ask without a question accepted")
	}
	if _, err := captureOutput(t, func() error { return cmdAgentGoal([]string{"ask", "which", "vault?"}) }); err != nil {
		t.Fatal(err)
	}
	if out := runStopHook(t); strings.TrimSpace(out) != "" {
		t.Fatalf("stop hook continued while waiting for input: %q", out)
	}
	withHookStdin(t, `{"hook_event_name":"UserPromptSubmit","prompt":"the prod vault"}`, func() {
		_ = cmdAgentGoal([]string{"prompt", "--hook"})
	})
	s := readGoalFile(t, path)
	if s.Status != claudegoal.StatusActive || !strings.HasPrefix(s.Objective, "rotate keys") || !strings.Contains(s.Objective, "the prod vault") {
		t.Fatalf("goal after reply: %+v", s)
	}
}

// Outside a Claude goal session the commands say so, and the hooks never
// disrupt the agent: a non-goal Stop still reports ready and continues nothing.
func TestAgentGoalOutsideGoalSession(t *testing.T) {
	t.Setenv(claudegoal.EnvFile, "")
	rpc := successfulReportRPC(t)
	if _, err := captureOutput(t, func() error { return cmdAgentGoal([]string{"show"}) }); err == nil || !strings.Contains(err.Error(), "not in Claude goal mode") {
		t.Fatalf("show outside goal mode: %v", err)
	}
	if out := runStopHook(t); strings.TrimSpace(out) != "" {
		t.Fatalf("stop hook output outside goal mode: %q", out)
	}
	if got := lastActivity(t, rpc); got != core.StateReady {
		t.Fatalf("stop outside goal mode reported %q, want ready", got)
	}
	if err := cmdAgentGoal([]string{"stop"}); err == nil {
		t.Fatal("stop accepted without --hook")
	}
}

func runStopHook(t *testing.T) string {
	t.Helper()
	var out string
	withHookStdin(t, `{"hook_event_name":"Stop","stop_hook_active":false}`, func() {
		var err error
		out, err = captureOutput(t, func() error { return cmdAgentGoal([]string{"stop", "--hook"}) })
		if err != nil {
			t.Fatal(err)
		}
	})
	return out
}

func readGoalFile(t *testing.T, path string) claudegoal.State {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := claudegoal.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func lastActivity(t *testing.T, rpc *fakeRestrictedRPC) string {
	t.Helper()
	for i := len(rpc.actions) - 1; i >= 0; i-- {
		if rpc.actions[i].Verb == sessionreport.Activity {
			return rpc.actions[i].Fields[sessionreport.FieldState]
		}
	}
	t.Fatal("no activity reported")
	return ""
}
