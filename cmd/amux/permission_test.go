package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"

	"amux/internal/core"
	"amux/internal/sessionreport"
	"amux/internal/sessionrpc"
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

func successfulReportRPC(t *testing.T) *fakeRestrictedRPC {
	t.Helper()
	result, err := json.Marshal(core.Result{Type: "result", OK: true})
	if err != nil {
		t.Fatal(err)
	}
	rpc := &fakeRestrictedRPC{
		query:  sessionrpc.Result{Status: sessionrpc.StatusOK, Body: []byte(`{"runtime_generation":"gen-1"}`)},
		action: sessionrpc.Result{Status: sessionrpc.StatusOK, Body: result},
	}
	installRestrictedRPC(t, rpc)
	return rpc
}

// TestAgentPermissionReportsObservationWithoutAuthority verifies the CLI sends
// bounded hook facts through fixed-context RPC and never writes the answerable
// permission journal. Hook session_id is intentionally irrelevant to authority.
func TestAgentPermissionReportsObservationWithoutAuthority(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AMUX_SESSION_ID", "")
	rpc := successfulReportRPC(t)
	const session = "33333333-3333-4333-8333-333333333333"
	request := `{"session_id":"` + session + `","hook_event_name":"PermissionRequest",` +
		`"tool_name":"Bash","tool_input":{"command":"rm -rf build/"}}`
	postTool := `{"session_id":"` + session + `","hook_event_name":"PostToolUse","tool_name":"Bash"}`

	withHookStdin(t, request, func() {
		if err := cmdAgentPermission([]string{"request", "--hook"}); err != nil {
			t.Fatalf("request: %v", err)
		}
	})
	if len(rpc.actions) != 1 || rpc.actions[0].Verb != sessionreport.PermissionRequest {
		t.Fatalf("actions = %+v", rpc.actions)
	}
	fields := rpc.actions[0].Fields
	if fields[sessionreport.FieldTool] != "Bash" || fields[sessionreport.FieldAction] != "rm -rf build/" {
		t.Errorf("fields = %+v, want tool/action observation", fields)
	}
	if fields[sessionreport.FieldRequestID] == "" || fields[sessionreport.FieldRuntimeGeneration] != "gen-1" {
		t.Errorf("fields = %+v, want random request correlation and daemon-issued generation", fields)
	}
	if got := core.PendingPermissions(session); len(got) != 0 {
		t.Fatalf("self observation entered answerable permission journal: %+v", got)
	}

	withHookStdin(t, postTool, func() {
		if err := cmdAgentPermission([]string{core.PermissionAllow, "--hook"}); err != nil {
			t.Fatalf("allow: %v", err)
		}
	})
	if len(rpc.actions) != 2 || rpc.actions[1].Verb != sessionreport.PermissionResolved ||
		rpc.actions[1].Fields[sessionreport.FieldDecision] != core.PermissionAllow {
		t.Fatalf("resolution action = %+v", rpc.actions)
	}
}

