package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/store"
)

// reconcilePopulation stores live agents (with real mailboxes) and archived
// ones, the shape of a long-lived install, then starts the session runtime.
func reconcilePopulation(b *testing.B, live, archived int) *sessionRuntime {
	b.Helper()
	isolateHome(b)
	db, err := store.Open()
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < live+archived; i++ {
		dir := filepath.Join(core.SessionsDir(), "root", fmt.Sprintf("a%04d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			b.Fatal(err)
		}
		if err := db.PutSession(store.Session{ID: fmt.Sprintf("a%04d", i), RootID: "root", Agent: "claude", Dir: dir, Archived: i >= live}); err != nil {
			b.Fatal(err)
		}
	}
	_ = db.Close()
	d := New("", nil, time.Hour)
	d.authority, err = access.Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = d.authority.Close() })
	r := newSessionRuntime(d)
	start := time.Now()
	if err := r.start(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.Logf("session runtime start with %d live / %d archived: %v", live, archived, time.Since(start))
	b.Cleanup(r.close)
	return r
}

func BenchmarkSessionReconcile(b *testing.B) {
	r := reconcilePopulation(b, 40, 300)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.reconcile(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}
