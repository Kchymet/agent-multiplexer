package codexapp

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// TestRootAmuxApprovalReplayCannotReopenPrompt is ROOT's approval-replay reproducer
// (bin/root-coordination/amux-approval-replay-review.go.txt): a re-sent/replayed
// approval whose id the tracker already holds (pending) or has resolved (confirmed)
// must NOT raise a second permission_request — that would reopen an unanswerable
// prompt or run the decision flow twice.
func TestRootAmuxApprovalReplayCannotReopenPrompt(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		name := "pending"
		if confirmed {
			name = "confirmed"
		}
		t.Run(name, func(t *testing.T) {
			s := New(Config{SessionID: "root-review"})
			s.threadID = "our-thread"
			defer s.Close()
			id := json.RawMessage(`42`)
			p := json.RawMessage(`{"threadId":"our-thread","turnId":"our-turn","itemId":"command","command":"echo hi"}`)
			s.handleApproval(id, "item/commandExecution/requestApproval", p)
			if confirmed {
				s.handleServerResolved(json.RawMessage(`{"requestId":42,"threadId":"our-thread"}`))
			}
			s.hub.mu.Lock()
			before := s.hub.seq
			s.hub.mu.Unlock()

			s.handleApproval(id, "item/commandExecution/requestApproval", p)

			s.hub.mu.Lock()
			defer s.hub.mu.Unlock()
			for _, e := range s.hub.ring {
				if e.seq > before && e.ev.Type == harnessproto.TypePermissionRequest {
					t.Errorf("replayed %s request emitted a fresh permission prompt after the tracker rejected registration", name)
				}
			}
		})
	}
}

// Concurrent duplicate deliveries of the same approval id must yield exactly one
// permission_request (register is atomic → one winner emits).
func TestConcurrentDuplicateApprovalEmitsOnce(t *testing.T) {
	s := New(Config{SessionID: "root-review"})
	s.threadID = "our-thread"
	defer s.Close()
	id := json.RawMessage(`7`)
	p := json.RawMessage(`{"threadId":"our-thread","turnId":"t","itemId":"c","command":"x"}`)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleApproval(id, "item/commandExecution/requestApproval", p)
		}()
	}
	wg.Wait()

	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	n := 0
	for _, e := range s.hub.ring {
		if e.ev.Type == harnessproto.TypePermissionRequest {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("concurrent duplicate approvals emitted %d permission_request events, want 1", n)
	}
}
