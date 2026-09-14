//go:build linux

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/launchenv"
	"amux/internal/panespec"
	"amux/internal/store"
)

// This fixture runs the compiled CLI in the production panespec namespace and
// uses the production daemon mailbox dispatch and real SQLite store. It needs
// no socket, host daemon, model credentials, or permission escalation. The test
// process is already inside the harness sandbox when invoked from an agent.
// It does not substitute for real Claude/Codex tool-turn acceptance.
func TestAgentNamespaceCommandSurface(t *testing.T) {
	testAgentCommandSurface(t, "shell")
}

// Opt-in: pinned native runtimes are external test dependencies, never host
// account state. A local deterministic provider requests an ordinary tool call.
func TestAgentRuntimeCommandSurface(t *testing.T) {
	mode := os.Getenv("AMUX_TEST_AGENT_RUNTIME")
	if mode == "" {
		t.Skip("set AMUX_TEST_AGENT_RUNTIME to claude, codex, or codex-app-server")
	}
	switch mode {
	case "claude", "codex", "codex-app-server":
	default:
		t.Fatalf("unknown runtime %q", mode)
	}
	testAgentCommandSurface(t, mode)
}

func testAgentCommandSurface(t *testing.T, mode string) {
	if err := panespec.IsolationSupport(); err != nil {
		if os.Getenv("AMUX_REQUIRE_NAMESPACE_TEST") == "1" {
			t.Fatal(err)
		}
		t.Skipf("protected namespace unavailable: %v", err)
	}
	candidate := filepath.Join(t.TempDir(), "amux")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", candidate, "../../cmd/amux")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	isolateHome(t)
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("CODEX_HOME", filepath.Join(os.Getenv("HOME"), ".codex"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(os.Getenv("HOME"), "state"))
	own := store.Session{ID: "own", RootID: "root", Agent: "claude", Dir: filepath.Join(core.SessionsDir(), "root", "own"), ClaudeID: "11111111-1111-4111-8111-111111111111"}
	peer := store.Session{ID: "peer", RootID: "other", Agent: "claude", Dir: filepath.Join(core.SessionsDir(), "other", "peer"), ClaudeID: "22222222-2222-4222-8222-222222222222"}
	for _, s := range []store.Session{own, peer} {
		if err := os.MkdirAll(s.Dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := claudecfg.At(claudecfg.AgentHome(s.Dir)).TranscriptPath(s.Dir, s.ClaudeID)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(`{"cwd":"`+s.Dir+`"}`+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []store.Session{own, peer} {
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	authorityRoot := filepath.Join(t.TempDir(), "authority")
	d := New("", nil, time.Hour)
	d.authority, err = access.Open(authorityRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { d.authority.Close() }()
	publishTestRuntime(t, d, own.ID)
	r := newSessionRuntime(d)
	grant, err := d.authority.EnsureSession(context.Background(), own.ID, own.Dir)
	if err != nil {
		t.Fatal(err)
	}
	start := func() (context.CancelFunc, chan struct{}) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		if err := r.open(ctx, own); err != nil {
			t.Fatal(err)
		}
		go func() { defer close(done); r.run(ctx) }()
		return cancel, done
	}
	cancel, stopped := start()
	defer func() { cancel(); <-stopped; r.close() }()
	script := `set -eux
test -e /amux-session-access/context.json
# The authenticated context must remain read-only in every launch mode.
if touch /amux-session-access/tamper 2>/dev/null; then exit 30; fi
for help in help -h --help; do amux agent "$help" 2>/dev/null; done
amux agent 2>/dev/null
for state in idle ready waiting running; do amux agent status "$state"; done
printf '%s' '{"session_id":"foreign","cwd":"/host"}' | amux agent hook running
printf '%s' '{}' | amux hook ready
amux agent model fixture-model
printf '%s' '{"model":{"id":"statusline-model"}}' | amux agent model --statusline --forward-base64 Y2F0 > forwarded
[ "$(cat forwarded)" = '{"model":{"id":"statusline-model"}}' ]
printf '%s' '{"model":{"id":"statusline-model"}}' | amux agent model --forward-base64=Y2F0 --statusline > forwarded
amux agent permission request --request-id fixture --tool=Bash --action 'synthetic command'
amux agent permission allow --request-id=fixture --tool Bash
amux agent permission request --request-id=denied --tool Bash
amux agent permission deny --request-id denied --tool=Bash
amux agent permission request --request-id clearing --tool Bash
amux agent permission clear
printf '%s' '{"hook_event_name":"PermissionRequest","tool_name":"Bash"}' | amux agent permission request --hook
printf '%s' '{"hook_event_name":"PostToolUse","tool_name":"Bash"}' | amux agent permission allow
printf '%s' '{"hook_event_name":"Stop"}' | amux agent permission clear --hook
printf '%s' '{"hook_event_name":"PermissionRequest","tool_name":"Bash"}' | amux agent permission request --hook
printf '%s' '{"hook_event_name":"PermissionDenied","tool_name":"Bash"}' | amux agent permission deny --hook
if [ "${AMUX_AGENT:-claude}" = claude ]; then
amux agent capture Stop
else
if amux agent capture Stop; then exit 31; fi
fi
printf '%s' '{"hook_event_name":"Stop","transcript_path":"/host/foreign"}' | amux agent capture --hook
printf '%s' '{"hook_event_name":"Stop"}' | amux agent capture
AMUX_WORKGROUP=peer AMUX_WORKSPACE=peer amux agent name First Name
unset AMUX_WORKGROUP AMUX_WORKSPACE AMUX_SESSION_ID
amux agent label Second Name
amux name Deprecated Alias
amux agent sessions > sessions.txt
amux agent sessions --json > sessions.json
grep -q 22222222-2222-4222-8222-222222222222 sessions.json
grep -q 22222222-2222-4222-8222-222222222222 sessions.txt
amux agent events --after=0 --json > events.json
amux agent events own --after 0 > events.txt
cursor=$(sed -n 's/.*"next_cursor": "\([^" ]*\)".*/\1/p' events.json)
[ -n "$cursor" ]
amux agent events --cursor="$cursor" --json > continued.json
amux agent events own --cursor "$cursor" > continued.txt
if amux do rename peer bad; then exit 21; fi
if amux do delete own; then exit 22; fi
if amux workgroup archive peer; then exit 23; fi
if amux agent done peer; then exit 24; fi
if amux agent status invalid; then exit 25; fi
if amux agent events peer --json; then exit 26; fi
# The peer path is metadata only: the production mount must still hide it.
if [ -e ../peer ] || [ -e ../../other/peer ]; then exit 27; fi
if touch /amux-session-access/tamper 2>/dev/null; then exit 28; fi
amux agent status ready & first=$!
amux agent model concurrent-model & second=$!
wait "$first"; wait "$second"
touch restart-ready
while [ ! -e retry-now ]; do sleep .02; done
# Publish against the old service while the daemon is stopped. Restart must
# settle safely; a lost pre-claim response may be retried by the CLI user.
if amux agent name Restarted > retry.out 2> retry.err; then
 echo accepted > retry.result
else
 echo uncertain > retry.result
fi
while [ ! -e restarted ]; do sleep .02; done
amux agent name AfterRestart
amux agent done
if amux agent done; then exit 29; fi
`
	if mode != "shell" {
		script = "set -eu\ntest \"$(cat /amux-inner-write-probe)\" = outer\nif echo inner > /amux-inner-write-probe 2>/dev/null; then exit 32; fi\n" + script
	}
	script += "touch surface-finished\n"
	scriptPath := filepath.Join(own.Dir, "surface.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	var runtime *surfaceRuntime
	if mode != "shell" {
		runtime = prepareSurfaceRuntime(t, mode, &own, scriptPath, candidate, grant)
		defer runtime.close()
		db, err := store.Open()
		if err != nil {
			t.Fatal(err)
		}
		if err := db.PutSession(own); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
	dir, env, argv, err := panespec.Resolve(panespec.LaunchSpec{Session: own, Access: grant}, panespec.TabTerminal)
	if err != nil {
		t.Fatal(err)
	}
	// panespec pins the running image. In this test that image is the Go test
	// binary; replace only its read-only bind sources with the compiled CLI.
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == "--ro-bind" && argv[i+1] == self {
			argv[i+1] = candidate
		}
	}
	if runtime == nil {
		argv = append(argv, scriptPath)
	} else {
		argv, env = runtime.launch(t, argv, env, own)
	}
	ctx, stop := context.WithTimeout(context.Background(), 60*time.Second)
	defer stop()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env, err = launchenv.Build(os.Environ(), env, launchenv.ForRuntime(own.Agent))
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	exited := make(chan error, 1)
	if runtime == nil {
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { exited <- cmd.Wait() }()
	} else {
		runtime.start(t, ctx, cmd, &output, exited)
	}
	ready := filepath.Join(own.Dir, "restart-ready")
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("surface stopped before restart: %v\n%s", err, output.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if record, ok := core.SessionHookState(own.ID, own.ClaudeID); !ok || record.State != core.StateReady || record.Cwd != own.Dir {
		t.Fatalf("activity did not reach the authoritative record: %+v", record)
	}
	if record, ok := core.SessionRuntimeModel(own.ID, own.ClaudeID); !ok || record.Model != "concurrent-model" {
		t.Fatalf("model did not reach the authoritative record: %+v", record)
	}
	if _, _, ok := core.SessionCapturedTranscript(own.ID, own.ClaudeID); own.Agent == "claude" && !ok {
		t.Fatal("capture did not reach daemon-owned storage")
	}
	if _, ok := core.SessionHookState(peer.ID, peer.ClaudeID); ok {
		t.Fatal("hook spoofing affected peer activity")
	}
	cancel()
	<-stopped
	r.close()
	d.authority.Close()
	if err := os.WriteFile(filepath.Join(own.Dir, "retry-now"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	// Wait for the exact offline publication instead of relying on scheduler
	// timing. Its old boot binding must not silently mutate after restart.
	published := false
	deadline := time.Now().Add(5 * time.Second)
	for !published && time.Now().Before(deadline) {
		entries, err := os.ReadDir(grant.RequestsHostDir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".req") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(grant.RequestsHostDir, entry.Name()))
			if err != nil {
				continue
			}
			var envelope access.SignedRequest
			if json.Unmarshal(body, &envelope) == nil && bytes.Contains(envelope.Body, []byte("Restarted")) {
				published = true
			}
		}
		if !published {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !published {
		t.Fatal("offline rename request was not published")
	}
	d = New("", nil, time.Hour)
	d.authority, err = access.Open(authorityRoot)
	if err != nil {
		t.Fatal(err)
	}
	publishTestRuntime(t, d, own.ID)
	r = newSessionRuntime(d)
	cancel, stopped = start()
	if err := os.WriteFile(filepath.Join(own.Dir, "restarted"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-exited; err != nil {
		t.Fatalf("surface: %v\n%s", err, output.String())
	}
	if runtime != nil {
		output.WriteString(runtime.result())
	}
	if !strings.Contains(output.String(), "marked done: archived own") {
		t.Fatalf("missing acknowledgement:\n%s", output.String())
	}
	db, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, ok, err := db.GetSession(own.ID)
	if err != nil || !ok || !got.Archived || got.Name != "AfterRestart" {
		t.Fatalf("own=%+v %v", got, err)
	}
	other, _, _ := db.GetSession(peer.ID)
	if other.Archived || other.Name != "" {
		t.Fatalf("foreign mutated: %+v", other)
	}
	t.Logf("%s: real CLI surface, peer discovery, self-only writes, concurrent reports, restart and archive acknowledgement passed inside production bubblewrap", mode)
}
