package daemon

import (
	"context"
	"fmt"
	"testing"

	"amux/internal/access"
	"amux/internal/core"
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