// TestAgentPermissionNeverDisrupts: like every telemetry hook, this one must
// exit 0 whatever it is handed — a hook that fails would interrupt the agent it
// is only meant to observe.
func TestAgentPermissionNeverDisrupts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AMUX_SESSION_ID", "")
	successfulReportRPC(t)
	cases := []struct {
		name    string
		args    []string
		payload string
	}{
		{"unknown verb", []string{"detonate", "--hook"}, `{"session_id":"s1"}`},
		{"invalid flag before hook marker", []string{"request", "--unknown", "--hook"}, `{"hook_event_name":"PermissionRequest"}`},
		{"no reported session needed", []string{"request", "--hook"}, `{"hook_event_name":"PermissionRequest","tool_name":"Bash"}`},
		{"unparsable payload", []string{"request", "--hook"}, `not json`},
		{"mismatched event", []string{core.PermissionAllow, "--hook"}, `{"hook_event_name":"PermissionRequest","tool_name":"Bash"}`},
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

func TestExplicitReportsPreserveErrorsWhileHooksAreNondisruptive(t *testing.T) {
	rpc := &fakeRestrictedRPC{queryErr: errors.New("fixed context unavailable")}
	installRestrictedRPC(t, rpc)
	if err := cmdAgentStatus([]string{core.StateRunning}, false); err == nil {
		t.Fatal("explicit status hid fixed-context failure")
	}
	withHookStdin(t, `{"hook_event_name":"UserPromptSubmit","session_id":"forged"}`, func() {
		if err := cmdAgentStatus([]string{core.StateRunning}, true); err != nil {
			t.Fatalf("generated activity hook disrupted runtime: %v", err)
		}
	})
	if err := cmdAgentCapture([]string{"Stop"}); err == nil {
		t.Fatal("explicit capture hid fixed-context failure")
	}
	withHookStdin(t, `{"hook_event_name":"Stop","transcript_path":"/host/foreign"}`, func() {
		if err := cmdAgentCapture([]string{"--hook"}); err != nil {
			t.Fatalf("generated capture hook disrupted runtime: %v", err)
		}
	})
}

func TestCaptureHookDoesNotForwardUUIDOrPath(t *testing.T) {
	rpc := successfulReportRPC(t)
	withHookStdin(t, `{"hook_event_name":"Stop","session_id":"foreign","transcript_path":"/host/foreign"}`, func() {
		if err := cmdAgentCapture([]string{"--hook"}); err != nil {
			t.Fatal(err)
		}
	})
	if len(rpc.actions) != 1 || rpc.actions[0].Verb != sessionreport.Capture {
		t.Fatalf("capture actions = %+v", rpc.actions)
	}
	fields := rpc.actions[0].Fields
	if len(fields) != 2 || fields[sessionreport.FieldEvent] != "Stop" || fields[sessionreport.FieldRuntimeGeneration] != "gen-1" {
		t.Fatalf("capture forwarded untrusted identity/path fields: %+v", fields)
	}
}

func TestLegacyGeneratedHookSpellingsRemainNondisruptive(t *testing.T) {
	rpc := successfulReportRPC(t)
	withHookStdin(t, `{"hook_event_name":"PermissionRequest","session_id":"foreign","tool_name":"Bash"}`, func() {
		if err := cmdAgentPermission([]string{"request"}); err != nil {
			t.Fatalf("legacy permission hook: %v", err)
		}
	})
	withHookStdin(t, `{"hook_event_name":"Stop","session_id":"foreign","transcript_path":"/host/foreign"}`, func() {
		if err := cmdAgentCapture(nil); err != nil {
			t.Fatalf("legacy capture hook: %v", err)
		}
	})
	if len(rpc.actions) != 2 || rpc.actions[0].Verb != sessionreport.PermissionRequest || rpc.actions[1].Verb != sessionreport.Capture {
		t.Fatalf("legacy hook reports = %+v", rpc.actions)
	}
	rpc.queryErr = errors.New("daemon unavailable")
	withHookStdin(t, `{"hook_event_name":"Stop"}`, func() {
		if err := cmdAgentCapture(nil); err != nil {
			t.Fatalf("legacy capture hook exposed report failure: %v", err)
		}
	})
}

func TestAgentModelRecordsStatusLineSelection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AMUX_SESSION_ID", "")
	rpc := successfulReportRPC(t)
	const session = "33333333-3333-4333-8333-333333333333"
	withHookStdin(t, `{"session_id":"`+session+`","model":{"id":"claude-opus-4-7","display_name":"Opus"}}`, func() {
		if err := cmdAgentModel([]string{"--statusline"}); err != nil {
			t.Fatal(err)
		}
	})
	if len(rpc.actions) != 1 || rpc.actions[0].Verb != sessionreport.Model ||
		rpc.actions[0].Fields[sessionreport.FieldModel] != "claude-opus-4-7" {
		t.Fatalf("model report = %+v", rpc.actions)
	}
	if _, ok := core.RuntimeModel(session); ok {
		t.Fatal("status line wrote the legacy UUID-only model record")
	}
}

func TestAgentModelForwardsExistingStatusLine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AMUX_SESSION_ID", "")
	successfulReportRPC(t)
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
