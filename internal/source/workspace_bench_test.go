package source

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/store"
)

// BenchmarkWorkspacePoll is the daemon's 2s rail poll over an install shaped
// like the long-lived one that motivated it: 507 workgroups, 293 archived,
// each with a coordinator and two members carrying a task-sized prompt.
func BenchmarkWorkspacePoll(b *testing.B) {
	b.Setenv("XDG_DATA_HOME", filepath.Join(b.TempDir(), "data"))
	b.Setenv("HOME", b.TempDir())
	store.ShareHandles(true)
	defer store.ShareHandles(false)
	db, err := store.Open()
	if err != nil {
		b.Fatal(err)
	}
	prompt := strings.Repeat("task prompt ", 300)
	created := int64(0)
	for i := 0; i < 507; i++ {
		archived := i < 293
		root := fmt.Sprintf("wg%04d", i)
		created++
		if err := db.PutSession(store.Session{ID: root, Scope: store.ScopeWork, Agent: "claude", ClaudeID: "r-" + root,
			Dir: store.CoordinatorDir(root), Prompt: prompt, Created: created, Archived: archived}); err != nil {
			b.Fatal(err)
		}
		for j := 0; j < 2; j++ {
			id := fmt.Sprintf("%s-a%d", root, j)
			created++
			if err := db.PutSession(store.Session{ID: id, RootID: root, Agent: "claude", ClaudeID: "r-" + id,
				Dir: store.AgentDir(root, id), Prompt: prompt, Created: created, Archived: archived}); err != nil {
				b.Fatal(err)
			}
		}
	}
	_ = db.Close()
	w := NewWorkspace()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := w.Poll(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}
