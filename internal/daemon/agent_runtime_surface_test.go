//go:build linux || darwin

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/accounts"
	"amux/internal/amuxcfg"
	"amux/internal/claudecfg"
	"amux/internal/codexapp"
	"amux/internal/codexcfg"
	"amux/internal/core"
	"amux/internal/panespec"
	"amux/internal/store"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

type surfaceRuntime struct {
	mode, command, dir, candidate string
	helperBin                     string
	grant                         access.SessionAccess
	server                        *httptest.Server
	mu                            sync.Mutex
	toolOutput                    string
	verified                      chan struct{}
	once                          sync.Once
	own                           store.Session
	env                           []string
	endpoint                      string
	supervisor                    *codexapp.Supervisor
}

func prepareSurfaceRuntime(t *testing.T, mode string, own *store.Session, script, candidate string, grant access.SessionAccess) *surfaceRuntime {
	t.Helper()
	r := &surfaceRuntime{mode: mode, command: "/bin/sh " + script, dir: own.Dir, candidate: candidate, grant: grant, verified: make(chan struct{})}
	// Each runtime below talks to a synthetic provider (a fixture endpoint and
	// key). Central auth pins official endpoints, so the fixture uses the
	// test-only environment seam compiled in with -tags amuxfixture.
	if !accounts.FixtureSourcesEnabled() {
		t.Fatal("native runtime surface tests need -tags amuxfixture")
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(os.Getenv("HOME"), "config"))
	harness := "codex"
	if mode == "claude" {
		harness = "claude"
	}
	if err := amuxcfg.SetAuthMode(harness, amuxcfg.AuthEnvironment); err != nil {
		t.Fatal(err)
	}
	// The shared namespace fixture creates a Claude row before choosing the
	// synthetic native runtime. Explicitly bind that test-only workload to its
	// chosen harness/provider; changing a global default must not reroute it.
	if err := accounts.Update(func(s *accounts.State) error {
		prior := s.Bindings[own.ID]
		s.Bindings[own.ID] = accounts.Binding{ProfileID: accounts.ID(harness, amuxcfg.AuthEnvironment), Harness: harness, Source: amuxcfg.AuthEnvironment, Revision: prior.Revision + 1, Origin: accounts.FixtureOrigin}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r.server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.server.Close)
	binDir := filepath.Join(own.Dir, "runtime-bin")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	if mode == "claude" {
		source := os.Getenv("AMUX_TEST_CLAUDE_BIN")
		if source == "" {
			t.Fatal("AMUX_TEST_CLAUDE_BIN must identify the pinned native Claude runtime")
		}
		target := filepath.Join(binDir, "claude")
		copyRuntime(t, source, target)
		t.Setenv("AMUX_CLAUDE_BIN", target)
		// CI builds bubblewrap in a private tool directory, which the production
		// PATH filter correctly removes. Package this exact dependency inside
		// the synthetic runtime just as we package the pinned native executable.
		if runtime.GOOS == "linux" {
			bw, err := exec.LookPath("bwrap")
			if err != nil {
				t.Fatal(err)
			}
			copyRuntime(t, bw, filepath.Join(binDir, "bwrap"))
			r.helperBin = binDir
			if source := os.Getenv("AMUX_TEST_SOCAT_ROOT"); source != "" {
				root := filepath.Join(binDir, "socat-package")
				copyRuntime(t, source, root)
				// Only the public library dependency is added to this helper process.
				wrapper := fmt.Sprintf("#!/bin/sh\nLD_LIBRARY_PATH=%s/usr/lib/x86_64-linux-gnu exec %s/usr/bin/socat \"$@\"\n", root, root)
				if err := os.WriteFile(filepath.Join(binDir, "socat"), []byte(wrapper), 0700); err != nil {
					t.Fatal(err)
				}
			}
		}
		// Use a synthetic provider and require the real Bash sandbox. No blanket
		// allowedTools entry or unsandboxed-command fallback is installed.
		settings := map[string]any{"sandbox": map[string]any{"enabled": true, "failIfUnavailable": true, "allowUnsandboxedCommands": false, "autoAllowBashIfSandboxed": true}, "permissions": map[string]any{"allow": []string{"Bash(" + r.command + ")"}}}
		writeRuntimeJSON(t, filepath.Join(own.Dir, ".amux", "claude", "settings.json"), settings)
		t.Setenv("ANTHROPIC_BASE_URL", r.server.URL)
		t.Setenv("ANTHROPIC_API_KEY", "synthetic-fixture-key")
		own.Model = "claude-sonnet-4-5"
		// The discovery stub is not a resumable Claude conversation. Start a
		// fresh real conversation with the launch planner's pinned --session-id.
		if err := os.Remove(claudecfg.At(claudecfg.AgentHome(own.Dir)).TranscriptPath(own.Dir, own.ClaudeID)); err != nil {
			t.Fatal(err)
		}
	} else {
		source := os.Getenv("AMUX_TEST_CODEX_PACKAGE")
		if source == "" {
			t.Fatal("AMUX_TEST_CODEX_PACKAGE must identify a pinned Codex package with sandbox helpers")
		}
		target := filepath.Join(binDir, "codex-package")
		copyRuntime(t, source, target)
		t.Setenv("AMUX_CODEX_BIN", filepath.Join(target, "bin", "codex"))
		own.Agent = "codex"
		own.Model = "stub-model"
		home := codexcfg.AgentHome(own.Dir)
		if err := os.MkdirAll(home, 0700); err != nil {
			t.Fatal(err)
		}
		config := fmt.Sprintf("check_for_update_on_startup = false\nmodel_provider = \"fixture\"\nmodel = \"stub-model\"\n[model_providers.fixture]\nname = \"fixture\"\nbase_url = %q\nwire_api = \"responses\"\nrequires_openai_auth = false\n", r.server.URL+"/v1")
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
			t.Fatal(err)
		}
		// A synthetic persisted rollout makes the launch planner and native runtime
		// resume the same store-authoritative UUID, without host conversation data.
		path := codexcfg.At(home).NewRolloutPath(own.ClaudeID)
		writeRuntimeJSON(t, path, map[string]any{"type": "session_meta", "timestamp": time.Now().UTC().Format(time.RFC3339), "payload": map[string]any{"id": own.ClaudeID, "timestamp": time.Now().UTC().Format(time.RFC3339), "cwd": own.Dir, "originator": "codex_cli_rs", "cli_version": "0.153.4", "source": "cli", "model_provider": "fixture"}})
	}
	r.own = *own
	return r
}

