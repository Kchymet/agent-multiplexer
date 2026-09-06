package main

import (
	"encoding/base64"
	"io"
	"os"
	"testing"

	"amux/internal/core"
)

// withHookStdin runs fn with payload piped on stdin, the way Claude Code invokes
// a hook command.
func withHookStdin(t *testing.T, payload string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = orig; r.Close() }()
	if _, err := w.WriteString(payload); err != nil {
		t.Fatal(err)
	}
	w.Close()
	fn()
}

// TestAgentPermissionJournalsTheHookLifecycle drives `amux agent permission` with
// the payloads Claude pipes to it and checks the journal that comes out — the
// producer end of the permission_request events a remote orchestrator answers.
// Identity and the tool come from the hook JSON, exactly as in a real session.
func TestAgentPermissionJournalsTheHookLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AMUX_SESSION_ID", "")
	const session = "33333333-3333-4333-8333-333333333333"
	request := `{"session_id":"` + session + `","hook_event_name":"PermissionRequest",` +
		`"tool_name":"Bash","tool_input":{"command":"rm -rf build/"}}`
	postTool := `{"session_id":"` + session + `","hook_event_name":"PostToolUse","tool_name":"Bash"}`

	withHookStdin(t, request, func() {
		if err := cmdAgentPermission([]string{"request"}); err != nil {
			t.Fatalf("request: %v", err)
		}
	})
	open := core.PendingPermissions(session)
	if len(open) != 1 {
		t.Fatalf("pending = %+v, want the one request the hook opened", open)
	}
	if open[0].Tool != "Bash" || open[0].Action != "rm -rf build/" {
		t.Errorf("request = %+v, want the tool and its command as the action", open[0])
	}
	if open[0].RequestID == "" {
		t.Error("the request must carry an id: it is what a permission verb quotes back")
	}
	// Only the options amux can actually deliver to the prompt are offered.
	if len(open[0].Options) != 2 ||
		open[0].Options[0] != core.PermissionAllow || open[0].Options[1] != core.PermissionDeny {
		t.Errorf("options = %v, want allow/deny", open[0].Options)
	}

	withHookStdin(t, postTool, func() {
		if err := cmdAgentPermission([]string{core.PermissionAllow}); err != nil {
			t.Fatalf("allow: %v", err)
		}
	})
	if got := core.PendingPermissions(session); len(got) != 0 {
		t.Fatalf("the tool ran, so its prompt must be resolved; still open: %+v", got)
	}

	// The turn boundary clears whatever survived — a denial Claude reported through
	// no hook we listen on, say.
	withHookStdin(t, request, func() {
		if err := cmdAgentPermission([]string{"request"}); err != nil {
			t.Fatalf("second request: %v", err)
		}
	})
	withHookStdin(t, `{"session_id":"`+session+`","hook_event_name":"Stop"}`, func() {
		if err := cmdAgentPermission([]string{"clear"}); err != nil {
			t.Fatalf("clear: %v", err)
		}
	})
	if got := core.PendingPermissions(session); len(got) != 0 {
		t.Fatalf("clear must leave nothing open, got %+v", got)
	}
}

// TestAgentPermissionNeverDisrupts: like every `amux agent` verb, this one must
// exit 0 whatever it is handed — a hook that fails would interrupt the agent it
// is only meant to observe.
func TestAgentPermissionNeverDisrupts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AMUX_SESSION_ID", "")
	cases := []struct {
		name    string
		args    []string
		payload string
	}{
		{"no verb", nil, `{"session_id":"s1"}`},
		{"unknown verb", []string{"detonate"}, `{"session_id":"s1"}`},
		{"no session to key on", []string{"request"}, `{"hook_event_name":"PermissionRequest"}`},
		{"unparsable payload", []string{"request"}, `not json`},
		{"resolving with nothing open", []string{core.PermissionAllow}, `{"session_id":"s1"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withHookStdin(t, tc.payload, func() {
				if err := cmdAgentPermission(tc.args); err != nil {
					t.Errorf("cmdAgentPermission(%v) = %v, want no error", tc.args, err)
				}
			})
		})
	}
	if got := core.PendingPermissions("s1"); len(got) != 0 {
		t.Errorf("no malformed call should have journaled anything, got %+v", got)
	}
}

func TestAgentModelRecordsStatusLineSelection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AMUX_SESSION_ID", "")
	const session = "33333333-3333-4333-8333-333333333333"
	withHookStdin(t, `{"session_id":"`+session+`","model":{"id":"claude-opus-4-7","display_name":"Opus"}}`, func() {
		if err := cmdAgentModel([]string{"--statusline"}); err != nil {
			t.Fatal(err)
		}
	})
	got, ok := core.RuntimeModel(session)
	if !ok || got.Model != "claude-opus-4-7" {
		t.Fatalf("RuntimeModel = %+v, %v", got, ok)
	}
}

func TestAgentModelForwardsExistingStatusLine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AMUX_SESSION_ID", "")
	payload := `{"session_id":"s1","model":{"id":"claude-sonnet-4-6"}}`
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	withHookStdin(t, payload, func() {
		encoded := base64.RawURLEncoding.EncodeToString([]byte("cat"))
		if err := cmdAgentModel([]string{"--statusline", "--forward-base64=" + encoded}); err != nil {
			t.Fatal(err)
		}
	})
	w.Close()
	os.Stdout = orig
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Fatalf("forwarded stdout = %q, want exact original payload", got)
	}
}
