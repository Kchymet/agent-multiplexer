package sessionrpc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/access"
)

// Regression tests for the 2026-11-22 daemon saturation: abandoned hook
// responses held the shared response budget until MaxResponseAge, stale
// claimed requests consumed whatever capacity freed up, and one refused
// telemetry call blocked every call behind it in the pass, credential reads
// included.

type shiftedClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *shiftedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *shiftedClock) advance(d time.Duration) {
	c.mu.Lock()
	c.offset += d
	c.mu.Unlock()
}

func publishCall(t *testing.T, f *fixture, call Call) access.SignedRequest {
	t.Helper()
	envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, call)
	publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
	return envelope
}

func responseExists(f *fixture, requestID string) bool {
	_, err := os.Stat(filepath.Join(f.grant.MailboxHostDir, ResponsesDirName, responseFileName(requestID)))
	return err == nil
}

// A response its client never collects (the hook timed out or was killed)
// releases its lease after the response TTL, not MaxResponseAge.
func TestAbandonedResponseLeaseReclaimedAfterTTL(t *testing.T) {
	budget := &testResponseBudget{limit: 1}
	f := newFixture(t, Callbacks{}, ServerOptions{ResponseBudget: budget, ResponseTTL: 10 * time.Second})
	snapshot := Call{Kind: CallOperation, Route: access.RouteQuery, Verb: "snapshot"}

	abandoned := publishCall(t, f, snapshot)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !responseExists(f, abandoned.RequestID) {
		t.Fatal("abandoned call was not answered")
	}
	next := publishCall(t, f, snapshot)
	if _, err := f.server.ServeOnce(context.Background()); !errors.Is(err, ErrResponseCapacity) {
		t.Fatalf("held lease admitted another call: %v", err)
	}
	age := func(d time.Duration) {
		t.Helper()
		at := time.Now().Add(-d)
		if err := os.Chtimes(filepath.Join(f.grant.MailboxHostDir, ResponsesDirName, responseFileName(abandoned.RequestID)), at, at); err != nil {
			t.Fatal(err)
		}
	}

	age(5 * time.Second) // inside the TTL: still the client's to collect
	_, _ = f.server.ServeOnce(context.Background())
	if !responseExists(f, abandoned.RequestID) || responseExists(f, next.RequestID) {
		t.Fatal("response reclaimed before its TTL")
	}

	age(11 * time.Second) // past the TTL, well inside MaxResponseAge
	for i := 0; i < 2 && !responseExists(f, next.RequestID); i++ {
		_, _ = f.server.ServeOnce(context.Background())
	}
	if responseExists(f, abandoned.RequestID) {
		t.Fatal("uncollected response still held after its TTL")
	}
	if !responseExists(f, next.RequestID) {
		t.Fatal("queued call not answered after the abandoned lease was reclaimed")
	}
	if active, _, releases := budget.counts(); active != 1 || releases != 1 {
		t.Fatalf("lease accounting active=%d releases=%d, want 1 and 1", active, releases)
	}
}

// A claimed request whose client stopped waiting is discarded unanswered
// instead of taking a response slot for a rejection nobody reads.
func TestStaleClaimedRequestDroppedWithoutResponse(t *testing.T) {
	clock := &shiftedClock{}
	budget := &testResponseBudget{limit: 1}
	var dispatched atomic.Int32
	f := newFixture(t, Callbacks{Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
		dispatched.Add(1)
		return DispatchResult{Status: StatusOK}, nil
	}}, ServerOptions{ResponseBudget: budget, Clock: clock.now})
	snapshot := Call{Kind: CallOperation, Route: access.RouteQuery, Verb: "snapshot"}
	held := publishCall(t, f, snapshot)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	stale := publishCall(t, f, snapshot)
	if _, err := f.server.ServeOnce(context.Background()); !errors.Is(err, ErrResponseCapacity) {
		t.Fatalf("second call not blocked: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(f.grant.MailboxHostDir, processingDirName)); len(entries) != 1 {
		t.Fatalf("blocked call not left claimed: %d entries", len(entries))
	}

	clock.advance(access.MaxRequestAge + expiredDropAfter + time.Second)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if responseExists(f, stale.RequestID) || dispatched.Load() != 1 {
		t.Fatalf("stale claimed call answered=%v dispatched=%d", responseExists(f, stale.RequestID), dispatched.Load())
	}
	if entries, _ := os.ReadDir(filepath.Join(f.grant.MailboxHostDir, processingDirName)); len(entries) != 0 {
		t.Fatalf("stale claimed call left in processing: %d entries", len(entries))
	}
	if responseExists(f, held.RequestID) {
		t.Fatal("abandoned response outlived its TTL")
	}
}

// classBudget refuses telemetry outright and admits everything else, like
// the daemon budget once telemetry has used its share.
type classBudget struct{ testResponseBudget }

func (b *classBudget) ReserveResponse(ctx context.Context, request ResponseReservation) (ResponseLease, error) {
	if request.Priority < PriorityNormal && !request.Existing {
		return nil, ErrResponseCapacity
	}
	return b.testResponseBudget.ReserveResponse(ctx, request)
}

// A credential read queued behind refused telemetry is admitted in the same
// pass (before the fix, the first refusal blocked every later call).
func TestControlAdmittedBehindRefusedTelemetry(t *testing.T) {
	budget := &classBudget{testResponseBudget{limit: 100}}
	var dispatched []string
	f := newFixture(t, Callbacks{Dispatch: func(_ context.Context, request DispatchRequest) (DispatchResult, error) {
		dispatched = append(dispatched, request.Call.Verb)
		return DispatchResult{Status: StatusOK}, nil
	}}, ServerOptions{ResponseBudget: budget, Priority: func(call Call) Priority {
		if call.Verb == "report-activity" {
			return PriorityTelemetry
		}
		return PriorityControl
	}})
	earlier := time.Now().Add(-time.Second)
	for i := 0; i < 5; i++ {
		envelope, encoded := earlierSignedCall(t, f, Call{Kind: CallOperation, Route: access.RouteAction, Verb: "report-activity"},
			earlier.Add(time.Duration(i)*time.Millisecond))
		publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
	}
	credential := publishCall(t, f, Call{Kind: CallOperation, Route: access.RouteQuery, Verb: "credential-read"})
	if _, err := f.server.ServeOnce(context.Background()); !errors.Is(err, ErrResponseCapacity) {
		t.Fatalf("refused telemetry not reported: %v", err)
	}
	if len(dispatched) != 1 || dispatched[0] != "credential-read" || !responseExists(f, credential.RequestID) {
		t.Fatalf("dispatched %v; the credential read must be served behind refused telemetry", dispatched)
	}
}
