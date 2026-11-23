package main

import (
	"errors"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/sessionreport"
)

// TestHookActivityReportsOnlyChanges pins the hook half of the 2026-11-22
// amplification fix: PostToolUse reporting "running" after every tool call
// costs the daemon one call per change (plus a periodic refresh), not one per
// tool call. A failed report is not remembered, so the next hook retries it.
func TestHookActivityReportsOnlyChanges(t *testing.T) {
	rpc := successfulReportRPC(t)
	now := time.Unix(1_800_000_000, 0)
	oldNow := statusLineNow
	statusLineNow = func() time.Time { return now }
	t.Cleanup(func() { statusLineNow = oldNow })
	hook := func(event, state string) {
		t.Helper()
		withHookStdin(t, `{"hook_event_name":"`+event+`","session_id":"conv-1"}`, func() {
			if err := cmdAgentStatus([]string{state}, true); err != nil {
				t.Fatal(err)
			}
		})
	}
	activity := func() int {
		n := 0
		for _, a := range rpc.actions {
			if a.Verb == sessionreport.Activity {
				n++
			}
		}
		return n
	}
	hook("UserPromptSubmit", core.StateRunning)
	for i := 0; i < 5; i++ {
		hook("PostToolUse", core.StateRunning)
	}
	if got := activity(); got != 1 {
		t.Fatalf("repeated running reports = %d calls, want 1", got)
	}
	hook("Stop", core.StateIdle)
	hook("UserPromptSubmit", core.StateRunning)
	if got := activity(); got != 3 {
		t.Fatalf("state changes = %d calls, want 3", got)
	}
	now = now.Add(activityRefresh)
	hook("PostToolUse", core.StateRunning)
	if got := activity(); got != 4 {
		t.Fatalf("unchanged state not refreshed after %v: %d calls", activityRefresh, got)
	}
	rpc.actionErr = errors.New("daemon busy")
	hook("Stop", core.StateIdle)
	rpc.actionErr = nil
	hook("Stop", core.StateIdle)
	if got := activity(); got != 6 {
		t.Fatalf("failed report was remembered as delivered: %d calls", got)
	}
}
