package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/credentialbroker"
	"amux/internal/panespec"
	"amux/internal/sessionreport"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

// reportPopulation is a long-lived install's shape: one live reporting agent
// among 40 live and 300 archived sessions.
func reportPopulation(b *testing.B) (*sessionRuntime, access.Principal, string) {
	b.Helper()
	const runtimeID = "33333333-3333-4333-8333-333333333333"
	sessions := []store.Session{{ID: "a0000", RootID: "root", Agent: "claude", ClaudeID: runtimeID}}
	for i := 1; i < 340; i++ {
		sessions = append(sessions, store.Session{ID: fmt.Sprintf("a%04d", i), RootID: "root", Agent: "claude", ClaudeID: runtimeID, Archived: i >= 40})
	}
	d, runtime, principals := sessionRuntimeFixture(b, sessions...)
	return runtime, principals["a0000"], publishTestRuntime(b, d, "a0000")
}

// BenchmarkSessionReportDispatch measures one hook's daemon-side cost: the
// context query plus the activity report, each of which runs under the
// daemon-wide dispatch/effect gates every other session also needs.
func BenchmarkSessionReportDispatch(b *testing.B) {
	runtime, principal, generation := reportPopulation(b)
	contextQuery := sessionrpc.DispatchRequest{Principal: principal, RequestID: "0123456789abcdef0123456789abcdef",
		Call: sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: sessionreport.Context}}
	activity := reportRequest(principal, sessionreport.Activity, map[string]string{
		sessionreport.FieldRuntimeGeneration: generation, sessionreport.FieldState: core.StateRunning,
	})
	for _, shared := range []bool{false, true} {
		store.ShareHandles(shared)
		handles := map[bool]string{false: "per-call", true: "shared"}[shared]
		b.Run("context/"+handles, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if result, err := runtime.dispatch(context.Background(), contextQuery); err != nil || result.Status != sessionrpc.StatusOK {
					b.Fatalf("context = %+v, %v", result, err)
				}
			}
		})
		b.Run("activity/"+handles, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if result, err := runtime.dispatch(context.Background(), activity); err != nil || result.Status != sessionrpc.StatusOK {
					b.Fatalf("activity = %+v, %v", result, err)
				}
			}
		})
	}
	store.ShareHandles(false)
}

// BenchmarkSessionReconcileSharedHandles is BenchmarkSessionReconcile with the
// daemon's pooled store handles.
func BenchmarkSessionReconcileSharedHandles(b *testing.B) {
	store.ShareHandles(true)
	defer store.ShareHandles(false)
	r := reconcilePopulation(b, 40, 300)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.reconcile(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStoreOpen(b *testing.B) {
	reportPopulation(b)
	b.Run("shared", func(b *testing.B) {
		store.ShareHandles(true)
		defer store.ShareHandles(false)
		for i := 0; i < b.N; i++ {
			db, err := store.Open()
			if err != nil {
				b.Fatal(err)
			}
			_ = db.Close()
		}
	})
	b.Run("writable", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			db, err := store.Open()
			if err != nil {
				b.Fatal(err)
			}
			_ = db.Close()
		}
	})
	b.Run("readonly", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			db, err := store.OpenReadOnly()
			if err != nil {
				b.Fatal(err)
			}
			_ = db.Close()
		}
	})
}

// BenchmarkConcurrentReportsDuringSlowEffect measures what two sessions'
// reports cost while a third session repeatedly runs something slow for 100ms
// — the shape of the user-visible stall. The holders go through the real
// paths, so the numbers track whatever locks those take
// (docs/daemon-locking.md):
//
//   - host-action: a host `start` of a3 (a creation, a restart);
//   - peer-report: a3's own report, stalled inside its write;
//   - peer-credential: a3's GitHub credential read, stalled in its helper;
//   - reconcile: a reconcile pass, stalled while it opens a3's mailbox.
//
// It reports per-report p50/max latency rather than throughput.
func BenchmarkConcurrentReportsDuringSlowEffect(b *testing.B) {
	b.Run("host-action", func(b *testing.B) {
		benchmarkReportsDuringSlowHolder(b, func(d *Daemon, runtime *sessionRuntime, slow func()) func() {
			d.agentsUnder = func(string) ([]string, error) {
				slow()
				return nil, nil
			}
			return func() { d.handle(context.Background(), core.Action{Action: core.ActionStart, ID: "a3"}) }
		})
	})
	b.Run("peer-report", func(b *testing.B) {
		benchmarkReportsDuringSlowHolder(b, func(d *Daemon, runtime *sessionRuntime, slow func()) func() {
			runtime.beforeReportEffect = func(id string) {
				if id == "a3" {
					slow()
				}
			}
			return nil // the holder is a3's own report loop
		})
	})
	b.Run("peer-credential", func(b *testing.B) {
		ghConfig := b.TempDir()
		b.Setenv("GH_CONFIG_DIR", ghConfig)
		if err := os.WriteFile(filepath.Join(ghConfig, "hosts.yml"), []byte("github.com:\n  user: selected\n"), 0600); err != nil {
			b.Fatal(err)
		}
		benchmarkReportsDuringSlowHolder(b, func(d *Daemon, runtime *sessionRuntime, slow func()) func() {
			runtime.credentialOperation = func(context.Context, credentialbroker.Operation) (credentialbroker.Result, error) {
				slow()
				return credentialbroker.Result{Value: "synthetic"}, nil
			}
			o := credentialbroker.Operation{Verb: credentialbroker.Read, Service: "gh:github.com", Account: credentialbroker.GitHubAccount}
			call := sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: o.Verb, Fields: o.Fields()}
			principal := principalFor(b, d, "a3")
			return func() {
				if result, _ := runtime.dispatch(context.Background(), sessionrpc.DispatchRequest{Principal: principal, Call: call}); result.Status != sessionrpc.StatusOK {
					b.Errorf("credential = %+v", result)
				}
			}
		})
	})
	b.Run("reconcile", func(b *testing.B) {
		benchmarkReportsDuringSlowHolder(b, func(d *Daemon, runtime *sessionRuntime, slow func()) func() {
			d.launchSpec = func(_ context.Context, id string) (panespec.LaunchSpec, error) {
				if id == "a3" {
					slow()
				}
				return panespec.LaunchSpec{}, errors.New("bench: no launch spec")
			}
			return func() {
				if err := runtime.reconcile(context.Background()); err != nil {
					b.Error(err)
				}
			}
		})
	})
}

