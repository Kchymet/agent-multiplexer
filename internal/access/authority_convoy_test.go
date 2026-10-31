package access

import (
	"context"
	"crypto/rand"
	"os"
	"sync"
	"testing"
	"time"
)

func convoyAuthority(t *testing.T, subjects ...string) (*FileAuthority, map[string]Principal, map[string]Credential) {
	t.Helper()
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	principals, credentials := map[string]Principal{}, map[string]Credential{}
	for _, subject := range subjects {
		dir, err := a.Ensure(context.Background(), SubjectSession, subject)
		if err != nil {
			t.Fatal(err)
		}
		cred, err := LoadCredential(dir)
		if err != nil {
			t.Fatal(err)
		}
		credentials[subject] = cred
		principals[subject] = Principal{KeyID: cred.KeyID, SubjectID: cred.SubjectID, Kind: cred.Kind, Generation: cred.Generation}
	}
	return a, principals, credentials
}

// within fails the test unless fn returns before the deadline.
func within(t *testing.T, what string, d time.Duration, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(d):
		t.Fatalf("%s waited behind the authority", what)
	}
}

// The live convoy (a SIGUSR1 dump of a stalled daemon): one long WithValid
// lease, a registry writer waiting for it, and every mailbox's Valid,
// Verify and invalidation watch blocked behind that writer, so all sessions
// stalled for the length of the lease. None of them may wait now, and the
// writer still takes effect, for everyone, the moment it returns.
func TestAuthorityChecksDoNotWaitBehindALeaseAndAWaitingWriter(t *testing.T) {
	a, principals, credentials := convoyAuthority(t, "leased", "rotated", "served")
	invalidated, cancelWatch, err := a.WatchInvalidation(principals["rotated"])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancelWatch) // after the lease is released (cleanups run last-first)
	leased, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unlease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unlease) // before Close's cleanup, which needs the lease gone
	leaseDone := make(chan error, 1)
	go func() {
		leaseDone <- a.WithValid(context.Background(), principals["leased"], func() error {
			close(leased)
			<-release
			return nil
		})
	}()
	<-leased
	rotated := make(chan error, 1)
	go func() {
		_, err := a.Rotate(context.Background(), SubjectSession, "rotated")
		rotated <- err
	}()
	time.Sleep(50 * time.Millisecond) // the rotation is waiting to publish

	const prompt = 2 * time.Second
	within(t, "Valid", prompt, func() error { return a.Valid(context.Background(), principals["served"]) })
	within(t, "HasCurrent", prompt, func() error {
		if !a.HasCurrent(SubjectSession, "served") {
			t.Error("served has no current credential")
		}
		return nil
	})
	within(t, "Verify", prompt, func() error {
		req, err := SignRequest(credentials["served"], a.BootID(), []byte(`{"query":"snapshot"}`), time.Now(), rand.Reader)
		if err != nil {
			return err
		}
		_, err = a.Verify(context.Background(), req)
		return err
	})
	within(t, "another WithValid", prompt, func() error {
		return a.WithValid(context.Background(), principals["served"], func() error { return nil })
	})
	within(t, "WatchInvalidation", prompt, func() error {
		_, cancel, err := a.WatchInvalidation(principals["served"])
		if err == nil {
			cancel()
		}
		return err
	})
	select {
	case err := <-rotated:
		t.Fatalf("rotation published during the lease: %v", err)
	case <-invalidated:
		t.Fatal("rotation invalidated its watchers during the lease")
	default:
	}
	// The rotated subject is still valid: its rotation has not returned.
	if err := a.Valid(context.Background(), principals["rotated"]); err != nil {
		t.Fatalf("rotated subject invalid before its rotation returned: %v", err)
	}

	unlease()
	if err := <-leaseDone; err != nil {
		t.Fatal(err)
	}
	if err := <-rotated; err != nil {
		t.Fatal(err)
	}
	select {
	case <-invalidated:
	default:
		t.Fatal("rotation returned before closing the old key's watchers")
	}
	if err := a.Valid(context.Background(), principals["rotated"]); err == nil {
		t.Fatal("old generation still valid after its rotation returned")
	}
	if a.HasCurrent(SubjectSession, "rotated") != true {
		t.Fatal("rotated subject lost its current generation")
	}
}

// A writer's registry commit and credential publication are file IO
// (fsyncs), done under writeMu but not mu: they delay other writers only.
func TestAuthorityLeasesDoNotWaitForAWritersFileIO(t *testing.T) {
	a, principals, credentials := convoyAuthority(t, "rotated", "served")
	stalled, release := make(chan struct{}), make(chan struct{})
	var once, releaseOnce sync.Once
	unstall := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unstall) // before Close's cleanup, which needs the writer done
	commit := a.commit
	a.commit = func(path string, value any, mode os.FileMode) (bool, error) {
		once.Do(func() {
			close(stalled)
			<-release
		})
		return commit(path, value, mode)
	}
	revoked := make(chan error, 1)
	go func() { revoked <- a.Revoke(context.Background(), SubjectSession, "rotated") }()
	<-stalled
	const prompt = 2 * time.Second
	within(t, "Valid", prompt, func() error { return a.Valid(context.Background(), principals["served"]) })
	within(t, "WithValid", prompt, func() error {
		return a.WithValid(context.Background(), principals["served"], func() error { return nil })
	})
	within(t, "Verify", prompt, func() error {
		req, err := SignRequest(credentials["served"], a.BootID(), []byte(`{"query":"snapshot"}`), time.Now(), rand.Reader)
		if err != nil {
			return err
		}
		_, err = a.Verify(context.Background(), req)
		return err
	})
	unstall()
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	if err := a.Valid(context.Background(), principals["rotated"]); err == nil {
		t.Fatal("revoked generation still valid after Revoke returned")
	}
}

// A lease that waits for mu (here, held by a publication) reports the wait.
func TestLeaseRecordsAuthorityLockWait(t *testing.T) {
	a, principals, _ := convoyAuthority(t, "served")
	a.lockPublish()
	time.AfterFunc(100*time.Millisecond, a.mu.Unlock)
	ctx, wait := WithLockWait(context.Background())
	if err := a.WithValid(ctx, principals["served"], func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if wait.Total() < 100*time.Millisecond {
		t.Fatalf("lock wait = %v, want at least the 100ms publication", wait.Total())
	}
}
