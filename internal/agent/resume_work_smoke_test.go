package agent

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

	"amux/internal/claudecfg"
	"amux/internal/hostprep"
	"amux/internal/store"
)

// Verify Claude consumes the positional continuation immediately, in the same
// saved conversation, using only an isolated local Anthropic protocol fixture.
func TestSmokeClaudeResumeWork(t *testing.T) {
	if os.Getenv("AMUX_CLAUDE_RESUME_SMOKE") != "1" {
		t.Skip("set AMUX_CLAUDE_RESUME_SMOKE=1 with Claude installed")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if strings.Contains(r.URL.Path, "count_tokens") {
			fmt.Fprint(w, `{"input_tokens":10}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			fmt.Fprint(w, `{}`)
			return
		}
		mu.Lock()
		requests = append(requests, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []map[string]any{
			{"type": "message_start", "message": map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6", "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 10, "output_tokens": 0}}},
			{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
			{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Synthetic task progress."}},
			{"type": "content_block_stop", "index": 0},
			{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 5}},
			{"type": "message_stop"},
		} {
			b, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], b)
		}
	}))
	defer server.Close()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := store.Session{ID: "resume-fixture", Agent: "claude", Dir: dir, ClaudeID: store.NewUUID()}
	home := claudecfg.AgentHome(dir)
	os.MkdirAll(home, 0700)
	t.Setenv("HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	root, err := hostprep.OpenSession(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CLAUDE_CONFIG_DIR=" + home, "ANTHROPIC_API_KEY=synthetic", "ANTHROPIC_BASE_URL=" + server.URL, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
	for _, resume := range []bool{false, true} {
		plan, err := (claudeHarness{}).PlanLaunch(LaunchRequest{Root: root, Session: s, Dir: dir, Prompt: "Synthetic task to resume", ResumeCwds: []string{dir}, ResumeWork: resume})
		if err != nil {
			t.Fatal(err)
		}
		if resume && (len(plan.Extra) != 3 || plan.Extra[0] != "--resume" || plan.Extra[1] != s.ClaudeID || plan.Extra[2] != ResumeWorkPrompt) {
			t.Fatal("did not resume original Claude conversation with continuation")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		cmd := exec.CommandContext(ctx, bin, append([]string{"--print", "--model", "claude-sonnet-4-6", "--permission-mode", "default"}, plan.Extra...)...)
		cmd.Dir, cmd.Env = plan.Dir, env
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil || !strings.Contains(string(out), "Synthetic task progress.") {
			t.Fatalf("isolated Claude launch: %v: %s", err, out)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, body := range requests {
		if strings.Contains(body, ResumeWorkPrompt) && strings.Contains(body, "Synthetic task to resume") && strings.Contains(body, "Synthetic task progress.") {
			found = true
		}
	}
	if !found {
		t.Fatal("Claude did not automatically submit continuation with existing conversation history")
	}
	if _, err := os.Stat(filepath.Join(home, "projects")); err != nil {
		t.Fatal("Claude did not persist its conversation")
	}
}
