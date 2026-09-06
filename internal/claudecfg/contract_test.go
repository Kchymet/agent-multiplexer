package claudecfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"amux/internal/core"
)

// TestHookPayloadContract pins the shape of the JSON Claude Code pipes to amux's
// hook commands on stdin, against a recorded payload (testdata/hook_payload.json).
// amux reads session_id/cwd for status and transcript_path/hook_event_name for
// capture; if an upstream Claude release renamed any of these, this fails loudly
// rather than the rail silently losing status and the capture diagnostic going dark.
func TestHookPayloadContract(t *testing.T) {
	b, err := os.ReadFile("testdata/hook_payload.json")
	if err != nil {
		t.Fatal(err)
	}
	var p HookPayload
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("hook payload no longer unmarshals into HookPayload: %v", err)
	}
	if p.SessionID != "33333333-3333-4333-8333-333333333333" {
		t.Errorf("session_id = %q", p.SessionID)
	}
	if p.Cwd != "/home/u/work" {
		t.Errorf("cwd = %q", p.Cwd)
	}
	if p.HookEventName != "Stop" {
		t.Errorf("hook_event_name = %q", p.HookEventName)
	}
	if p.TranscriptPath == "" {
		t.Error("transcript_path did not parse")
	}
}

// TestHookEventStateContract pins the authoritative hook-event → activity-state
// table (Claude runs `amux agent hook <state>` on each event). A rename or a
// meaning change of a Claude hook event would silently corrupt rail status; this
// makes it a test failure. Every listed event must map, and the mapping is exact.
func TestHookEventStateContract(t *testing.T) {
	want := map[string]string{
		"SessionStart":     core.StateReady,
		"UserPromptSubmit": core.StateRunning,
		"Notification":     core.StateWaiting,
		"Stop":             core.StateReady,
		"SessionEnd":       core.StateIdle,
	}
	for event, wantState := range want {
		got, ok := HookEventState(event)
		if !ok {
			t.Errorf("HookEventState(%q): not listed, want %q", event, wantState)
			continue
		}
		if got != wantState {
			t.Errorf("HookEventState(%q) = %q, want %q", event, got, wantState)
		}
	}
	// The exported ordered list must match exactly (no event added/removed silently).
	names := HookEventNames()
	if len(names) != len(want) {
		t.Fatalf("HookEventNames() has %d events, want %d: %v", len(names), len(want), names)
	}
	for _, n := range names {
		if _, ok := want[n]; !ok {
			t.Errorf("HookEventNames() lists unexpected event %q", n)
		}
	}
	// An unknown event is reported as unlisted, not silently mapped.
	if _, ok := HookEventState("SomeFutureEvent"); ok {
		t.Error("HookEventState(unknown) reported ok=true")
	}
}

// TestPermissionHookContract pins the Claude hook events amux drives its
// permission journal from, and the verb each runs. This table is the entire
// producer of the permission_request events a remote orchestrator answers
// (docs/remote-provider-sessions.md §4.5) — Claude's transcript records no prompt
// — so an upstream rename here silently costs a consumer the ability to approve
// anything. Failing loudly is the point.
func TestPermissionHookContract(t *testing.T) {
	want := map[string]string{
		"PermissionRequest": PermissionVerbRequest,
		"PostToolUse":       core.PermissionAllow,
		"PermissionDenied":  core.PermissionDeny,
		"Stop":              PermissionVerbClear,
		"SessionEnd":        PermissionVerbClear,
	}
	for event, wantVerb := range want {
		got, ok := PermissionHookVerb(event)
		if !ok {
			t.Errorf("PermissionHookVerb(%q): not listed, want %q", event, wantVerb)
			continue
		}
		if got != wantVerb {
			t.Errorf("PermissionHookVerb(%q) = %q, want %q", event, got, wantVerb)
		}
	}
	events := PermissionHookEvents()
	if len(events) != len(want) {
		t.Fatalf("PermissionHookEvents() has %d events, want %d: %v", len(events), len(want), events)
	}
	for _, e := range events {
		if _, ok := want[e]; !ok {
			t.Errorf("PermissionHookEvents() lists unexpected event %q", e)
		}
	}
	if _, ok := PermissionHookVerb("SomeFutureEvent"); ok {
		t.Error("PermissionHookVerb(unknown) reported ok=true")
	}
}

