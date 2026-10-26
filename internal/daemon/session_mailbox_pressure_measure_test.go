package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/centralauth"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/engine/local"
	"amux/internal/panespec"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

// signedCall publishes one legitimate signed query and waits for its response.
func measureSignedCall(ctx context.Context, d *Daemon, grant access.SessionAccess) (time.Duration, error) {
	credential, err := access.LoadCredential(grant.CredentialHostDir)
	if err != nil {
		return 0, err
	}
	body, _ := json.Marshal(sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: "version"})
	envelope, err := access.SignRequest(credential, d.authority.BootID(), body, time.Now(), rand.Reader)
	if err != nil {
		return 0, err
	}
	encoded, _ := json.Marshal(envelope)
	tmp := filepath.Join(grant.RequestsHostDir, ".tmp-"+envelope.RequestID)
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return 0, err
	}
	start := time.Now()
	if err := os.Rename(tmp, filepath.Join(grant.RequestsHostDir, envelope.RequestID+".req")); err != nil {
		return 0, err
	}
	resp := filepath.Join(grant.MailboxHostDir, sessionrpc.ResponsesDirName, envelope.RequestID+".res")
	for {
		if _, err := os.Stat(resp); err == nil {
			return time.Since(start), nil
		}
		select {
		case <-ctx.Done():
			return time.Since(start), ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestMeasureMailboxPressure(t *testing.T) {
	if os.Getenv("AMUX_MEASURE_PRESSURE") == "" {
		t.Skip()
	}
	var bg []store.Session
	for i := 0; i < 6; i++ {
		bg = append(bg, store.Session{ID: fmt.Sprintf("bg%d", i), RootID: "root", Agent: "claude"})
	}
	d, runtime, _ := sessionRuntimeFixture(t, bg...)
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("AMUX_SHUTDOWN_GRACE", "0s")
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	t.Cleanup(cancel)
	fake := filepath.Join(os.Getenv("HOME"), "bin", "claude")
	os.MkdirAll(filepath.Dir(fake), 0700)
	os.WriteFile(fake, []byte("#!/bin/sh\n[ \"$CLAUDE_CODE_OAUTH_TOKEN\" = "+mockCentralToken+" ] || exit 17\nprintf ready > \"$PWD/launched\"\nsleep 60\n"), 0700)
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

	rate, _ := time.ParseDuration(os.Getenv("AMUX_MEASURE_PRESSURE"))
	traffic, stopTraffic := context.WithCancel(ctx)
	var mu sync.Mutex
	var lat []time.Duration
	var wg sync.WaitGroup
	for _, s := range bg {
		grant, err := d.authority.EnsureSession(ctx, s.ID, filepath.Join(core.SessionsDir(), "root", s.ID))
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for traffic.Err() == nil {
				c, cc := context.WithTimeout(traffic, 35*time.Second)
				l, err := measureSignedCall(c, d, grant)
				cc()
				if traffic.Err() != nil {
					return
				}
				mu.Lock()
				if err != nil {
					l = -l
				}
				lat = append(lat, l)
				mu.Unlock()
				time.Sleep(rate)
			}
		}()
	}
	time.Sleep(8 * time.Second)
	runtime.responseBudget.(*sessionResponseBudget).mu.Lock()
	b := runtime.responseBudget.(*sessionResponseBudget)
	t.Logf("budget after warmup: files=%d bytes=%d receipts=%d", b.files, b.bytes, b.receipts)
	b.mu.Unlock()

	s := store.Session{ID: "fresh", RootID: "root", Agent: "claude", Dir: filepath.Join(core.SessionsDir(), "root", "fresh")}
	os.MkdirAll(s.Dir, 0700)
	db, _ := store.Open()
	db.PutSession(s)
	db.Close()
	begin := time.Now()
	if r := d.handle(ctx, core.Action{Action: core.ActionStart, ID: s.ID}); !r.OK {
		t.Fatal(r.Error)
	}
	instance, _ := d.engine.Lookup(engine.Key{AgentID: s.ID, Tab: panespec.TabAgent})
	var omu sync.Mutex
	out := ""
	unsub := instance.Subscribe(engine.Sink{Output: func(b []byte) { omu.Lock(); out += string(b); omu.Unlock() }})
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
	omu.Lock()
	t.Logf("credential delivered after %v; launcher: %q", time.Since(begin), strings.TrimSpace(out))
	omu.Unlock()
	stopTraffic()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	failed := 0
	var ok []time.Duration
	for _, l := range lat {
		if l < 0 {
			failed++
		} else {
			ok = append(ok, l)
		}
	}
	sort.Slice(ok, func(i, j int) bool { return ok[i] < ok[j] })
	if len(ok) > 0 {
		t.Logf("background calls: %d ok, %d timed out; p50=%v p90=%v max=%v", len(ok), failed, ok[len(ok)/2], ok[len(ok)*9/10], ok[len(ok)-1])
	}
}
