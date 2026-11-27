package claudecfg

import (
	"encoding/json"
	"testing"
)

// "waiting" means blocked on the user. Only a Notification that is a prompt
// implies it; the idle reminder after a finished turn, a login notice and any
// other non-prompt leave the state alone, and an unknown type is never read as
// waiting (it is reported for the daemon log instead).
func TestClassifyNotification(t *testing.T) {
	cases := []struct {
		name string
		p    HookPayload
		want NotificationClass
	}{
		// Truly blocked on the user.
		{"permission", HookPayload{NotificationType: "permission_prompt", Message: "Claude needs your permission to use Bash"}, NotificationBlocking},
		{"elicitation", HookPayload{NotificationType: "elicitation_dialog"}, NotificationBlocking},
		{"elicitation url", HookPayload{NotificationType: "elicitation_url_dialog"}, NotificationBlocking},
		{"teammate needs input", HookPayload{NotificationType: "agent_needs_input"}, NotificationBlocking},
		// Idle or informational: the state stands.
		{"idle reminder", HookPayload{NotificationType: "idle_prompt", Message: "Claude is waiting for your input"}, NotificationIgnored},
		{"login", HookPayload{NotificationType: "auth_success"}, NotificationIgnored},
		{"login storage failure", HookPayload{NotificationType: "auth_storage_failure"}, NotificationIgnored},
		{"elicitation done", HookPayload{NotificationType: "elicitation_complete"}, NotificationIgnored},
		{"teammate done", HookPayload{NotificationType: "agent_completed"}, NotificationIgnored},
		{"plugin", HookPayload{NotificationType: "plugin_notification", Message: "needs your permission"}, NotificationIgnored},
		// Unknown: never waiting, reported for the log.
		{"future type", HookPayload{NotificationType: "some_future_prompt"}, NotificationUnrecognized},
		// Missing type (a Claude Code that predates notification_type).
		{"legacy idle", HookPayload{Message: "Claude is waiting for your input"}, NotificationIgnored},
		{"legacy permission", HookPayload{Message: "Claude needs your permission to use Bash"}, NotificationBlocking},
		{"legacy other", HookPayload{Message: "Something else happened"}, NotificationUnrecognized},
		{"empty", HookPayload{}, NotificationUnrecognized},
	}
	for _, tc := range cases {
		if got := ClassifyNotification(tc.p); got != tc.want {
			t.Errorf("%s: ClassifyNotification(%+v) = %v, want %v", tc.name, tc.p, got, tc.want)
		}
		if got, want := NotificationNeedsUser(tc.p), tc.want == NotificationBlocking; got != want {
			t.Errorf("%s: NotificationNeedsUser = %v, want %v", tc.name, got, want)
		}
	}
}

// The Notification payload field names are load-bearing: pin them against the
// documented shape (https://code.claude.com/docs/en/hooks, Notification input).
func TestNotificationPayloadContract(t *testing.T) {
	raw := `{"session_id":"abc123","transcript_path":"/t.jsonl","cwd":"/w","hook_event_name":"Notification",` +
		`"message":"Claude is waiting for your input","title":"Claude Code","notification_type":"idle_prompt"}`
	var p HookPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	if p.HookEventName != "Notification" || p.NotificationType != "idle_prompt" || p.Message != "Claude is waiting for your input" {
		t.Fatalf("payload = %+v", p)
	}
	if ClassifyNotification(p) != NotificationIgnored {
		t.Fatal("the idle reminder must not read as waiting")
	}
}

// The AskUserQuestion status hook is installed with its matcher, so ordinary
// tool calls never spawn a waiting report, and reinstalling does not stack it.
func TestQuestionHookInstalledWithMatcher(t *testing.T) {
	root := map[string]any{}
	for i := 0; i < 2; i++ {
		b, err := encodeHooks(root, "/opt/amux", nil, false)
		if err != nil {
			t.Fatal(err)
		}
		root = map[string]any{}
		if err := json.Unmarshal(b, &root); err != nil {
			t.Fatal(err)
		}
	}
	groups, _ := root["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(groups) != 2 {
		t.Fatalf("PreToolUse groups = %v, want the question status hook and the question hold", groups)
	}
	g := groups[0].(map[string]any)
	if g["matcher"] != "AskUserQuestion" || groupCommand(g) != "/opt/amux agent hook waiting" {
		t.Fatalf("PreToolUse group = %v", g)
	}
	if h := groups[1].(map[string]any); h["matcher"] != "AskUserQuestion" || groupCommand(h) != "/opt/amux agent question hold" {
		t.Fatalf("PreToolUse hold group = %v", h)
	}
	for _, event := range []string{"PostToolUse", "Notification", "Stop"} {
		for _, g := range root["hooks"].(map[string]any)[event].([]any) {
			if _, ok := g.(map[string]any)["matcher"]; ok {
				t.Errorf("%s group %v has a matcher", event, g)
			}
		}
	}
}
