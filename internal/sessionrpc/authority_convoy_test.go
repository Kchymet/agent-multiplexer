package sessionrpc

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"amux/internal/access"
)

// The live stall: a read lease on the authority is held across slow work
// while a registry writer queues behind it, so every new reader blocks. A
// backlog of already-expired requests needs no authority state to reject, but
// signing each rejection must not wait for the lease, or the mailbox (and every
// other mailbox) freezes with it.
func TestExpiredBacklogDrainsWhileAuthorityWriterQueued(t *testing.T) {
	const backlog = 50
	const lease = 2 * time.Second
	f := newFixture(t, Callbacks{}, ServerOptions{})
	ctx := context.Background()
	if _, err := f.auth.Ensure(ctx, access.SubjectSession, "subject-b"); err != nil {
		t.Fatal(err)
	}
	requests := filepath.Join(f.grant.MailboxHostDir, RequestsDirName)
	issued := time.Now().Add(-3 * time.Minute)
	for i := 0; i < backlog; i++ {
		envelope, encoded := earlierSignedCall(t, f, Query{Verb: "report-context"}.call(), issued)
		publishEnvelope(t, requests, envelope, encoded)
	}

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
	rotated := make(chan struct{})
	go func() {
		defer close(rotated)
		_, _ = f.auth.Rotate(ctx, access.SubjectSession, "subject-b")
	}()
	// Let the rotation queue behind the lease; from then on new readers block.
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	time.AfterFunc(lease, unlease)

	served := make(chan error, 1)
	go func() {
		_, err := f.server.ServeOnce(ctx)
		served <- err
	}()
	drained := func() bool {
		left, err := os.ReadDir(requests)
		return err == nil && len(left) == 0
	}
	for !drained() && time.Since(start) < lease/2 {
		time.Sleep(5 * time.Millisecond)
	}
	if !drained() {
		left, _ := os.ReadDir(requests)
		t.Fatalf("expired backlog stalled behind the authority lock: %d of %d still queued after %v", len(left), backlog, time.Since(start))
	}
	t.Logf("%d expired requests rejected in %v with an authority writer queued", backlog, time.Since(start))
	unlease()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	<-rotated
	if n := responseCount(t, f.grant.MailboxHostDir); n != backlog {
		t.Fatalf("responses = %d, want %d", n, backlog)
	}
}
