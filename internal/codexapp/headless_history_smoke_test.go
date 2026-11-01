package codexapp

import (
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

	"github.com/creack/pty"

	"amux/internal/launchenv"
)

// headlessReply is the assistant text the fixture model answers with. It is
// what a native attach must be able to show for a turn that ran before it.
const headlessReply = "HEADLESS-REPLY-MARKER"

// TestSmokeNativeAttachShowsHeadlessHistory is the shape of a workgroup created
// from the web dashboard: amux's supervisor starts the thread and runs the
// creation prompt's turn with NO native TUI attached, and the user opens the
// session in amux only afterwards. The native `codex --remote … resume <thread>`
// must then show that earlier turn, exactly as it would for a turn it watched.
//
// Real Codex 0.157.1 and real native TUI; the model is a local SSE fixture.
func TestSmokeNativeAttachShowsHeadlessHistory(t *testing.T) {
	bin := os.Getenv("AMUX_TEST_CODEX_BIN")
	if bin == "" {
		t.Skip("set AMUX_TEST_CODEX_BIN to CI-pinned Codex 0.157.1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 8<<20))
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		item := map[string]any{
			"type": "message", "id": "msg_headless", "role": "assistant", "status": "completed",
			"content": []map[string]any{{"type": "output_text", "text": headlessReply}},
		}
		for _, ev := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": "resp_headless"}},
			{"type": "response.output_item.done", "item": item},
			{"type": "response.completed", "response": map[string]any{"id": "resp_headless", "usage": map[string]any{
				"input_tokens": 10, "output_tokens": 5, "total_tokens": 15,
				"input_tokens_details":  map[string]any{"cached_tokens": 0},
				"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			}}},
		} {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], b)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer model.Close()

	dir := t.TempDir()
	home := filepath.Join(dir, "codex")
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf("model_provider = 'fixture'\nmodel = 'gpt-5.6'\n"+
		"[model_providers.fixture]\nname = 'fixture'\nbase_url = %q\nwire_api = 'responses'\nenv_key = 'OPENAI_API_KEY'\n"+
		"[projects.%q]\ntrust_level = 'trusted'\n", model.URL, dir)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	endpoint, err := LoopbackEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	access, _ := launchenv.ForModelEnvironment(launchenv.CodexModelAccount, []string{"OPENAI_API_KEY=synthetic"})
	s := New(Config{
		SessionID: "headless-history", Bin: bin, Dir: dir, Env: []string{"CODEX_HOME=" + home},
		ModelAccess: access, Endpoint: endpoint, InitialPrompt: "HEADLESS-PROMPT-MARKER",
		EventLogPath: filepath.Join(dir, "events.log"),
	})
	col := newSmokeCollector(s.Subscribe(ctx, 0))
	if err := s.Start(ctx, nil); err != nil {
		t.Fatalf("start supervisor: %v", err)
	}
	defer s.Close()
	select {
	case <-col.turnEnded:
	case <-ctx.Done():
		t.Fatal("the creation prompt's turn never ended")
	}
	thread := s.ThreadID()

	// Evidence: the exact bootstrap the native TUI performs (resume with
	// excludeTurns, then page turns), from a second client while amux's
	// supervisor still holds the thread loaded.
	logNativeBootstrap(t, ctx, endpoint, thread)

	argv := AttachArgv(bin, endpoint, thread)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CODEX_HOME=" + home, "TERM=xterm-256color", "OPENAI_API_KEY=synthetic"}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = f.Close() }()
	var mu sync.Mutex
	var out strings.Builder
	go func() {
		b := make([]byte, 8192)
		for {
			n, e := f.Read(b)
			if n > 0 {
				mu.Lock()
				out.Write(b[:n])
				mu.Unlock()
			}
			if e != nil {
				return
			}
		}
	}()
	shown := waitUntil(20*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(out.String(), headlessReply)
	})
	if !shown {
		mu.Lock()
		tail := out.String()
		mu.Unlock()
		if len(tail) > 4000 {
			tail = tail[len(tail)-4000:]
		}
		t.Fatalf("native attach after a headless turn shows no history (reply %q never painted); output tail:\n%q", headlessReply, tail)
	}
}

func logNativeBootstrap(t *testing.T, ctx context.Context, endpoint, thread string) {
	t.Helper()
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := dialWS(dctx, endpoint, "")
	if err != nil {
		t.Logf("bootstrap probe: dial: %v", err)
		return
	}
	defer conn.Close()
	id := 0
	call := func(method string, params any) string {
		id++
		req, _ := json.Marshal(map[string]any{"method": method, "id": id, "params": params})
		if err := conn.WriteMessage(dctx, req); err != nil {
			return "write: " + err.Error()
		}
		for {
			msg, err := conn.ReadMessage()
			if err != nil {
				return "read: " + err.Error()
			}
			var resp struct {
				ID *int `json:"id"`
			}
			_ = json.Unmarshal(msg, &resp)
			if resp.ID != nil && *resp.ID == id {
				s := string(msg)
				if len(s) > 1500 {
					s = s[:1500] + "…"
				}
				return s
			}
		}
	}
	t.Logf("initialize: %s", call("initialize", map[string]any{
		"clientInfo":   map[string]any{"name": "codex-tui", "title": "probe", "version": "0.157.1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}))
	t.Logf("thread/resume excludeTurns: %s", call("thread/resume", map[string]any{"threadId": thread, "excludeTurns": true}))
	t.Logf("thread/turns/list: %s", call("thread/turns/list", map[string]any{"threadId": thread, "limit": 10}))
	t.Logf("thread/read includeTurns: %s", call("thread/read", map[string]any{"threadId": thread, "includeTurns": true}))
}
