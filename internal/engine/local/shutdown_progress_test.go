package local

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"amux/internal/engine"
)

// Shutdown reports each instance's progress to the observer: a busy agent is
// waited on (with its SIGTERM deadline) before it is terminated, an idle one
// that ignores SIGTERM escalates to SIGKILL, and every instance ends stopped.
func TestShutdownReportsInstanceProgress(t *testing.T) {
	t.Setenv("AMUX_SHUTDOWN_GRACE", "5s")
	t.Setenv("AMUX_KILL_GRACE", "300ms")
	busyKey := engine.Key{AgentID: "busy"}
	stubbornKey := engine.Key{AgentID: "stubborn"}
	var mu sync.Mutex
	busyUntil := time.Now().Add(300 * time.Millisecond)
	eng := New(WithActivity(func(k engine.Key) engine.Activity {
		mu.Lock()
		defer mu.Unlock()
		if k == busyKey && time.Now().Before(busyUntil) {
			return engine.ActivityBusy
		}
		return engine.ActivitySafe
	}))
	for key, script := range map[engine.Key]string{
		busyKey:     "trap 'exit 0' TERM; printf READY; while :; do sleep 0.02; done",
		stubbornKey: "trap '' TERM; printf READY; while :; do sleep 0.02; done",
	} {
		inst, err := eng.Ensure(context.Background(), engine.Spec{Key: key, Argv: []string{"sh", "-c", script}, Cols: 80, Rows: 24})
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		c := &collector{}
		inst.Subscribe(c.sink())
		waitFor(t, c, "READY", 3*time.Second)
	}

	var snapshots [][]ShutdownProgress
	eng.SetShutdownObserver(func(p []ShutdownProgress) {
		snapshots = append(snapshots, append([]ShutdownProgress(nil), p...))
	})
	eng.Shutdown()

	history := map[engine.Key][]string{}
	var busyDeadline time.Time
	for _, snap := range snapshots {
		if len(snap) != 2 {
			t.Fatalf("snapshot has %d instances, want 2: %+v", len(snap), snap)
		}
		for _, p := range snap {
			h := history[p.Key]
			if len(h) == 0 || h[len(h)-1] != p.State {
				history[p.Key] = append(h, p.State)
			}
			if p.Key == busyKey && p.State == ShutdownBusy {
				busyDeadline = p.Until
			}
		}
	}
	want := map[engine.Key][]string{
		busyKey:     {ShutdownTerminating, ShutdownBusy, ShutdownTerminating, ShutdownStopped},
		stubbornKey: {ShutdownTerminating, ShutdownKilling, ShutdownStopped},
	}
	for key, states := range want {
		if got := history[key]; !slices.Equal(got, states) {
			t.Errorf("%s states = %v, want %v", key.AgentID, got, states)
		}
	}
	if left := time.Until(busyDeadline); busyDeadline.IsZero() || left > 5*time.Second || left < 3*time.Second {
		t.Errorf("busy instance's SIGTERM deadline %v is not ~the shutdown grace away", busyDeadline)
	}
	last := snapshots[len(snapshots)-1]
	for _, p := range last {
		if p.State != ShutdownStopped || !p.Until.IsZero() {
			t.Errorf("final snapshot still pending: %+v", p)
		}
	}
}
