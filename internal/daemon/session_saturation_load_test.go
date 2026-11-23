package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/sessionrpc"
)

// TestSessionRPCSaturationLoad replays the 2026-11-22 incident shape against
// real session mailboxes: ~30 agents whose hooks (PostToolUse on every tool
// call) and status lines fire reports that are never collected (the hook was
// killed or timed out), plus a burst of add-agent actions, while every agent
// keeps needing credential reads and a coordinator keeps steering. With the
// fixed budget (per-session ceilings, priority tiers, short response TTL)
// credential reads and steers stay fast and the budget drains after the burst.
// The pre-fix configuration (one flat 128-response budget shared by all, no
// priority, responses held for MaxResponseAge) is run as a negative control
// and must starve them, so the test proves its own power.
func TestSessionRPCSaturationLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	fixed := runSaturationLoad(t, saturationConfig{
		budget: newSessionResponseBudget(), priority: sessionCallPriority, ttl: 3 * time.Second,
	})
	t.Logf("fixed:   %s", fixed)
	if fixed.controlMissed != 0 {
		t.Errorf("fixed budget starved %d of %d control calls", fixed.controlMissed, fixed.controlSent)
	}
	if bound := 1500 * time.Millisecond * raceSlowdown; fixed.p99 > bound {
		t.Errorf("fixed budget control p99 = %v, want < %v", fixed.p99, bound)
	}
	if fixed.filesAfter > fixed.filesPeak/4 {
		t.Errorf("budget did not recover after the burst: %d of peak %d responses still held", fixed.filesAfter, fixed.filesPeak)
	}

	legacy := runSaturationLoad(t, saturationConfig{
		budget: newSessionResponseBudgetWithLimits(sessionrpc.MaxResponseFiles, sessionrpc.MaxResponseBytes, sessionrpc.MaxPendingReceipts),
		ttl:    time.Duration(access.MaxResponseAge) * time.Second,
	})
	t.Logf("pre-fix: %s", legacy)
	if legacy.rejected == 0 || legacy.controlMissed*4 < legacy.controlSent {
		t.Errorf("negative control: pre-fix budget starved only %d of %d control calls; the load does not reproduce the incident",
			legacy.controlMissed, legacy.controlSent)
	}
}

type saturationConfig struct {
	budget   *sessionResponseBudget
	priority func(sessionrpc.Call) sessionrpc.Priority
	ttl      time.Duration
}

type saturationResult struct {
	telemetrySent, controlSent, controlMissed int
	p50, p99, max                             time.Duration
	filesPeak, filesAfter                     int
	rejected                                  uint64
}

func (r saturationResult) String() string {
	return fmt.Sprintf("telemetry=%d control=%d missed=%d p50=%v p99=%v max=%v budget peak=%d after=%d refused=%d",
		r.telemetrySent, r.controlSent, r.controlMissed, r.p50.Round(time.Millisecond), r.p99.Round(time.Millisecond),
		r.max.Round(time.Millisecond), r.filesPeak, r.filesAfter, r.rejected)
}

const (
	loadAgents        = 30
	loadDuration      = 6 * time.Second
	loadToolInterval  = time.Second // PostToolUse: a tool call a second per agent, ~10x the incident log
	loadStatusLine    = 5 * time.Second
	loadControlPeriod = 250 * time.Millisecond
	loadControlWait   = 3 * time.Second
	loadAddAgentBurst = 20
)

type loadSession struct {
	grant  access.SessionAccess
	cred   access.Credential
	server *sessionrpc.Server
}

