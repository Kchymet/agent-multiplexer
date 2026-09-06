package codexapp

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// A native client answers while the turn is still active. Production amux must
// clear exactly that request immediately on the server's authoritative notice.
func TestRootServerClearsCrossClientApprovalBeforeTurnEnd(t *testing.T) {
	sup, fs, client := newFakePair(t)
	defer fs.close()
	defer sup.Close()
	attach(t, sup, client)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	events := sup.Subscribe(ctx, 0)
	fs.write(map[string]any{"id": 42, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "thr_1", "turnId": "active", "itemId": "command", "command": "echo hello"}})
	for {
		select {
		case batch := <-events:
			for _, event := range batch.Events {
				if event.Type == harnessproto.TypePermissionRequest {
					goto registered
				}
			}
		case <-ctx.Done():
			t.Fatal("approval request not observed")
		}
	}
registered:
	fs.pushNotify("serverRequest/resolved", map[string]any{"threadId": "thr_1", "requestId": 42})
	for {
		select {
		case batch := <-events:
			for _, event := range batch.Events {
				if event.Type != harnessproto.TypePermissionResolved {
					continue
				}
				var payload struct {
					RequestID string `json:"request_id"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				if payload.RequestID != "42" {
					t.Fatalf("wrong approval cleared: %s", event.Payload)
				}
				if err := sup.Resolve(ctx, "42", harnessproto.DecisionAllow); err == nil {
					t.Fatal("accepted stale answer after native resolution")
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("server resolved native approval, but amux left it pending until turn end")
		}
	}
}