func copyRuntime(t *testing.T, source, target string) {
	t.Helper()
	if !filepath.IsAbs(source) {
		t.Fatal("runtime source must be absolute")
	}
	var err error
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-a", "--reflink=auto", source, target}
	if runtime.GOOS == "darwin" {
		args = []string{"-R", source, target}
	}
	if out, err := exec.Command("cp", args...).CombinedOutput(); err != nil {
		t.Fatalf("copy runtime: %v: %s", err, out)
	}
}
func writeRuntimeJSON(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func (r *surfaceRuntime) launch(t *testing.T, _ []string, _ []string, own store.Session) ([]string, []string) {
	t.Helper()
	spec := panespec.LaunchSpec{Session: own, Access: r.grant}
	var argv, env []string
	var err error
	if r.mode == "codex-app-server" {
		_, env, argv, r.endpoint, err = panespec.AppServerCommand(spec)
	} else {
		_, env, argv, err = panespec.Resolve(spec, panespec.TabAgent)
	}
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == "--ro-bind" && argv[i+1] == self {
			argv[i+1] = r.candidate
		}
	}
	if runtime.GOOS == "darwin" {
		replaceNativeTestTool(t, r.candidate, own.ID)
	}
	if r.mode == "claude" {
		argv = append(argv, "--print", "--verbose", "--output-format", "stream-json", "--permission-mode", "default", "Use Bash once to run the fixture command, then finish.")
	}
	// Prove the outer namespace permits this ephemeral root write. The
	// ordinary tool script must be denied the same write by the inner sandbox.
	for i := 0; i+3 < len(argv); i++ {
		if argv[i] == "--" && argv[i+2] == "--amux-payload-clean-exec" {
			prefix := append([]string(nil), argv[:i+3]...)
			prefix = append(prefix, "/bin/sh", "-c", `echo outer > /amux-inner-write-probe || exit 32; if [ -n "$1" ]; then PATH="$1:$PATH"; export PATH; fi; shift; exec "$@"`, "runtime", r.helperBin)
			argv = append(prefix, argv[i+3:]...)
			break
		}
	}
	r.env = env
	return argv, env
}