func runSaturationLoad(t *testing.T, cfg saturationConfig) saturationResult {
	t.Helper()
	authority, err := access.Open(filepath.Join(t.TempDir(), "authority"))
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	sessions := make([]*loadSession, loadAgents)
	for i := range sessions {
		id := fmt.Sprintf("agent%02d", i)
		dir := filepath.Join(t.TempDir(), id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		grant, err := authority.EnsureSession(context.Background(), id, dir)
		if err != nil {
			t.Fatal(err)
		}
		cred, err := access.LoadCredential(grant.CredentialHostDir)
		if err != nil {
			t.Fatal(err)
		}
		server, err := sessionrpc.OpenServerMailbox(id, grant.MailboxHostDir, authority, authority, sessionrpc.Callbacks{
			Authorize: func(context.Context, access.Principal, sessionrpc.Call) error { return nil },
			Dispatch: func(context.Context, sessionrpc.DispatchRequest) (sessionrpc.DispatchResult, error) {
				return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: []byte(`{"ok":true}`)}, nil
			},
		}, sessionrpc.ServerOptions{ResponseBudget: cfg.budget, Priority: cfg.priority, ResponseTTL: cfg.ttl})
		if err != nil {
			t.Fatal(err)
		}
		if err := server.PublishService(context.Background()); err != nil {
			t.Fatal(err)
		}
		sessions[i] = &loadSession{grant: grant, cred: cred, server: server}
	}
	defer func() {
		for _, s := range sessions {
			_ = s.server.Close()
		}
	}()

	staging := t.TempDir()
	publish := func(s *loadSession, call sessionrpc.Call) (string, error) {
		body, err := json.Marshal(call)
		if err != nil {
			return "", err
		}
		envelope, err := access.SignRequest(s.cred, authority.BootID(), body, time.Now(), rand.Reader)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(envelope)
		if err != nil {
			return "", err
		}
		// Publish atomically, as the real client does: a pass must never claim
		// a half-written request (it would discard it as malformed).
		final := filepath.Join(s.grant.RequestsHostDir, envelope.RequestID+".req")
		tmp := filepath.Join(staging, envelope.RequestID)
		if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
			return "", err
		}
		return envelope.RequestID, os.Rename(tmp, final)
	}
	answered := func(s *loadSession, requestID string) bool {
		_, err := os.Stat(filepath.Join(s.grant.MailboxHostDir, sessionrpc.ResponsesDirName, requestID+".res"))
		return err == nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	var serving sync.WaitGroup
	for _, s := range sessions { // the daemon's per-mailbox passes
		serving.Add(1)
		go func(s *loadSession) {
			defer serving.Done()
			for ctx.Err() == nil {
				_, _ = s.server.ServeOnce(ctx)
				time.Sleep(5 * time.Millisecond)
			}
		}(s)
	}

	var telemetry atomic.Int64
	var peak atomic.Int64
	var mu sync.Mutex
	var latencies []time.Duration
	missed := 0
	var load sync.WaitGroup
	end := time.Now().Add(loadDuration)
	for i, s := range sessions {
		load.Add(1)
		go func(i int, s *loadSession) { // hooks and status line: never collected
			defer load.Done()
			tool, status := time.NewTicker(loadToolInterval), time.NewTicker(loadStatusLine)
			defer tool.Stop()
			defer status.Stop()
			for time.Now().Before(end) {
				verb := "report-activity"
				select {
				case <-tool.C:
				case <-status.C:
					verb = "report-model"
				}
				if _, err := publish(s, sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteAction, Verb: verb,
					Fields: map[string]string{"state": "running"}}); err == nil {
					telemetry.Add(1)
				}
			}
		}(i, s)
		load.Add(1)
		go func(i int, s *loadSession) { // credential reads and steers someone waits on
			defer load.Done()
			for n := 0; time.Now().Before(end); n++ {
				call := sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: "credential-read"}
				if n%3 == 2 {
					call = sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteAction, Verb: "steer"}
				}
				start := time.Now()
				id, err := publish(s, call)
				if err != nil {
					continue
				}
				ok := false
				for time.Since(start) < loadControlWait {
					if answered(s, id) {
						ok = true
						break
					}
					time.Sleep(2 * time.Millisecond)
				}
				mu.Lock()
				if ok {
					latencies = append(latencies, time.Since(start))
				} else {
					missed++
					where := "gone"
					for _, d := range []string{sessionrpc.RequestsDirName, "processing", sessionrpc.ResponsesDirName} {
						for _, ext := range []string{".req", ".res"} {
							if _, err := os.Stat(filepath.Join(s.grant.MailboxHostDir, d, id+ext)); err == nil {
								where = d
							}
						}
					}
					t.Logf("missed %s %s: %s", s.grant.SubjectID, call.Verb, where)
				}
				mu.Unlock()
				time.Sleep(loadControlPeriod)
			}
		}(i, s)
	}
	// The add-agent burst lands on the coordinator half a second in.
	time.Sleep(500 * time.Millisecond)
	for i := 0; i < loadAddAgentBurst; i++ {
		_, _ = publish(sessions[0], sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteAction, Verb: "add-agent"})
	}
	sampling := make(chan struct{})
	go func() {
		for {
			select {
			case <-sampling:
				return
			case <-time.After(20 * time.Millisecond):
				if n := int64(cfg.budget.snapshot().Files); n > peak.Load() {
					peak.Store(n)
				}
			}
		}
	}()
	load.Wait()
	// Recovery: the burst is over; abandoned responses must drain.
	recovery := cfg.ttl + 1500*time.Millisecond
	if cfg.ttl > 10*time.Second {
		recovery = 2 * time.Second // legacy: do not wait out MaxResponseAge
	}
	time.Sleep(recovery)
	close(sampling)
	after := cfg.budget.snapshot()
	cancel()
	serving.Wait()

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	r := saturationResult{
		telemetrySent: int(telemetry.Load()), controlSent: len(latencies) + missed, controlMissed: missed,
		filesPeak: int(peak.Load()), filesAfter: after.Files, rejected: after.Rejected,
	}
	if n := len(latencies); n > 0 {
		r.p50, r.p99, r.max = latencies[n/2], latencies[n*99/100], latencies[n-1]
	}
	return r
}
