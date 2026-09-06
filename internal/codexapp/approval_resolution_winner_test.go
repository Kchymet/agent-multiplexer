package codexapp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func rootApprovalFixture(t *testing.T) (*Supervisor, *fakeServer, *collector) {
	t.Helper()
	sup, fs, client := newFakePair(t)
	t.Cleanup(func() { fs.close(); sup.Close() })
	attach(t, sup, client)
	col := subscribeCollector(context.Background(), sup)
	fs.pushNotify("turn/started", map[string]any{"threadId": "thr_1", "turn": map[string]any{"id": "active"}})
	fs.write(map[string]any{"id": 42, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": "thr_1", "turnId": "active", "itemId": "command", "command": "echo hello"}})
	col.waitFor(t, harnessproto.TypePermissionRequest)
	return sup, fs, col
}

// The notification confirms resolution, but contains neither the winning client
// nor its decision. A concurrent peer may have denied before our allow arrived.
func TestRootResolutionDoesNotInventWinningDecision(t *testing.T) {
	for _, ended := range []bool{false, true} {
		name := "server-confirmation"
		if ended {
			name = "turn-abandoned-before-confirmation"
		}
		t.Run(name, func(t *testing.T) {
			sup, fs, col := rootApprovalFixture(t)
			if err := sup.Resolve(context.Background(), "42", harnessproto.DecisionAllow); err != nil {
				t.Fatal(err)
			}
			if ended {
				fs.pushNotify("turn/completed", map[string]any{"threadId": "thr_1", "turn": map[string]any{"id": "active", "status": "interrupted"}})
			} else {
				fs.pushNotify("serverRequest/resolved", map[string]any{"threadId": "thr_1", "requestId": 42})
			}
			event := col.waitFor(t, harnessproto.TypePermissionResolved)
			var p struct {
				Decision string `json:"decision"`
			}
			if err := json.Unmarshal(event.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.Decision != harnessproto.DecisionCleared {
				t.Fatalf("server never identified winning decision, but event claims %q: %s", p.Decision, event.Payload)
			}
		})
	}
}

func TestRootResolutionRequiresThreadCorrelation(t *testing.T) {
	sup, _, _ := rootApprovalFixture(t)
	sup.handleServerResolved(json.RawMessage(`{"requestId":42}`))
	if ids := sup.OpenApprovals(); len(ids) != 1 || ids[0] != "42" {
		t.Fatalf("notification missing required threadId cleared a live request: %v", ids)
	}
}
