package sessionrpc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// unpublish removes service.json, the state of a session whose processes have
// started before the daemon's reconcile published its mailbox.
func unpublish(t *testing.T, f *fixture) {
	t.Helper()
	if err := os.Remove(filepath.Join(f.grant.MailboxHostDir, ServiceFileName)); err != nil {
		t.Fatal(err)
	}
}

func TestCallWaitsForMailboxPublication(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{})
	unpublish(t, f)
	client := f.client(clientOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := client.Query(ctx, Query{Verb: "version"})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("call before publication returned at once: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	served := make(chan error, 1)
	go func() { served <- f.server.Run(ctx) }() // Run publishes service.json
	start := time.Now()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("call after publication: %v", err)
		}
		t.Logf("answered %v after publication", time.Since(start))
	case <-time.After(5 * time.Second):
		t.Fatal("call never noticed publication")
	}
	cancel()
	<-served
}

func TestCallReportsUnpublishedWhenContextEnds(t *testing.T) {
	f := newFixture(t, Callbacks{}, ServerOptions{})
	unpublish(t, f)
	client := f.client(clientOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := client.Query(ctx, Query{Verb: "version"}); !errors.Is(err, ErrUnpublished) {
		t.Fatalf("call to an unpublished mailbox = %v, want ErrUnpublished", err)
	}
	entries, err := os.ReadDir(f.grant.RequestsHostDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unpublished call left %d request files; nothing may be sent", len(entries))
	}
}
