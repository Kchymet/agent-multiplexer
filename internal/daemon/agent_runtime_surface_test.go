//go:build linux

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
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/claudecfg"
	"amux/internal/codexapp"
	"amux/internal/codexcfg"
	"amux/internal/panespec"
	"amux/internal/store"
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
	if out, err := exec.Command("cp", "-a", "--reflink=auto", source, target).CombinedOutput(); err != nil {
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
		r.supervisor = codexapp.New(codexapp.Config{SessionID: r.own.ID, Dir: r.dir, Env: r.env, Model: r.own.Model, Endpoint: r.endpoint, ResumeThreadID: r.own.ClaudeID})
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
		terminal, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 60, Cols: 160})
		if err != nil {
			t.Fatal(err)
		}
		drained := make(chan struct{})
		go func() { defer close(drained); _, _ = io.Copy(output, terminal) }()
		go func() {
			// The resumed interactive TUI accepts an ordinary prompt on its PTY.
			time.Sleep(1500 * time.Millisecond)
			_, _ = terminal.Write([]byte("Run the fixture command once with exec_command."))
			time.Sleep(300 * time.Millisecond)
			_, _ = terminal.Write([]byte("\r"))
			err := r.requireVerified(ctx)
			_ = cmd.Process.Kill()
			_ = terminal.Close()
			_ = cmd.Wait()
			<-drained
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
	for _, item := range input {
		if item["type"] == "function_call_output" {
			out, _ := item["output"].(string)
			if !strings.Contains(out, "Process exited with code 0") || !strings.Contains(out, "marked done: archived own") {
				http.Error(w, "fixture tool did not return confirmed success: "+out, 400)
				return
			}
			r.accept(out)
			r.codexFinal(w)
			return
		}
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
	args, _ := json.Marshal(map[string]any{"cmd": r.command, "workdir": r.dir, "shell": "/bin/sh", "login": false, "sandbox_permissions": "use_default", "yield_time_ms": 10000})
	item := map[string]any{"id": "fc_fixture", "type": "function_call", "status": "completed", "call_id": "call_fixture", "name": "exec_command", "arguments": string(args)}
	writeSSE(w, false, []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_fixture", "status": "in_progress"}},
		{"type": "response.output_item.added", "output_index": 0, "item": item},
		{"type": "response.function_call_arguments.delta", "item_id": "fc_fixture", "output_index": 0, "delta": string(args)},
		{"type": "response.function_call_arguments.done", "item_id": "fc_fixture", "output_index": 0, "arguments": string(args)},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": map[string]any{"id": "resp_fixture", "status": "completed", "output": []any{item}}},
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
