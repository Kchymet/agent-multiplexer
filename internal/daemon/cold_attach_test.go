package daemon

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A real native TUI resumes with excludeTurns=true. A plain second-client
// initialize (or a non-paginated resume) misses the empty-rollout bootstrap bug.
// Both sandbox-launch tests call this before any client has submitted a turn.
func assertFreshNativeResume(t *testing.T, endpoint, threadID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", strings.TrimPrefix(endpoint, "unix://"))
	}}
	conn, _, err := dialer.DialContext(ctx, "ws://localhost", http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	conn.SetReadDeadline(deadline)
	conn.SetWriteDeadline(deadline)
	call := func(id int, method string, params any) json.RawMessage {
		t.Helper()
		if err := conn.WriteJSON(map[string]any{"id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		for {
			var reply struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := conn.ReadJSON(&reply); err != nil {
				t.Fatal(err)
			}
			if reply.ID != id {
				continue
			}
			if len(reply.Error) != 0 && string(reply.Error) != "null" {
				t.Fatalf("native bootstrap %s: %s", method, reply.Error)
			}
			return reply.Result
		}
	}
	call(1, "initialize", map[string]any{"clientInfo": map[string]any{"name": "native-bootstrap-test", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}})
	if err := conn.WriteJSON(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	resumed := call(2, "thread/resume", map[string]any{"threadId": threadID, "excludeTurns": true})
	history := call(3, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true})
	for _, raw := range []json.RawMessage{resumed, history} {
		var result struct {
			Thread struct {
				ID    string            `json:"id"`
				Turns []json.RawMessage `json:"turns"`
			} `json:"thread"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if result.Thread.ID != threadID || len(result.Thread.Turns) != 0 {
			t.Fatalf("cold attach must preserve the empty canonical thread: %s", raw)
		}
	}
	t.Log("second client resumed the empty sandboxed thread with native pagination; no warmup turn")
}
