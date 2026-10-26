package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/centralauth"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/engine/local"
	"amux/internal/panespec"
	"amux/internal/store"
)

// TestMeasureFirstDelivery is an opt-in measurement (AMUX_MEASURE_FIRST=1,
// optional AMUX_MEASURE_HOLD=12s): a fresh central Claude session started by a
// real action on the serving runtime, optionally while an unrelated admitted
// effect holds the dispatch/effect gate.
func TestMeasureFirstDelivery(t *testing.T) {
	if os.Getenv("AMUX_MEASURE_FIRST") == "" {
		t.Skip()
	}
	d, runtime, _ := sessionRuntimeFixture(t, store.Session{ID: "other", RootID: "root", Agent: "claude"})
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("AMUX_SHUTDOWN_GRACE", "0s")
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	t.Cleanup(cancel)
	fake := filepath.Join(os.Getenv("HOME"), "bin", "claude")
	os.MkdirAll(filepath.Dir(fake), 0700)
	os.WriteFile(fake, []byte("#!/bin/sh\n[ \"$CLAUDE_CODE_OAUTH_TOKEN\" = "+mockCentralToken+" ] || exit 17\nprintf ready > \"$PWD/launched\"\nsleep 30\n"), 0700)
	t.Setenv("AMUX_CLAUDE_BIN", fake)
	tok, _ := centralauth.NewSecret(mockCentralToken)
	centralauth.ClaudeStore().Save(tok, time.Now())
	d.engine = local.New()
	d.sessionRPC = runtime
	t.Cleanup(d.engine.Shutdown)
	if err := runtime.start(ctx); err != nil {
		t.Fatal(err)
	}
	serving, stop := context.WithCancel(ctx)
	joined := make(chan struct{})
	go func() { defer close(joined); runtime.run(serving) }()
	t.Cleanup(func() { stop(); <-joined; runtime.close() })
	time.Sleep(200 * time.Millisecond)

	hold := 0 * time.Second
	if v := os.Getenv("AMUX_MEASURE_HOLD"); v != "" {
		hold, _ = time.ParseDuration(v)
	}
	s := store.Session{ID: "fresh", RootID: "root", Agent: "claude", Dir: filepath.Join(core.SessionsDir(), "root", "fresh")}
	os.MkdirAll(s.Dir, 0700)
	db, _ := store.Open()
	db.PutSession(s)
	db.Close()
	begin := time.Now()
	r := d.handle(ctx, core.Action{Action: core.ActionStart, ID: s.ID})
	if !r.OK {
		t.Fatal(r.Error)
	}
	t.Logf("start action returned after %v", time.Since(begin))
	var wg sync.WaitGroup
	if hold > 0 {
		// An unrelated admitted effect (stand-in for another creation's git/config work).
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.dispatchMu.Lock()
			d.effectMu.Lock()
			time.Sleep(hold)
			d.effectMu.Unlock()
			runtime.dispatchMu.Unlock()
		}()
	}
	instance, _ := d.engine.Lookup(engine.Key{AgentID: s.ID, Tab: panespec.TabAgent})
	var mu sync.Mutex
	out := ""
	unsub := instance.Subscribe(engine.Sink{Output: func(b []byte) { mu.Lock(); out += string(b); mu.Unlock() }})
	defer unsub()
	for {
		if b, err := os.ReadFile(filepath.Join(s.Dir, "launched")); err == nil && string(b) == "ready" {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("no delivery")
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	t.Logf("delivered after %v; launcher output: %q", time.Since(begin), strings.TrimSpace(out))
	mu.Unlock()
	wg.Wait()
}
