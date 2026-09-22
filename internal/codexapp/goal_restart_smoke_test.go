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
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/launchenv"
	"github.com/creack/pty"
)

// Actual Codex, actual native TUI, isolated account and a local fake model. No
// production API credentials or model requests are used. The fake holds turns
// open so restart must preserve running work rather than an already-finished turn.
func TestSmokeGoalRestart(t *testing.T) {
	bin := requireSmokeCodex(t)
	t.Run("disabled", func(t *testing.T) {
		dir := t.TempDir()
		home := filepath.Join(dir, "codex")
		t.Setenv("HOME", dir)
		if err := os.MkdirAll(home, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[features]\ngoals = false\n"), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s := New(Config{SessionID: "disabled", Bin: bin, Dir: dir, Env: []string{"CODEX_HOME=" + home}, Endpoint: "unix://" + filepath.Join(dir, "a.sock")})
		defer s.Close()
		if err := s.Start(ctx, nil); err != nil {
			t.Fatalf("goals disabled must not break ordinary startup: %v", err)
		}
	})
	for _, running := range []bool{true, false} {
		t.Run(fmt.Sprintf("running=%t", running), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			defer cancel()
			var requests atomic.Int32
			stop := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				requests.Add(1)
				select {
				case <-stop:
				case <-r.Context().Done():
				}
			}))
			defer server.Close()
			defer close(stop)
			dir := t.TempDir()
			home := filepath.Join(dir, "codex")
			t.Setenv("HOME", dir)
			t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
			if err := os.MkdirAll(home, 0700); err != nil {
				t.Fatal(err)
			}
			config := fmt.Sprintf("model_provider = 'fixture'\nmodel = 'gpt-5.6'\n[features]\ngoals = true\n[model_providers.fixture]\nname = 'fixture'\nbase_url = %q\nwire_api = 'responses'\nenv_key = 'OPENAI_API_KEY'\n[projects.%q]\ntrust_level = 'trusted'\n", server.URL, dir)
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			cap, _ := launchenv.ForModelEnvironment(launchenv.CodexModelAccount, []string{"OPENAI_API_KEY=synthetic"})
			cfg := Config{SessionID: "goal-restart", Bin: bin, Dir: dir, Env: []string{"CODEX_HOME=" + home}, ModelAccess: cap, Endpoint: "unix://" + filepath.Join(dir, "a.sock")}
			s := New(cfg)
			if err := s.Start(ctx, nil); err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			status := "paused"
			if running {
				status = "active"
			}
			raw, err := s.rpc.call(ctx, "thread/goal/set", map[string]any{"threadId": s.ThreadID(), "objective": "Synthetic local goal", "status": status, "tokenBudget": 10000})
			if err != nil {
				t.Fatal(err)
			}
			var before struct {
				Goal map[string]any `json:"goal"`
			}
			json.Unmarshal(raw, &before)
			// The set's RESPONSE and the goal notification are separate messages:
			// the supervisor learns the goal from the notification, on its read
			// loop. Wait for that observation before reading restart intent, or
			// this asserts on whichever arrived first.
			if !awaitObservedGoal(s, status, 5*time.Second) {
				st, ok := s.Goal()
				t.Fatalf("supervisor never observed the %s goal: %+v (ok=%t)", status, st, ok)
			}
			work := s.RestartWork()
			if (work.GoalKey != "") != running {
				t.Fatal("incorrect pre-shutdown goal intent")
			}
			if running {
				// Emulate a harness pausing its goal while shutting down, after
				// amux has captured intent. A deliberate pause is captured earlier.
				if _, err := s.rpc.call(ctx, "thread/goal/set", map[string]any{"threadId": s.ThreadID(), "status": "paused"}); err != nil {
					t.Fatal(err)
				}
			}
			cfg.ResumeThreadID = s.ThreadID()
			cfg.RestartWork = &work
			s.Close()
			cfg.Endpoint = "unix://" + filepath.Join(dir, "b.sock")
			priorRequests := requests.Load()
			s = New(cfg)
			if err := s.Start(ctx, nil); err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			raw, err = s.rpc.call(ctx, "thread/goal/get", map[string]any{"threadId": s.ThreadID()})
			if err != nil {
				t.Fatal(err)
			}
			var after struct {
				Goal map[string]any `json:"goal"`
			}
			json.Unmarshal(raw, &after)
			if after.Goal["status"] != status {
				t.Fatalf("status=%v want %s", after.Goal["status"], status)
			}
			for _, key := range []string{"threadId", "objective", "tokenBudget", "tokensUsed", "createdAt"} {
				if after.Goal[key] != before.Goal[key] {
					t.Fatalf("restart changed %s", key)
				}
			}
			argv := AttachArgv(bin, cfg.Endpoint, s.ThreadID())
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
			cmd.Dir = dir
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "CODEX_HOME=" + home, "TERM=xterm-256color", "OPENAI_API_KEY=synthetic"}
			f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { cmd.Process.Kill(); cmd.Wait(); f.Close() }()
			var mu sync.Mutex
			var output strings.Builder
			go func() {
				b := make([]byte, 8192)
				for {
					n, e := f.Read(b)
					if n > 0 {
						mu.Lock()
						output.Write(b[:n])
						mu.Unlock()
					}
					if e != nil {
						return
					}
				}
			}()
			// Bound native painting time; assert positive goal/model state as well
			// as the dialog, so an unloaded/failed TUI cannot prove prompt absence.
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				mu.Lock()
				out := output.String()
				mu.Unlock()
				if !running && strings.Contains(out, "Resume paused goal?") {
					break
				}
				if running && requests.Load() > priorRequests && strings.Contains(out, "Working") {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			mu.Lock()
			out := output.String()
			mu.Unlock()
			prompt := strings.Contains(out, "Resume paused goal?")
			if running && (prompt || requests.Load() <= priorRequests || !strings.Contains(out, "Working")) {
				t.Fatalf("active goal did not resume without confirmation: dialog=%t modelRequests=%d", prompt, requests.Load()-priorRequests)
			}
			if !running && (!prompt || requests.Load() != priorRequests) {
				t.Fatal("deliberately paused goal did not stay paused")
			}
		})
	}
}

// awaitObservedGoal waits until the supervisor has observed a goal in the given
// status. Its own writes record the RPC result directly, but a goal set through
// the raw transport (as this smoke does, to stand in for another client) is
// learned only from the broadcast notification.
func awaitObservedGoal(s *Supervisor, status string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if st, ok := s.Goal(); ok && st.Status == status {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
