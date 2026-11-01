package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/store"
)

// idlePopulation is the live install that motivated this measurement: 214 live
// and 293 archived sessions, each carrying a task-sized prompt.
func idlePopulation(b *testing.B, live, archived int) *sessionRuntime {
	b.Helper()
	isolateHome(b)
	db, err := store.Open()
	if err != nil {
		b.Fatal(err)
	}
	prompt := strings.Repeat("task prompt ", 300)
	for i := 0; i < live+archived; i++ {
		id := fmt.Sprintf("a%04d", i)
		dir := filepath.Join(core.SessionsDir(), "root", id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			b.Fatal(err)
		}
		if err := db.PutSession(store.Session{ID: id, RootID: "root", Agent: "claude", Dir: dir, Prompt: prompt, Archived: i >= live}); err != nil {
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
	if err := r.start(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(r.close)
	return r
}

func processCPU() time.Duration {
	var u syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &u)
	return time.Duration(u.Utime.Nano() + u.Stime.Nano())
}

// BenchmarkSessionRuntimeIdle runs the real serving loop over an idle install
// and reports the CPU it burns per wall second (cpu-%) and the mean reconcile
// tick, which runs on the goroutine that starts every mailbox pass.
func BenchmarkSessionRuntimeIdle(b *testing.B) {
	store.ShareHandles(true)
	defer store.ShareHandles(false)
	r := idlePopulation(b, 214, 293)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	b.ResetTimer()
	cpu0, wall0 := processCPU(), time.Now()
	go func() { defer close(done); r.run(ctx) }()
	time.Sleep(time.Duration(b.N) * time.Second)
	cpu, wall := processCPU()-cpu0, time.Since(wall0)
	cancel()
	<-done
	b.StopTimer()
	b.ReportMetric(100*cpu.Seconds()/wall.Seconds(), "cpu-%")
	tick := time.Now()
	const ticks = 20
	for i := 0; i < ticks; i++ {
		if err := r.reconcileTick(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(time.Since(tick).Microseconds())/ticks, "us/reconcile")
}
