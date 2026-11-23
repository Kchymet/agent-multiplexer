package main

import (
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/sessionreport"
)

// The waiting status hook reports waiting only when the agent is blocked on
// the user: a permission prompt or an open AskUserQuestion. Claude's idle
// reminder leaves the state alone, and an unknown notification type is sent to
// the daemon log rather than read as waiting.
func TestStatusHookReportsWaitingOnlyWhenBlocked(t *testing.T) {
	cases := []struct {
		name      string
		payload   string
		wantVerb  string // "" = no report at all
		wantField string
	}{
		{"permission prompt", `{"hook_event_name":"Notification","notification_type":"permission_prompt","message":"Claude needs your permission to use Bash"}`,
			sessionreport.Activity, core.StateWaiting},
		{"elicitation", `{"hook_event_name":"Notification","notification_type":"elicitation_dialog"}`,
			sessionreport.Activity, core.StateWaiting},
		{"legacy permission", `{"hook_event_name":"Notification","message":"Claude needs your permission to use Bash"}`,
			sessionreport.Activity, core.StateWaiting},
		{"idle reminder", `{"hook_event_name":"Notification","notification_type":"idle_prompt","message":"Claude is waiting for your input"}`,
			"", ""},
		{"legacy idle reminder", `{"hook_event_name":"Notification","message":"Claude is waiting for your input"}`,
			"", ""},
		{"unknown type", `{"hook_event_name":"Notification","notification_type":"brand_new_kind"}`,
			sessionreport.Notice, "claude notification_type=brand_new_kind"},
		{"missing type, unknown text", `{"hook_event_name":"Notification","message":"hello"}`,
			sessionreport.Notice, "claude untyped notification: hello"},
		{"open question", `{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_input":{"questions":[]}}`,
			sessionreport.Activity, core.StateWaiting},
		{"ordinary tool", `{"hook_event_name":"PreToolUse","tool_name":"Bash","tool_input":{"command":"ls"}}`,
			"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rpc := successfulReportRPC(t)
			withHookStdin(t, tc.payload, func() {
				if err := cmdAgentStatus([]string{core.StateWaiting}, true); err != nil {
					t.Fatal(err)
				}
			})
			if tc.wantVerb == "" {
				if len(rpc.actions) != 0 {
					t.Fatalf("reports = %+v, want none", rpc.actions)
				}
				return
			}
			if len(rpc.actions) != 1 || rpc.actions[0].Verb != tc.wantVerb {
				t.Fatalf("reports = %+v, want one %s", rpc.actions, tc.wantVerb)
			}
			field := sessionreport.FieldState
			if tc.wantVerb == sessionreport.Notice {
				field = sessionreport.FieldNotice
			}
			if got := rpc.actions[0].Fields[field]; got != tc.wantField {
				t.Fatalf("%s = %q, want %q", field, got, tc.wantField)
			}
		})
	}
}

func TestNoticeIsBounded(t *testing.T) {
	rpc := successfulReportRPC(t)
	withHookStdin(t, `{"hook_event_name":"Notification","message":"`+strings.Repeat("é", 400)+`"}`, func() {
		if err := cmdAgentStatus([]string{core.StateWaiting}, true); err != nil {
			t.Fatal(err)
		}
	})
	if len(rpc.actions) != 1 {
		t.Fatalf("reports = %+v", rpc.actions)
	}
	if n := len(rpc.actions[0].Fields[sessionreport.FieldNotice]); n > sessionreport.MaxNoticeBytes || n == 0 {
		t.Fatalf("notice is %d bytes, want 1..%d", n, sessionreport.MaxNoticeBytes)
	}
}
