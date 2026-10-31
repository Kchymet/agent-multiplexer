package sessionrpc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"amux/internal/access"
)

func queuedRequests(t *testing.T, mailboxDir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(mailboxDir, RequestsDirName))
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

// An abandoned request long past expiry is discarded unclaimed and unanswered;
// one only just expired is still claimed and answered "expired", as before.
func TestServerDropsLongExpiredRequestsUnanswered(t *testing.T) {
	var dispatched atomic.Int32
	f := newFixture(t, Callbacks{Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
		dispatched.Add(1)
		return DispatchResult{Status: StatusOK}, nil
	}}, ServerOptions{})
	requests := filepath.Join(f.grant.MailboxHostDir, RequestsDirName)
	call := Query{Verb: "report-context"}.call()
	abandoned, encoded := earlierSignedCall(t, f, call, time.Now().Add(-access.MaxRequestAge-expiredDropAfter-time.Second))
	publishEnvelope(t, requests, abandoned, encoded)
	recent, encoded := earlierSignedCall(t, f, call, time.Now().Add(-access.MaxRequestAge-requestClockSkew-time.Second))
	publishEnvelope(t, requests, recent, encoded)

	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := queuedRequests(t, f.grant.MailboxHostDir); n != 0 {
		t.Fatalf("queued requests = %d, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(f.grant.MailboxHostDir, ResponsesDirName, responseFileName(abandoned.RequestID))); !os.IsNotExist(err) {
		t.Fatalf("abandoned request was answered: %v", err)
	}
	if response := readTestResponse(t, f.grant.MailboxHostDir, recent.RequestID); response.Code != "expired" {
		t.Fatalf("recently expired response = %s %s, want expired", response.Status, response.Code)
	}
	if dispatched.Load() != 0 {
		t.Fatal("an expired request was dispatched")
	}
}

// A call whose deadline passes before the daemon claims it withdraws its own
// request and reports that it did not run.
func TestClientWithdrawsUnclaimedRequestAtDeadline(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{})
	client := f.client(clientOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := client.Query(ctx, Query{Verb: "report-context"})
	if !errors.Is(err, ErrNotExecuted) || errors.Is(err, ErrIndeterminate) {
		t.Fatalf("error = %v, want ErrNotExecuted only", err)
	}
	if n := queuedRequests(t, f.grant.MailboxHostDir); n != 0 {
		t.Fatalf("withdrawn call left %d queued requests", n)
	}
	if _, err := f.server.ServeOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := responseCount(t, f.grant.MailboxHostDir); n != 0 {
		t.Fatalf("the daemon answered a withdrawn call: %d responses", n)
	}
}

// Once the daemon has claimed the request the call may be running, so a
// deadline still reports an indeterminate outcome.
func TestClientClaimedRequestAtDeadlineStaysIndeterminate(t *testing.T) {
	entered, finish := make(chan struct{}), make(chan struct{})
	f := newFixture(t, Callbacks{Dispatch: func(context.Context, DispatchRequest) (DispatchResult, error) {
		close(entered)
		<-finish
		return DispatchResult{Status: StatusOK}, nil
	}}, ServerOptions{})
	client := f.client(clientOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.Query(ctx, Query{Verb: "report-context"})
		done <- err
	}()
	// Serve until the call is dispatched: a pass can run while the request is
	// still the client's publication temporary and find nothing to claim.
	served := make(chan error, 1)
	go func() {
		for {
			if _, err := f.server.ServeOnce(context.Background()); err != nil {
				served <- err
				return
			}
			select {
			case <-entered:
				served <- nil
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	select {
	case <-entered:
	case err := <-served:
		t.Fatalf("server stopped before dispatch: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the request was never dispatched")
	}
	cancel()
	err := <-done
	close(finish)
	if !errors.Is(err, ErrIndeterminate) || errors.Is(err, ErrNotExecuted) {
		t.Fatalf("error = %v, want ErrIndeterminate only", err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

// A caller with a longer deadline than the request's lifetime stops waiting
// for an unclaimed request once no daemon could accept it.
func TestClientWithdrawsUnclaimedRequestAfterExpiry(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{})
	// Sign the request as if issued a full lifetime ago; the client then
	// observes the present.
	var signed atomic.Bool
	clock := func() time.Time {
		if signed.CompareAndSwap(false, true) {
			return time.Now().Add(-access.MaxRequestAge - requestCancelAfter)
		}
		return time.Now()
	}
	client := f.client(clientOptions{clock: clock})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, err := client.Query(ctx, Query{Verb: "report-context"})
	if !errors.Is(err, ErrNotExecuted) {
		t.Fatalf("error = %v, want ErrNotExecuted", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("withdrawal took %v, want it at expiry, not at the caller's deadline", elapsed)
	}
	if n := queuedRequests(t, f.grant.MailboxHostDir); n != 0 {
		t.Fatalf("withdrawn call left %d queued requests", n)
	}
}
