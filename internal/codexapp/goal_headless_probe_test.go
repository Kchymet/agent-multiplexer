package codexapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/launchenv"
)

// PROBE 2: peer turn/start + goal/set during the turn; interrupt; complete via
// update_goal; new objective after complete; prompt while paused.
func TestProbeGoalHeadless2(t *testing.T) {
	bin := requireSmokeCodex(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	var mu sync.Mutex
	var bodies []string
	var mode atomic.Value // "complete" | "hold" | "tool-complete"
	mode.Store("complete")
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		// summarize the user-side inputs of the request
		var req struct {
			Input []struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Name    string `json:"name"`
				Output  string `json:"output"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		json.Unmarshal(body, &req)
		var summ []string
		for _, it := range req.Input {
			if it.Type == "message" && it.Role == "user" {
				txt := ""
				for _, c := range it.Content {
					txt += c.Text
				}
				txt = strings.Join(strings.Fields(txt), " ")
				if len(txt) > 90 {
					txt = txt[:90] + "…"
				}
				summ = append(summ, "user:"+txt)
			} else if it.Type == "function_call_output" {
				summ = append(summ, "tool_output:"+truncate(it.Output, 120))
			} else if it.Type != "message" {
				summ = append(summ, it.Type+":"+it.Name)
			}
		}
		t.Logf("model request #%d mode=%s inputs=%v", n, mode.Load(), summ)
		w.Header().Set("Content-Type", "text/event-stream")
		id := fmt.Sprintf("resp_%d", n)
		var item map[string]any
		switch mode.Load() {
		case "hold":
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			item = map[string]any{"type": "message", "id": fmt.Sprintf("msg_%d", n), "role": "assistant", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": fmt.Sprintf("Held progress %d.", n)}}}
		case "tool-complete":
			mode.Store("complete")
			item = map[string]any{"type": "function_call", "id": fmt.Sprintf("fc_%d", n), "call_id": fmt.Sprintf("call_%d", n), "name": "update_goal", "arguments": `{"status":"complete"}`, "status": "completed"}
		default:
			item = map[string]any{"type": "message", "id": fmt.Sprintf("msg_%d", n), "role": "assistant", "status": "completed", "content": []map[string]any{{"type": "output_text", "text": fmt.Sprintf("Synthetic progress %d.", n)}}}
		}
		for _, ev := range []map[string]any{
			{"type": "response.created", "response": map[string]any{"id": id}},
			{"type": "response.output_item.done", "item": item},
			{"type": "response.completed", "response": map[string]any{"id": id, "usage": map[string]any{"input_tokens": 700, "output_tokens": 300, "total_tokens": 1000, "input_tokens_details": map[string]any{"cached_tokens": 0}, "output_tokens_details": map[string]any{"reasoning_tokens": 0}}}},
		} {
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev["type"], b)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer server.Close()
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
	cfg := Config{SessionID: "probe2", Bin: bin, Dir: dir, Env: []string{"CODEX_HOME=" + home}, ModelAccess: cap, Endpoint: "unix://" + filepath.Join(dir, "a.sock"), EventLogPath: filepath.Join(dir, "events.log")}
	s := New(cfg)
	if err := s.Start(ctx, nil); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	col := newSmokeCollector(s.Subscribe(ctx, 0))
	defer col.stop()
	goal := func(label string) map[string]any {
		raw, _ := s.rpc.call(ctx, "thread/goal/get", map[string]any{"threadId": s.ThreadID()})
		var g struct {
			Goal map[string]any `json:"goal"`
		}
		json.Unmarshal(raw, &g)
		starts, ends, _ := col.counts()
		mu.Lock()
		n := len(bodies)
		mu.Unlock()
		t.Logf("[%s] requests=%d starts=%d ends=%d goal=%v", label, n, starts, ends, g.Goal)
		return g.Goal
	}
	requests := func() int { mu.Lock(); defer mu.Unlock(); return len(bodies) }
	waitFor := func(label string, cond func() bool, d time.Duration) bool {
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if cond() {
				return true
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Logf("waitFor %s: timeout", label)
		return false
	}

	// A. peer-style turn/start (as the native TUI would), then goal/set during it.
	mode.Store("hold")
	if _, err := s.rpc.call(ctx, "turn/start", map[string]any{"threadId": s.ThreadID(), "input": inputBlocks("User typed task one")}); err != nil {
		t.Fatal(err)
	}
	waitFor("first request", func() bool { return requests() >= 1 }, 10*time.Second)
	goal("A: turn running, no goal")
	if _, err := s.rpc.call(ctx, "thread/goal/set", map[string]any{"threadId": s.ThreadID(), "objective": "User typed task one", "status": "active"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	goal("A: after goal/set during turn (still held)")
	nBefore := requests()
	mode.Store("complete")
	close(release)
	waitFor("continuation after held turn", func() bool { return requests() >= nBefore+2 }, 10*time.Second)
	goal("A: after release")

	// B. interrupt a goal turn: does the goal pause? does continuation resume?
	release = make(chan struct{})
	mode.Store("hold")
	waitFor("held goal turn", func() bool { starts, ends, _ := col.counts(); return starts > ends }, 10*time.Second)
	nBefore = requests()
	if err := s.Cancel(ctx); err != nil {
		t.Logf("cancel: %v", err)
	}
	time.Sleep(2500 * time.Millisecond)
	goal("B: after interrupt")
	t.Logf("B: requests after interrupt: %d (before %d)", requests(), nBefore)
	mode.Store("complete")
	close(release)
	time.Sleep(2500 * time.Millisecond)
	goal("B: after release post-interrupt")

	// C. resume (if paused) then model marks complete via update_goal.
	goal("C: before")
	mode.Store("tool-complete")
	if _, err := s.rpc.call(ctx, "turn/start", map[string]any{"threadId": s.ThreadID(), "input": inputBlocks("User: please finish task one")}); err != nil {
		t.Fatal(err)
	}
	waitFor("complete", func() bool { g := goal("C: poll"); return g != nil && g["status"] == "complete" }, 8*time.Second)
	nBefore = requests()
	time.Sleep(3 * time.Second)
	goal("C: after complete, idle?")
	t.Logf("C: requests after complete: %d (before %d)", requests(), nBefore)

	// D. new task after completion via turn/start (TUI bypass) + goal/set new objective.
	mode.Store("complete")
	if _, err := s.rpc.call(ctx, "turn/start", map[string]any{"threadId": s.ThreadID(), "input": inputBlocks("User typed task two")}); err != nil {
		t.Fatal(err)
	}
	raw, err := s.rpc.call(ctx, "thread/goal/set", map[string]any{"threadId": s.ThreadID(), "objective": "User typed task two", "status": "active"})
	t.Logf("D: goal/set new objective on complete goal -> %s err=%v", raw, err)
	time.Sleep(3 * time.Second)
	goal("D: after new task")

	// E. pause, then a plain prompt while paused: goal must stay paused.
	if _, err := s.rpc.call(ctx, "thread/goal/set", map[string]any{"threadId": s.ThreadID(), "status": "paused"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	goal("E: paused")
	nBefore = requests()
	if err := s.Prompt(ctx, "Question while paused"); err != nil {
		t.Logf("prompt while paused: %v", err)
	}
	time.Sleep(2500 * time.Millisecond)
	goal("E: after prompt while paused")
	t.Logf("E: requests: %d (before %d)", requests(), nBefore)
	log, _ := os.ReadFile(cfg.EventLogPath)
	var kinds []string
	for _, line := range strings.Split(string(log), "\n") {
		var ev struct {
			Type    string `json:"type"`
			Payload struct {
				NativeType string          `json:"native_type"`
				Body       json.RawMessage `json:"body"`
			} `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		k := ev.Type
		if ev.Payload.NativeType != "" {
			k = ev.Payload.NativeType
		}
		if k == "turn/started" || k == "item/started" || k == "item/completed" {
			k += ":" + truncate(string(ev.Payload.Body), 200)
		}
		kinds = append(kinds, k)
	}
	t.Logf("event kinds:\n%s", strings.Join(kinds, "\n"))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return strings.TrimSpace(s)
}
