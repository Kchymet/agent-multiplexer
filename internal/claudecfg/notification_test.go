package claudecfg

import "testing"

// Only a Notification that blocks on the user implies "waiting"; the idle
// reminder after a finished turn and a login notice leave the state alone.
func TestNotificationNeedsUser(t *testing.T) {
	cases := []struct {
		p    HookPayload
		want bool
	}{
		{HookPayload{NotificationType: "permission_prompt", Message: "Claude needs your permission to use Bash"}, true},
		{HookPayload{NotificationType: "elicitation_dialog"}, true},
		{HookPayload{NotificationType: "some_future_prompt"}, true},
		{HookPayload{NotificationType: "idle_prompt", Message: "Claude is waiting for your input"}, false},
		{HookPayload{NotificationType: "auth_success"}, false},
		{HookPayload{Message: "Claude is waiting for your input"}, false},
		{HookPayload{Message: "Claude needs your permission to use Bash"}, true},
	}
	for _, tc := range cases {
		if got := NotificationNeedsUser(tc.p); got != tc.want {
			t.Errorf("NotificationNeedsUser(%+v) = %v, want %v", tc.p, got, tc.want)
		}
	}
}