func (r *surfaceRuntime) start(t *testing.T, ctx context.Context, cmd *exec.Cmd, output *bytes.Buffer, exited chan error) {
	t.Helper()
	if r.mode == "codex-app-server" {
		r.supervisor = codexapp.New(codexapp.Config{SessionID: r.own.ID, Dir: r.dir, Env: r.env, Model: r.own.Model, Endpoint: r.endpoint, ResumeThreadID: r.own.ClaudeID, Sandbox: panespec.CodexSandboxForLaunch()})
		if err := r.supervisor.Start(ctx, cmd.Args); err != nil {
			t.Fatal(err)
		}
		go func() {
			err := r.supervisor.Prompt(ctx, "Run the fixture command once with exec_command.")
			if err == nil {
				err = r.requireVerified(ctx)
			}
			exited <- err
		}()
		return
	}
	if r.mode == "codex" {
		cmd.Stdout = nil
		cmd.Stderr = nil
		const rows, cols = 60, 160
		terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
		if err != nil {
			t.Fatal(err)
		}
		drained := make(chan struct{})
		ready := make(chan struct{})
		var readyOnce sync.Once
		screen := newCodexReadinessScreen(cols, rows, func(p []byte) {
			_, _ = terminal.Write(p)
		})
		go func() {
			defer close(drained)
			buf := make([]byte, 4096)
			for {
				n, err := terminal.Read(buf)
				if n > 0 {
					output.Write(buf[:n])
					_, _ = screen.Write(buf[:n])
					// Readiness is a property of the terminal picture, not
					// adjacency in the PTY byte stream. Codex may position the
					// footer fields with a control sequence split across reads.
					if codexNativeReady(screen.Render(), r.own.Model) {
						readyOnce.Do(func() { close(ready) })
					}
				}
				if err != nil {
					return
				}
			}
		}()
		go func() {
			select {
			case <-ready:
				_, _ = terminal.Write([]byte("\x1b[200~Run the fixture command once with exec_command.\x1b[201~\r"))
			case <-ctx.Done():
			}
			err := r.requireVerified(ctx)
			if err == nil && !strings.Contains(r.result(), "Process running with session ID") {
				err = fmt.Errorf("native fixture did not exercise yielded-process collection")
			}
			_ = cmd.Process.Kill()
			_ = terminal.Close()
			_ = cmd.Wait()
			<-drained
			screen.Close()
			exited <- err
		}()
		return
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		err := cmd.Wait()
		if err == nil {
			err = r.requireVerified(ctx)
		}
		exited <- err
	}()
}

// codexReadinessScreen is a test-only VT screen with an ordered response drain.
// Closing the emulator's reply writer first lets the reader exit before the
// emulator itself closes; this avoids relying on vterm's asynchronous lifecycle
// in a fixture whose PTY reader already supplies the serialization we need.
type codexReadinessScreen struct {
	*vt.Emulator
	responsesDone chan struct{}
}