// TestPermissionHookPayloadContract pins the extra fields the tool hook events
// carry, against a recorded PermissionRequest payload. amux reads tool_name to
// know what is being asked for and tool_input to summarize it onto the approve
// card; a rename would leave every card blank.
func TestPermissionHookPayloadContract(t *testing.T) {
	b, err := os.ReadFile("testdata/permission_request_payload.json")
	if err != nil {
		t.Fatal(err)
	}
	var p HookPayload
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("PermissionRequest payload no longer unmarshals into HookPayload: %v", err)
	}
	if p.HookEventName != "PermissionRequest" {
		t.Errorf("hook_event_name = %q", p.HookEventName)
	}
	if p.SessionID == "" {
		t.Error("session_id did not parse: the journal would have no session to key on")
	}
	if p.ToolName != "Bash" {
		t.Errorf("tool_name = %q, want Bash", p.ToolName)
	}
	if got := SummarizeToolInput(p.ToolInput); got != "rm -rf build/" {
		t.Errorf("SummarizeToolInput = %q, want the command", got)
	}
	// An input amux has no known field for still says something rather than nothing.
	if got := SummarizeToolInput(json.RawMessage(`{"unknown":"x"}`)); got == "" {
		t.Error("an unknown tool input must fall back to its JSON, not an empty action")
	}
}

// TestMungeDrift verifies the doctor drift probe's engine: a transcript stored
// under a project dir that matches ProjectDirName(cwd) is clean, while one stored
// under a different dir — as would happen if Claude changed its munge scheme — is
// flagged. This is what turns a silent resume/capture breakage into a loud report.
func TestMungeDrift(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	projects := filepath.Join(dir, "projects")

	write := func(project, uuid, cwd string) {
		d := filepath.Join(projects, project)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, uuid+".jsonl"), []byte(`{"cwd":"`+cwd+`"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Correct: the project dir is exactly ProjectDirName(cwd).
	write(ProjectDirName("/home/u/work"), "11111111-1111-4111-8111-111111111111", "/home/u/work")
	// Also correct: the project dir is munge of an ANCESTOR of cwd — amux launched
	// the agent in /home/u/agent and it cd'd into the repo worktree beneath. This is
	// a normal cwd shift, not scheme drift, and must not be flagged.
	write(ProjectDirName("/home/u/agent"), "44444444-4444-4444-8444-444444444444", "/home/u/agent/repo")
	// Actual Claude layout: underscores in cwd become hyphens too.
	write("-home-u-harness--ws-permission-deny", "55555555-5555-4555-8555-555555555555", "/home/u/harness/_ws/permission_deny")
	if d := MungeDrift(); len(d) != 0 {
		t.Fatalf("no drift expected for a matching or ancestor layout, got %v", d)
	}

	// Drifted: transcript stored under a name that is NOT ProjectDirName(cwd).
	write("some-other-scheme", "22222222-2222-4222-8222-222222222222", "/home/u/other")
	drift := MungeDrift()
	if len(drift) != 1 {
		t.Fatalf("expected exactly one drift finding, got %d: %v", len(drift), drift)
	}
}

// TestProjectDirMungeContract pins the load-bearing project-dir path munge Claude
// uses to locate a session's transcript: '/', '.', and '_' become '-'. Resume detection,
// gap-fill, and transcript listing all depend on reproducing it exactly.
func TestProjectDirMungeContract(t *testing.T) {
	cases := map[string]string{
		"/home/u/work":                        "-home-u-work",
		"/home/u/.local/share/amux/x":         "-home-u--local-share-amux-x",
		"/home/u/harness/_ws/permission_deny": "-home-u-harness--ws-permission-deny",
		"/tmp/scratchpad/wsHost_default":      "-tmp-scratchpad-wsHost-default",
	}
	for cwd, want := range cases {
		if got := ProjectDirName(cwd); got != want {
			t.Errorf("ProjectDirName(%q) = %q, want %q", cwd, got, want)
		}
	}
}

func TestFindSessionWithUnderscores(t *testing.T) {
	home := At(t.TempDir())
	cwd := "/home/u/harness/_ws/permission_deny"
	id := "55555555-5555-4555-8555-555555555555"
	// Use Claude's observed folder spelling rather than the implementation under test.
	writeSession(t, home.ProjectsRoot(), "-home-u-harness--ws-permission-deny", id, `{"cwd":"`+cwd+`"}`)
	if got, ok := home.FindSession(id, cwd); !ok || got != cwd {
		t.Fatalf("FindSession = %q, %v; want %q, true", got, ok, cwd)
	}
	if _, err := os.Stat(home.TranscriptPath(cwd, id)); err != nil {
		t.Fatalf("transcript lookup: %v", err)
	}
}