// benchmarkReportsDuringSlowHolder runs a1 and a2's report pairs while a holder
// for a3 repeats. setup wires slow (a 100ms stall) into the holder's path and
// returns one holder iteration, or nil to repeat a3's report.
func benchmarkReportsDuringSlowHolder(b *testing.B, setup func(d *Daemon, runtime *sessionRuntime, slow func()) func()) {
	store.ShareHandles(true)
	defer store.ShareHandles(false)
	const runtimeID = "33333333-3333-4333-8333-333333333333"
	sessions := []store.Session{
		{ID: "a1", RootID: "root", Agent: "claude", ClaudeID: runtimeID},
		{ID: "a2", RootID: "root", Agent: "claude", ClaudeID: runtimeID},
		{ID: "a3", RootID: "root", Agent: "claude", ClaudeID: runtimeID},
	}
	d, runtime, principals := sessionRuntimeFixture(b, sessions...)
	d.sessionRPC = runtime
	b.Cleanup(runtime.completions.close)
	d.engine = newFakeEngine()
	generations := map[string]string{}
	for _, s := range sessions {
		generations[s.ID] = publishTestRuntime(b, d, s.ID)
	}
	report := func(id string) error {
		for _, req := range []sessionrpc.DispatchRequest{
			{Principal: principals[id], RequestID: "0123456789abcdef0123456789abcdef",
				Call: sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: sessionreport.Context}},
			reportRequest(principals[id], sessionreport.Activity, map[string]string{
				sessionreport.FieldRuntimeGeneration: generations[id], sessionreport.FieldState: core.StateRunning,
			}),
		} {
			if result, err := runtime.dispatch(context.Background(), req); err != nil || result.Status != sessionrpc.StatusOK {
				return fmt.Errorf("report %s = %+v, %v", id, result, err)
			}
		}
		return nil
	}
	hold := setup(d, runtime, func() { time.Sleep(100 * time.Millisecond) })
	if hold == nil {
		hold = func() {
			if err := report("a3"); err != nil {
				b.Error(err)
			}
		}
	}
	stop := make(chan struct{})
	holderDone := make(chan struct{})
	go func() {
		defer close(holderDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			hold()
			time.Sleep(10 * time.Millisecond)
		}
	}()
	var mu sync.Mutex
	var latencies []time.Duration
	b.ResetTimer()
	var wg sync.WaitGroup
	for _, s := range sessions[:2] {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for i := 0; i < b.N; i++ {
				start := time.Now()
				if err := report(id); err != nil {
					b.Error(err)
					return
				}
				mu.Lock()
				latencies = append(latencies, time.Since(start))
				mu.Unlock()
			}
		}(s.ID)
	}
	wg.Wait()
	b.StopTimer()
	close(stop)
	<-holderDone
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if len(latencies) > 0 {
		b.ReportMetric(float64(latencies[len(latencies)/2].Microseconds()), "p50-us")
		b.ReportMetric(float64(latencies[len(latencies)-1].Microseconds()), "max-us")
	}
}

// principalFor loads the principal of a session the fixture provisioned.
func principalFor(b *testing.B, d *Daemon, id string) access.Principal {
	credential, err := access.LoadCredential(d.authority.CredentialDir(access.SubjectSession, id))
	if err != nil {
		b.Fatal(err)
	}
	return credentialPrincipal(credential)
}