func newCodexReadinessScreen(cols, rows int, respond func([]byte)) *codexReadinessScreen {
	s := &codexReadinessScreen{Emulator: vt.NewEmulator(cols, rows), responsesDone: make(chan struct{})}
	go func() {
		defer close(s.responsesDone)
		buf := make([]byte, 256)
		for {
			n, err := s.Read(buf)
			if n > 0 && respond != nil {
				respond(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return s
}

func (s *codexReadinessScreen) Close() {
	if closer, ok := s.InputPipe().(io.Closer); ok {
		_ = closer.Close()
	}
	<-s.responsesDone
	_ = s.Emulator.Close()
}

// codexNativeReady recognizes the configured model and approval mode as
// adjacent fields on one rendered footer row. The caller supplies an emulated
// screen, so ANSI styling, cursor movement, erasure, and chunk boundaries have
// already been interpreted exactly as a terminal would interpret them.
func codexNativeReady(screen, model string) bool {
	for _, line := range strings.Split(ansi.Strip(screen), "\n") {
		fields := strings.Fields(line)
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == model && fields[i+1] == "default" {
				return true
			}
		}
	}
	return false
}

func TestCodexNativeReadinessUsesRenderedTerminal(t *testing.T) {
	screen := newCodexReadinessScreen(80, 24, nil)
	defer screen.Close()

	// Deliberately split both text and the cursor-position sequence across
	// writes. The fields are separated in the byte stream, but adjacent in the
	// rendered footer; the coordinates are arbitrary and not part of readiness.
	chunks := [][]byte{
		[]byte("\x1b[7;5Hstub-mo"),
		[]byte("del\x1b["),
		[]byte("7;24Hdef"),
		[]byte("ault"),
	}
	for i, chunk := range chunks {
		if _, err := screen.Write(chunk); err != nil {
			t.Fatalf("parse chunk %d: %v", i, err)
		}
		if i+1 < len(chunks) && codexNativeReady(screen.Render(), "stub-model") {
			t.Fatalf("readiness reported before the complete footer after chunk %d", i)
		}
	}
	if !codexNativeReady(screen.Render(), "stub-model") {
		t.Fatalf("rendered footer not recognized:\n%s", ansi.Strip(screen.Render()))
	}
}
func (r *surfaceRuntime) requireVerified(ctx context.Context) error {
	select {
	case <-r.verified:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("runtime returned no successful ordinary tool result: %w", ctx.Err())
	}
}
func (r *surfaceRuntime) close() {
	if r.supervisor != nil {
		_ = r.supervisor.Close()
	}
	if r.server != nil {
		r.server.Close()
	}
}
func (r *surfaceRuntime) result() string { r.mu.Lock(); defer r.mu.Unlock(); return r.toolOutput }

func (r *surfaceRuntime) serve(w http.ResponseWriter, req *http.Request) {
	if req.Method != "POST" {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[]}`)
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(req.Body, 8<<20)).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if r.mode == "claude" {
		r.serveClaude(w, body)
		return
	}
	var input []map[string]any
	_ = json.Unmarshal(body["input"], &input)
	var combined strings.Builder
	lastOutput := ""
	outputs := 0
	for _, item := range input {
		if item["type"] == "function_call_output" {
			out, _ := item["output"].(string)
			combined.WriteString(out)
			combined.WriteByte('\n')
			lastOutput = out
			outputs++
		}
	}
	if outputs > 0 {
		// exec_command legitimately yields before the full CLI acceptance
		// script finishes. Poll the same process; never rerun its mutations.
		for _, line := range strings.Split(lastOutput, "\n") {
			if raw, ok := strings.CutPrefix(line, "Process running with session ID "); ok {
				id, err := strconv.Atoi(strings.TrimSpace(raw))
				if err != nil || id <= 0 {
					http.Error(w, "invalid fixture tool session", 400)
					return
				}
				r.codexToolCall(w, "write_stdin", fmt.Sprintf("call_fixture_poll_%d", outputs), map[string]any{
					"session_id": id, "chars": "", "yield_time_ms": 1000,
				})
				return
			}
		}
		if !strings.Contains(lastOutput, "Process exited with code 0") || !strings.Contains(combined.String(), "marked done: archived own") {
			http.Error(w, "fixture tool did not return confirmed success: "+lastOutput, 400)
			return
		}
		r.accept(combined.String())
		r.codexFinal(w)
		return
	}
	var tools []struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(body["tools"], &tools)
	found := false
	for _, tool := range tools {
		if tool.Name == "exec_command" {
			found = true
		}
	}
	if !found {
		http.Error(w, "exec_command not advertised", 400)
		return
	}
	// Exercise the asynchronous result contract on fast hosts as well as CI.
	r.codexToolCall(w, "exec_command", "call_fixture", map[string]any{"cmd": r.command, "workdir": r.dir, "shell": "/bin/sh", "login": false, "sandbox_permissions": "use_default", "yield_time_ms": 1})
}

func (r *surfaceRuntime) codexToolCall(w http.ResponseWriter, name, callID string, arguments map[string]any) {
	args, _ := json.Marshal(arguments)
	itemID := "fc_" + callID
	item := map[string]any{"id": itemID, "type": "function_call", "status": "completed", "call_id": callID, "name": name, "arguments": string(args)}
	writeSSE(w, false, []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_" + callID, "status": "in_progress"}},
		{"type": "response.output_item.added", "output_index": 0, "item": item},
		{"type": "response.function_call_arguments.delta", "item_id": itemID, "output_index": 0, "delta": string(args)},
		{"type": "response.function_call_arguments.done", "item_id": itemID, "output_index": 0, "arguments": string(args)},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": map[string]any{"id": "resp_" + callID, "status": "completed", "output": []any{item}}},
	})
}
func (r *surfaceRuntime) accept(output string) {
	r.mu.Lock()
	r.toolOutput = output
	r.mu.Unlock()
	r.once.Do(func() { close(r.verified) })
}
func (r *surfaceRuntime) codexFinal(w http.ResponseWriter) {
	item := map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "FIXTURE_OK"}}}
	writeSSE(w, false, []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_final", "status": "in_progress"}},
		{"type": "response.output_item.added", "output_index": 0, "item": item},
		{"type": "response.output_text.delta", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": "FIXTURE_OK"},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": map[string]any{"id": "resp_final", "status": "completed", "output": []any{item}}},
	})
}
func (r *surfaceRuntime) serveClaude(w http.ResponseWriter, body map[string]json.RawMessage) {
	var messages []struct {
		Content json.RawMessage `json:"content"`
	}
	_ = json.Unmarshal(body["messages"], &messages)
	success := false
	for _, msg := range messages {
		var parts []map[string]any
		_ = json.Unmarshal(msg.Content, &parts)
		for _, part := range parts {
			if part["type"] == "tool_result" {
				data, _ := json.Marshal(part["content"])
				if part["is_error"] == true || !bytes.Contains(data, []byte("marked done: archived own")) {
					http.Error(w, "fixture Bash failed: "+string(data), 400)
					return
				}
				r.accept(string(data))
				success = true
			}
		}
	}
	var advertised []struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(body["tools"], &advertised)
	hasBash := false
	for _, tool := range advertised {
		if tool.Name == "Bash" {
			hasBash = true
		}
	}
	events := []map[string]any{{"type": "message_start", "message": map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "model": "claude-sonnet-4-5", "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 1, "output_tokens": 0}}}}
	reason := "end_turn"
	if hasBash && !success {
		reason = "tool_use"
		args, _ := json.Marshal(map[string]any{"command": r.command, "description": "Run isolated amux CLI acceptance", "timeout": 60000})
		events = append(events, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu_fixture", "name": "Bash", "input": map[string]any{}}}, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(args)}})
	} else {
		events = append(events, map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}, map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "FIXTURE_OK"}})
	}
	events = append(events, map[string]any{"type": "content_block_stop", "index": 0}, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": reason, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}}, map[string]any{"type": "message_stop"})
	writeSSE(w, true, events)
}
func writeSSE(w http.ResponseWriter, named bool, events []map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		body, _ := json.Marshal(event)
		if named {
			fmt.Fprintf(w, "event: %s\n", event["type"])
		}
		fmt.Fprintf(w, "data: %s\n\n", body)
	}
	if !named {
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

// Replace the test executable published by panespec with the compiled CLI.
func replaceNativeTestTool(t *testing.T, candidate, sessionID string) {
	t.Helper()
	dest := core.SessionBinPath(sessionID)
	body, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	staged := dest + ".test"
	if err := os.WriteFile(staged, body, 0500); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(staged, dest); err != nil {
		t.Fatal(err)
	}
}

// Reproduce the native CI result: the long shell script yielded after ten
// seconds. A running result is neither a failed command nor confirmed success.
func TestSurfaceRuntimePollsYieldedCodexCommand(t *testing.T) {
	r := &surfaceRuntime{mode: "codex", verified: make(chan struct{})}
	input := []map[string]any{}
	call := func(out string) *httptest.ResponseRecorder {
		t.Helper()
		input = append(input, map[string]any{"type": "function_call_output", "output": out})
		body, _ := json.Marshal(map[string]any{"input": input})
		w := httptest.NewRecorder()
		r.serve(w, httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(body)))
		return w
	}
	for i, out := range []string{
		"Wall time: 10.0008 seconds\nProcess running with session ID 64966\nOutput:\namux agent events own --after 0",
		"Process running with session ID 64966\nOutput:\nmarked done: archived own",
	} {
		w := call(out)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"name":"write_stdin"`) || strings.Contains(w.Body.String(), `"name":"exec_command"`) {
			t.Fatalf("yield was failed or command restarted: %d %s", w.Code, w.Body)
		}
		var args map[string]any
		for _, line := range strings.Split(w.Body.String(), "\n") {
			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var event struct {
				Type      string `json:"type"`
				Arguments string `json:"arguments"`
				ItemID    string `json:"item_id"`
			}
			if json.Unmarshal([]byte(data), &event) == nil && event.Type == "response.function_call_arguments.done" {
				if event.ItemID != fmt.Sprintf("fc_call_fixture_poll_%d", i+1) {
					t.Fatal("poll reused its call id")
				}
				if err := json.Unmarshal([]byte(event.Arguments), &args); err != nil {
					t.Fatal(err)
				}
			}
		}
		if args["session_id"] != float64(64966) || args["chars"] != "" {
			t.Fatalf("poll changed the running process: %v", args)
		}
		select {
		case <-r.verified:
			t.Fatal("running tool accepted prematurely")
		default:
		}
	}
	w := call("Process exited with code 0\nOutput:")
	if w.Code != http.StatusOK {
		t.Fatalf("final result: %d %s", w.Code, w.Body)
	}
	select {
	case <-r.verified:
	default:
		t.Fatal("completed tool was not accepted")
	}
	if !strings.Contains(r.result(), "marked done: archived own") {
		t.Fatal("lost prior output")
	}
}

func TestSurfaceRuntimeRejectsUnconfirmedCodexCompletion(t *testing.T) {
	for _, out := range []string{
		"Process exited with code 0\nOutput: no acknowledgement",
		"Process exited with code 1\nmarked done: archived own",
		"Process running with session ID invalid",
	} {
		r := &surfaceRuntime{mode: "codex", verified: make(chan struct{})}
		body, _ := json.Marshal(map[string]any{"input": []any{map[string]any{"type": "function_call_output", "output": out}}})
		w := httptest.NewRecorder()
		r.serve(w, httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(body)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("unconfirmed tool accepted: %d %s", w.Code, w.Body)
		}
		select {
		case <-r.verified:
			t.Fatal("unconfirmed result accepted")
		default:
		}
	}
}
