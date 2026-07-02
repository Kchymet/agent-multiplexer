package wsops

import (
	"context"
	"sync"
	"testing"

	"amux/internal/store"
)

// TestConcurrentRenameAndLaunchNoLostUpdate is the AGE-133 regression guard: a
// rename (the TUI/CLI path, wsops.Rename) racing a launch adopting a conversation
// id (the engine path, store.SetClaudeID — what agent.persistConvID calls) must
// leave BOTH changes persisted. Before field-scoped writes, each did a full-row
// read-modify-write via PutSession, so whichever committed its stale row second
// reverted the other's column — a silent lost update. The single-column updaters
// touch disjoint columns, so neither can clobber the other. Run under -race in CI.
func TestConcurrentRenameAndLaunchNoLostUpdate(t *testing.T) {
	isolateStore(t)
	ctx := context.Background()

	rootID, err := CreateWorkspace(ctx, "ws", &AgentSpec{})
	if err != nil {
		t.Fatal(err)
	}
	agentID := firstChild(t, rootID)

	const iterations = 40
	var wg sync.WaitGroup
	wg.Add(2)

	// The rename path (TUI/CLI): only the name column.
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if err := Rename(agentID, "renamed"); err != nil {
				t.Errorf("Rename: %v", err)
				return
			}
		}
	}()

	// The launch path: adopt a conversation id (a separate store connection, as in
	// the running daemon) — only the claude_id column.
	go func() {
		defer wg.Done()
		db, err := store.Open()
		if err != nil {
			t.Errorf("store.Open: %v", err)
			return
		}
		defer db.Close()
		for i := 0; i < iterations; i++ {
			if err := db.SetClaudeID(agentID, "cid-adopted"); err != nil {
				t.Errorf("SetClaudeID: %v", err)
				return
			}
		}
	}()

	wg.Wait()

	s := getSession(t, agentID)
	if s.Name != "renamed" {
		t.Errorf("rename lost: name = %q, want renamed", s.Name)
	}
	if s.ClaudeID != "cid-adopted" {
		t.Errorf("adopted conversation id lost: claudeID = %q, want cid-adopted", s.ClaudeID)
	}
}
