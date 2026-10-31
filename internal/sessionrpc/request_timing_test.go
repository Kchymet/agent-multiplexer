package sessionrpc

import (
	"context"
	"sync"
	"testing"
	"time"

	"amux/internal/access"
)

type timingLog struct {
	mu      sync.Mutex
	timings []RequestTiming
}

func (l *timingLog) record(t RequestTiming) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.timings = append(l.timings, t)
}

func (l *timingLog) all() []RequestTiming {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]RequestTiming(nil), l.timings...)
}

func TestSlowRequestReportsItsDispatchPhase(t *testing.T) {
	var log timingLog
	f := newFixture(t, Callbacks{Dispatch: func(_ context.Context, request DispatchRequest) (DispatchResult, error) {
		if request.Call.Verb == "slow" {
			time.Sleep(SlowRequestThreshold + 100*time.Millisecond)
		}
		return DispatchResult{Status: StatusOK}, nil
	}}, ServerOptions{SlowRequest: log.record})
	client := f.client(clientOptions{})
	for _, verb := range []string{"fast", "slow"} {
		if _, err := pumpClient(t, f.server, func(ctx context.Context) (Result, error) {
			return client.Query(ctx, Query{Verb: verb})
		}); err != nil {
			t.Fatal(err)
		}
	}
	got := log.all()
	if len(got) != 1 {
		t.Fatalf("slow request reports = %+v, want only the slow call", got)
	}
	timing := got[0]
	if timing.Verb != "slow" || timing.SubjectID != "subject-a" || timing.Kind != CallOperation || timing.Status != StatusOK {
		t.Fatalf("timing identity = %+v", timing)
	}
	if timing.Dispatch < SlowRequestThreshold || timing.Total < timing.Dispatch || timing.Verify >= SlowRequestThreshold {
		t.Fatalf("timing did not attribute the stall to dispatch: %+v", timing)
	}
}

// The live stall: one long authority lease (a 10s WithValid) with a registry
// writer waiting for it froze every mailbox, because Verify and Valid queued
// behind the writer. A live request is now served at once, and nothing in it
// waits on the authority lock. (access tests cover lock-wait attribution.)
func TestRequestServedDuringLongAuthorityLeaseWithWriterWaiting(t *testing.T) {
	const lease = 10 * time.Second
	var log timingLog
	f := newFixture(t, Callbacks{}, ServerOptions{SlowRequest: log.record})
	ctx := context.Background()
	if _, err := f.auth.Ensure(ctx, access.SubjectSession, "subject-b"); err != nil {
		t.Fatal(err)
	}
	envelope, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Query{Verb: "report-context"}.call())
	publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)

	credential, err := access.LoadCredential(f.grant.CredentialHostDir)
	if err != nil {
		t.Fatal(err)
	}
	principal := access.Principal{KeyID: credential.KeyID, SubjectID: credential.SubjectID, Kind: credential.Kind, Generation: credential.Generation}
	leased, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unlease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unlease)
	go func() {
		_ = f.auth.WithValid(ctx, principal, func() error {
			close(leased)
			<-release
			return nil
		})
	}()
	<-leased
	time.AfterFunc(lease, unlease)
	rotated := make(chan error, 1)
	go func() {
		_, err := f.auth.Rotate(ctx, access.SubjectSession, "subject-b")
		rotated <- err
	}()
	time.Sleep(50 * time.Millisecond) // the rotation is waiting to publish
	start := time.Now()
	served := make(chan error, 1)
	go func() {
		_, err := f.server.ServeOnce(ctx)
		served <- err
	}()
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(SlowRequestThreshold):
		t.Fatalf("request still unserved %v into a %v lease", time.Since(start), lease)
	}
	t.Logf("served in %v during a %v lease with a rotation waiting", time.Since(start), lease)
	if n := responseCount(t, f.grant.MailboxHostDir); n != 1 {
		t.Fatalf("responses = %d, want 1", n)
	}
	if got := log.all(); len(got) != 0 {
		t.Fatalf("request reported slow: %+v", got)
	}
	unlease()
	if err := <-rotated; err != nil {
		t.Fatal(err)
	}
}

// A request processed quickly after waiting in the queue is still reported:
// that wait is the stall a caller saw.
func TestQueuedRequestReportsItsWait(t *testing.T) {
	var log timingLog
	f := newFixture(t, Callbacks{}, ServerOptions{SlowRequest: log.record})
	envelope, encoded := earlierSignedCall(t, f, Query{Verb: "report-context"}.call(), time.Now().Add(-2*SlowRequestThreshold))
	publishEnvelope(t, f.grant.RequestsHostDir, envelope, encoded)
	fresh, encoded := signedCall(t, f.auth, f.grant.CredentialHostDir, Query{Verb: "report-context"}.call())
	publishEnvelope(t, f.grant.RequestsHostDir, fresh, encoded)
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := log.all()
	if len(got) != 1 || got[0].RequestID != envelope.RequestID {
		t.Fatalf("reports = %+v, want only the queued request", got)
	}
	if got[0].Age < SlowRequestThreshold || got[0].Total >= SlowRequestThreshold || got[0].Status != StatusOK {
		t.Fatalf("queued request timing = %+v", got[0])
	}
}
