package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/claudegoal"
	"amux/internal/core"
	"amux/internal/sessionrpc"
)

func TestIdleMemberReminderListsOnlyIdleUnarchivedMembers(t *testing.T) {
	sessions := []core.Session{
		{ID: "wg1", IsRoot: true, State: core.StateReady, Title: "workgroup"},
		{ID: "m-ready", RootID: "wg1", State: core.StateReady, Title: "port auth"},
		{ID: "m-idle", RootID: "wg1", Title: "docs"},
		{ID: "m-wait", RootID: "wg1", State: core.StateWaiting, Title: "needs input"},
		{ID: "m-run", RootID: "wg1", State: core.StateRunning, Title: "busy"},
		{ID: "m-done", RootID: "wg1", Archived: true, State: core.StateIdle, Title: "archived"},
	}
	got := idleMemberReminder(sessions)
	for _, want := range []string{
		"3 member(s)",
		"amux do set-archived m-ready -f archived=true",
		"amux do set-archived m-idle -f archived=true",
		"amux do set-archived m-wait -f archived=true",
		"say why",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reminder lacks %q:\n%s", want, got)
		}
	}
	for _, forbidden := range []string{"m-run", "m-done", "set-archived wg1"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("reminder lists %q:\n%s", forbidden, got)
		}
	}
	if got := idleMemberReminder(sessions[:1]); got != "" {
		t.Errorf("no members, reminder = %q", got)
	}
	if got := idleMemberReminder([]core.Session{sessions[0], sessions[4], sessions[5]}); got != "" {
		t.Errorf("only running/archived members, reminder = %q", got)
	}
}

// `amux agent goal complete` from a coordinator prints the reminder from its
// own scoped snapshot, and still completes the goal when the query fails.
func TestAgentGoalCompleteRemindsToArchiveMembers(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".amux", "goal.json")
	t.Setenv(claudegoal.EnvFile, path)
	rpc := successfulReportRPC(t)
	body, err := json.Marshal([]core.Session{
		{ID: "wg1", IsRoot: true, State: core.StateReady},
		{ID: "a1", RootID: "wg1", State: core.StateReady, Title: "shipped"},
		{ID: "a2", RootID: "wg1", State: core.StateRunning},
	})
	if err != nil {
		t.Fatal(err)
	}
	rpc.query = sessionrpc.Result{Status: sessionrpc.StatusOK, Body: body}
	withHookStdin(t, `{"hook_event_name":"UserPromptSubmit","prompt":"ship it"}`, func() {
		if err := cmdAgentGoal([]string{"prompt", "--hook"}); err != nil {
			t.Fatal(err)
		}
	})
	out, err := captureOutput(t, func() error { return cmdAgentGoal([]string{"complete", "shipped"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "amux do set-archived a1 -f archived=true") || strings.Contains(out, "a2") {
		t.Fatalf("complete output:\n%s", out)
	}
	if rpc.lastQuery.Verb != core.QuerySnapshot {
		t.Fatalf("reminder queried %+v, want the scoped snapshot", rpc.lastQuery)
	}
	if s := readGoalFile(t, path); s.Status != claudegoal.StatusComplete {
		t.Fatalf("goal after complete: %+v", s)
	}

	// A failed query never blocks completion and prints no reminder.
	rpc.query = sessionrpc.Result{Status: sessionrpc.StatusDenied, Code: "access_denied"}
	withHookStdin(t, `{"hook_event_name":"UserPromptSubmit","prompt":"again"}`, func() {
		if err := cmdAgentGoal([]string{"prompt", "--hook"}); err != nil {
			t.Fatal(err)
		}
	})
	out, err = captureOutput(t, func() error { return cmdAgentGoal([]string{"complete"}) })
	if err != nil || strings.Contains(out, "reminder") {
		t.Fatalf("complete with failed query: %q, %v", out, err)
	}
	if s := readGoalFile(t, path); s.Status != claudegoal.StatusComplete {
		t.Fatalf("goal after complete: %+v", s)
	}
}
