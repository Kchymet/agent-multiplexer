package codexapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// Launch-argument input is submitted before the supervisor is exposed. It must
// use the same canonical echo as later web/native input, not a fabricated row.
func TestInitialPromptJournalUsesCanonicalEcho(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	sup.cfg.InitialPrompt = "launch argument"
	sup.cfg.EventLogPath = filepath.Join(t.TempDir(), "initial.events.jsonl")
	attach(t, sup, client)
	if _, ok := fs.sawCall("turn/start"); !ok {
		t.Fatal("initial input was not submitted")
	}
	data, err := os.ReadFile(sup.cfg.EventLogPath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"type":"prompt"`) {
		t.Fatalf("prompt fabricated before canonical echo: %s", data)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	collector := subscribeCollector(ctx, sup)
	fs.pushTurnStarted()
	params := map[string]any{"threadId": "thr_1", "turnId": "turn_1",
		"item": map[string]any{"id": "initial-user", "type": "userMessage", "content": []map[string]string{{"type": "text", "text": "canonical launch input"}}}}
	fs.pushNotify("item/started", params)
	fs.pushNotify("item/completed", params)
	fs.pushNotify("turn/completed", map[string]any{"threadId": "thr_1", "turn": map[string]string{"id": "turn_1", "status": "completed"}})
	collector.waitFor(t, harnessproto.TypeTurnEnd)
	data, err = os.ReadFile(sup.cfg.EventLogPath)
	if err != nil {
		t.Fatal(err)
	}
	var prompts []harnessproto.RuntimeEvent
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var ev harnessproto.RuntimeEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type == harnessproto.TypePrompt {
			prompts = append(prompts, ev)
		}
	}
	if len(prompts) != 1 || prompts[0].ItemID != "initial-user" || !strings.Contains(string(prompts[0].Payload), "canonical launch input") {
		t.Fatalf("initial canonical prompt missing or duplicated: %s", data)
	}
}

// AGE-232: exercise the authoritative journal without a model or UI. Both
// clients' canonical messages must persist, with no synthetic Prompt call.
func TestSharedConversationJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	s := New(Config{EventLogPath: path})
	s.threadID = "shared"
	for _, turn := range []string{"native", "web"} {
		s.onNotify("turn/started", mustMarshal(map[string]any{"threadId": "shared", "turn": map[string]string{"id": turn}}))
		params := mustMarshal(map[string]any{"threadId": "shared", "turnId": turn,
			"item": map[string]any{"id": turn + "-user", "type": "userMessage",
				"content": []map[string]string{{"type": "text", "text": turn + " prompt"}}}})
		s.onNotify("item/started", params)
		s.onNotify("item/completed", params)
		s.onNotify("item/completed", params)
		s.onNotify("item/agentMessage/delta", mustMarshal(map[string]string{
			"threadId": "shared", "turnId": turn, "itemId": turn + "-reply", "delta": turn + " reply"}))
		s.onNotify("item/completed", mustMarshal(map[string]any{"threadId": "shared", "turnId": turn,
			"item": map[string]string{"id": turn + "-reply", "type": "agentMessage", "text": turn + " reply"}}))
		s.onNotify("turn/completed", mustMarshal(map[string]any{"threadId": "shared", "turn": map[string]string{"id": turn, "status": "completed"}}))
	}
	// Other threads can be broadcast by a shared server too; do not log them.
	s.onNotify("item/completed", json.RawMessage(`{"threadId":"foreign","turnId":"foreign","item":{"id":"foreign","type":"userMessage","content":[{"type":"text","text":"foreign prompt"}]}}`))
	s.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 10 {
		t.Fatalf("want 5 journal rows per turn, got %d: %s", len(lines), data)
	}
	for i, line := range lines {
		var ev harnessproto.RuntimeEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		turn := []string{"native", "web"}[i/5]
		var p struct {
			Thread string `json:"thread_id"`
			Turn   string `json:"turn_id"`
			Text   string `json:"text"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if p.Thread != "shared" || p.Turn != turn {
			t.Fatalf("journal row lost identity: %s", line)
		}
		wantType := []string{"turn_start", "prompt", "text", "text", "turn_end"}[i%5]
		if ev.Type != wantType {
			t.Fatalf("row %d type %s, want %s", i, ev.Type, wantType)
		}
		if i%5 == 1 && (p.Text != turn+" prompt" || ev.Direction != "in" || ev.ItemID != turn+"-user") {
			t.Fatalf("bad prompt: %s", line)
		}
		if i%5 == 2 && p.Text != turn+" reply" {
			t.Fatalf("missing reply: %s", line)
		}
		if i%5 == 3 && p.Text != "" {
			t.Fatalf("completion duplicated reply: %s", line)
		}
	}
}
