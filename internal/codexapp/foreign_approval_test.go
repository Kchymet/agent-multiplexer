package codexapp

import (
	"encoding/json"
	"testing"
)

// TestRootAmuxForeignApprovalNotAnswerable is ROOT's foreign-approval reproducer
// (bin/root-coordination/foreign-approval-review.go.txt): an approval naming a
// foreign thread — or no thread — must not become a local OpenApprovals entry that a
// `permission` verb could answer through this supervisor.
func TestRootAmuxForeignApprovalNotAnswerable(t *testing.T) {
	for _, thread := range []string{"foreign-thread", ""} {
		t.Run(thread, func(t *testing.T) {
			s := New(Config{SessionID: "root-review"})
			s.threadID = "our-thread"
			defer s.Close()
			params, _ := json.Marshal(map[string]any{
				"threadId": thread, "turnId": "foreign-turn",
				"itemId": "foreign-item", "command": "touch foreign-file",
			})
			s.handleApproval(json.RawMessage(`42`), "item/commandExecution/requestApproval", params)
			if ids := s.OpenApprovals(); len(ids) != 0 {
				t.Fatalf("foreign/missing-thread approval is answerable through local supervisor: %v", ids)
			}
		})
	}
}

// A same-thread approval is still registered and answerable — including one that
// arrives while no turn is tracked (passive/early): the ownership guard is on thread,
// not turn.
func TestSameThreadApprovalRegistersEvenEarly(t *testing.T) {
	s := New(Config{SessionID: "root-review"})
	s.threadID = "our-thread"
	defer s.Close()
	// curTurn deliberately unset: the request predates any tracked turn.
	params, _ := json.Marshal(map[string]any{
		"threadId": "our-thread", "turnId": "t1",
		"itemId": "item-1", "command": "ls",
	})
	s.handleApproval(json.RawMessage(`"ap1"`), "item/commandExecution/requestApproval", params)
	ids := s.OpenApprovals()
	if len(ids) != 1 || ids[0] != "ap1" {
		t.Fatalf("same-thread approval not registered/answerable: %v", ids)
	}
}
