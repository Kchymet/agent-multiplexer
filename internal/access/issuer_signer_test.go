package access

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func issuerPublicKey(t *testing.T, a *FileAuthority, subject string) ed25519.PublicKey {
	t.Helper()
	dir, err := a.Ensure(context.Background(), SubjectSession, subject)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := LoadCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := base64.RawStdEncoding.DecodeString(cred.IssuerPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return ed25519.PublicKey(pub)
}

// The live convoy: a read lease is held across slow work while a registry
// writer queues behind it, so every new reader of mu blocks. Signing a
// response must not be one of them, or every mailbox stalls with it.
func TestSignIssuerDoesNotWaitBehindQueuedWriter(t *testing.T) {
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	pub := issuerPublicKey(t, a, "a1")
	if _, err := a.Ensure(context.Background(), SubjectSession, "a2"); err != nil {
		t.Fatal(err)
	}
	cred, err := LoadCredential(a.CredentialDir(SubjectSession, "a1"))
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{KeyID: cred.KeyID, SubjectID: cred.SubjectID, Kind: cred.Kind, Generation: cred.Generation}

	leased, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unlease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unlease) // before Close's cleanup runs, which needs the lease gone
	leaseDone := make(chan error, 1)
	go func() {
		leaseDone <- a.WithValid(context.Background(), principal, func() error {
			close(leased)
			<-release
			return nil
		})
	}()
	<-leased
	rotated := make(chan error, 1)
	go func() {
		_, err := a.Rotate(context.Background(), SubjectSession, "a2")
		rotated <- err
	}()
	// TryRLock fails exactly when a writer is waiting: from then on every new
	// reader of mu blocks until the lease ends.
	for deadline := time.Now().Add(5 * time.Second); a.mu.TryRLock(); {
		a.mu.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("the rotation never queued behind the read lease")
		}
		time.Sleep(time.Millisecond)
	}

	message := []byte("amux-sessionrpc-response-v1\x00convoy")
	start := time.Now()
	signed := make(chan error, 1)
	go func() {
		if a.IssuerKeyID() == "" {
			signed <- errors.New("issuer key id empty")
			return
		}
		sig, err := a.SignIssuer(message)
		if err == nil && !ed25519.Verify(pub, message, sig) {
			err = errors.New("signature does not verify under the issuer key")
		}
		signed <- err
	}()
	select {
	case err := <-signed:
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("signed in %v behind a queued writer", time.Since(start))
	case <-time.After(2 * time.Second):
		t.Fatal("SignIssuer waited behind the authority lock")
	}

	unlease()
	for name, ch := range map[string]chan error{"lease": leaseDone, "rotate": rotated} {
		if err := <-ch; err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// Rotation and revocation change only subject credentials. Signing runs
// concurrently with them (run with -race), keeps one issuer identity, and
// leaves their visibility exactly as before: once Rotate or Revoke returns,
// the old generation is rejected.
func TestSignIssuerConcurrentWithRotationAndRevocation(t *testing.T) {
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	pub := issuerPublicKey(t, a, "a1")
	keyID := a.IssuerKeyID()

	stop := make(chan struct{})
	var signers sync.WaitGroup
	signErr := make(chan error, 8)
	for i := 0; i < 8; i++ {
		signers.Add(1)
		go func(i int) {
			defer signers.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				message := []byte(fmt.Sprintf("amux-sessionrpc-response-v1\x00%d/%d", i, n))
				sig, err := a.SignIssuer(message)
				switch {
				case err != nil:
					signErr <- err
					return
				case a.IssuerKeyID() != keyID:
					signErr <- errors.New("issuer key id changed")
					return
				case !ed25519.Verify(pub, message, sig):
					signErr <- errors.New("signature does not verify under the issuer key")
					return
				}
			}
		}(i)
	}

	ctx := context.Background()
	for round := 0; round < 20; round++ {
		old, err := LoadCredential(a.CredentialDir(SubjectSession, "a1"))
		if err != nil {
			t.Fatal(err)
		}
		oldPrincipal := Principal{KeyID: old.KeyID, SubjectID: old.SubjectID, Kind: old.Kind, Generation: old.Generation}
		oldReq, err := SignRequest(old, a.BootID(), []byte(`{"query":"snapshot"}`), time.Now(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if round%2 == 0 {
			if _, err := a.Rotate(ctx, SubjectSession, "a1"); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := a.Revoke(ctx, SubjectSession, "a1"); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Regrant(ctx, SubjectSession, "a1", old.Generation); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.Valid(ctx, oldPrincipal); err == nil {
			t.Fatalf("round %d: old generation still valid after it was replaced", round)
		}
		if _, err := a.Verify(ctx, oldReq); err == nil {
			t.Fatalf("round %d: old generation's request verified after it was replaced", round)
		}
	}
	close(stop)
	signers.Wait()
	select {
	case err := <-signErr:
		t.Fatal(err)
	default:
	}
}
