package direct

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCodeIsSingleUseUnderConcurrency(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	code, _, err := a.NewCode("x", []string{PermRead}, allSessions, 0)
	if err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.RedeemCode(code, "c", ClientWeb); err == nil {
				wins.Add(1)
			} else if !errors.Is(err, errInvalidCode) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("code redeemed %d times", wins.Load())
	}
	if len(a.List()) != 1 {
		t.Fatalf("grants: %d", len(a.List()))
	}
}

func TestCodeExpiryAndExplicitScope(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, c.now)
	if _, _, err := a.NewCode("x", []string{PermSteer}, allSessions, 0); err == nil {
		t.Fatal("steer without read accepted")
	}
	if _, _, err := a.NewCode("x", []string{PermRead}, ResourceScope{}, 0); err == nil {
		t.Fatal("implicit resource scope accepted")
	}
	if _, _, err := a.NewCode("x", []string{PermRead}, allSessions, 2*time.Hour); err == nil {
		t.Fatal("over-long code lifetime accepted")
	}
	code, _, err := a.NewCode("x", []string{PermRead}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c.add(time.Minute)
	if _, err := a.RedeemCode(code, "c", ClientWeb); !errors.Is(err, errInvalidCode) {
		t.Fatalf("expired code: %v", err)
	}
	code, _, _ = a.NewCode("x", []string{PermRead}, ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}}, 0)
	tok, err := a.RedeemCode(code, "c", ClientWeb)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Scope != "read" || tok.ResourceScope.Kind != ResourceWorkgroups || tok.ResourceScope.Workgroups[0] != "wg1" ||
		tok.TokenType != "Bearer" || tok.ExpiresIn != 600 || tok.ServerID != a.ServerID() {
		t.Fatalf("token response: %+v", tok)
	}
	if len(tok.AccessToken) < len(AccessTokenPrefix)+43 || len(tok.RefreshToken) < len(RefreshTokenPrefix)+43 {
		t.Fatal("tokens carry fewer than 32 random bytes")
	}
}

func TestRefreshRotatesAndReuseRevokesTheGrant(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	first := enroll(t, a, []string{PermRead}, allSessions)
	second, err := a.Refresh(first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if second.RefreshToken == first.RefreshToken || second.AccessToken == first.AccessToken {
		t.Fatal("refresh did not rotate")
	}
	l, err := a.Admit(context.Background(), second.AccessToken, PermRead)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() { <-l.Context().Done(); close(released); l.Done() }() // a handler releases on cancel
	// Presenting the consumed token again ends the whole family, including the
	// live successor and its open lease.
	if _, err := a.Refresh(first.RefreshToken); !errors.Is(err, errInvalidCode) {
		t.Fatalf("reuse: %v", err)
	}
	select {
	case <-released:
	default:
		t.Fatal("reuse did not cancel and drain the open lease")
	}
	if !l.Revoked() {
		t.Fatal("lease not revoked")
	}
	if _, err := a.Admit(context.Background(), second.AccessToken, PermRead); !errors.Is(err, errAuthRevoked) {
		t.Fatalf("successor access after reuse: %v", err)
	}
	if _, err := a.Refresh(second.RefreshToken); !errors.Is(err, errInvalidCode) {
		t.Fatalf("successor refresh after reuse: %v", err)
	}
}

func TestRefreshRotationLimitFailsClosedWithoutEvictingHistory(t *testing.T) {
	old := maxUsedRefresh
	maxUsedRefresh = 3
	t.Cleanup(func() { maxUsedRefresh = old })
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	tok := enroll(t, a, []string{PermRead}, allSessions)
	first := tok.RefreshToken
	for range 3 {
		var err error
		if tok, err = a.Refresh(tok.RefreshToken); err != nil {
			t.Fatal(err)
		}
	}
	// At the bound the grant ends rather than forgetting the oldest token.
	if _, err := a.Refresh(tok.RefreshToken); !errors.Is(err, errInvalidCode) {
		t.Fatalf("refresh at the bound: %v", err)
	}
	if a.List()[0].RevokedAt == 0 {
		t.Fatal("grant not revoked at the rotation bound")
	}
	if _, err := a.Refresh(first); !errors.Is(err, errInvalidCode) {
		t.Fatal("first token accepted")
	}
}

func TestRefreshIdleAndAbsoluteExpiry(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a := openTestAuthority(t, t.TempDir(), Lifetimes{Idle: time.Hour, Absolute: 3 * time.Hour}, c.now)
	tok := enroll(t, a, []string{PermRead}, allSessions)
	c.add(59 * time.Minute)
	tok, err := a.Refresh(tok.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	c.add(61 * time.Minute)
	if _, err := a.Refresh(tok.RefreshToken); !errors.Is(err, errInvalidCode) {
		t.Fatalf("idle-expired refresh: %v", err)
	}
	tok = enroll(t, a, []string{PermRead}, allSessions)
	for range 6 {
		c.add(30 * time.Minute)
		if tok2, err := a.Refresh(tok.RefreshToken); err == nil {
			tok = tok2
		} else {
			if !errors.Is(err, errInvalidCode) {
				t.Fatal(err)
			}
			return // absolute bound reached
		}
	}
	t.Fatal("grant outlived its absolute lifetime")
}

func TestLifetimesOnlyShorten(t *testing.T) {
	if _, err := OpenAuthority(t.TempDir(), Lifetimes{Access: 11 * time.Minute}, nil); err == nil {
		t.Fatal("access lifetime above 10m accepted")
	}
}

func TestAccessTokensAreMemoryOnlyAndGrantsPersist(t *testing.T) {
	root := t.TempDir()
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	id := a.ServerID()
	if _, err := OpenAuthority(root, Lifetimes{}, nil); !errors.Is(err, ErrStoreBusy) {
		t.Fatalf("second owner: %v", err)
	}
	a.Close()
	b := openTestAuthority(t, root, Lifetimes{}, nil)
	if b.ServerID() != id {
		t.Fatal("identity changed across restart")
	}
	if _, err := b.Admit(context.Background(), tok.AccessToken, PermRead); !errors.Is(err, errAuthInvalid) {
		t.Fatalf("access token survived restart: %v", err)
	}
	if _, err := b.Refresh(tok.RefreshToken); err != nil {
		t.Fatalf("refresh after restart: %v", err)
	}
	for _, name := range []string{grantsFile, identityName} {
		st, err := os.Stat(filepath.Join(root, name))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode: %v %v", name, st.Mode(), err)
		}
	}
	if st, _ := os.Stat(root); st.Mode().Perm() != 0o700 {
		t.Fatalf("root mode %v", st.Mode())
	}
}

// failingWrites injects a store failure on the next write: before the rename
// (nothing on disk changes) or after it (content renamed in, sync failed).
func failingWrites(a *Authority, ambiguous bool) {
	a.st.write = func(dir, name string, data []byte) error {
		if !ambiguous {
			return errNotCommitted
		}
		if err := writeTemp(dir, name, data); err != nil {
			return err
		}
		return errAmbiguousCommit
	}
}

func TestRedeemFailureBeforeRenameRollsBack(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	code, _, _ := a.NewCode("x", []string{PermRead}, allSessions, 0)
	failingWrites(a, false)
	if _, err := a.RedeemCode(code, "c", ClientWeb); !errors.Is(err, errNotCommitted) {
		t.Fatalf("redeem: %v", err)
	}
	a.st.write = writeAtomic
	if len(a.List()) != 0 {
		t.Fatal("rolled-back grant visible")
	}
	if _, err := a.RedeemCode(code, "c", ClientWeb); err != nil {
		t.Fatalf("code unusable after an uncommitted attempt: %v", err)
	}
}

func TestRedeemAmbiguousCommitConsumesCodeAndDegrades(t *testing.T) {
	root := t.TempDir()
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ := a.NewCode("x", []string{PermRead}, allSessions, 0)
	failingWrites(a, true)
	if _, err := a.RedeemCode(code, "c", ClientWeb); !errors.Is(err, errAmbiguousCommit) {
		t.Fatalf("redeem: %v", err)
	}
	a.st.write = writeAtomic
	if _, _, err := a.NewCode("y", []string{PermRead}, allSessions, 0); !errors.Is(err, errDegraded) {
		t.Fatalf("issuance after ambiguous commit: %v", err)
	}
	if _, err := a.RedeemCode(code, "c", ClientWeb); err == nil {
		t.Fatal("code reusable after ambiguous commit")
	}
	a.Close()
	// A restart re-reads disk: the renamed content is authoritative and the
	// code stays consumed.
	b := openTestAuthority(t, root, Lifetimes{}, nil)
	if _, err := b.RedeemCode(code, "c", ClientWeb); !errors.Is(err, errInvalidCode) {
		t.Fatalf("code after restart: %v", err)
	}
	if _, _, err := b.NewCode("y", []string{PermRead}, allSessions, 0); err != nil {
		t.Fatalf("restart did not clear degraded state: %v", err)
	}
}

func TestRefreshFailureBothSidesOfRename(t *testing.T) {
	t.Run("not committed keeps the old token valid", func(t *testing.T) {
		a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
		tok := enroll(t, a, []string{PermRead}, allSessions)
		failingWrites(a, false)
		if _, err := a.Refresh(tok.RefreshToken); !errors.Is(err, errNotCommitted) {
			t.Fatalf("refresh: %v", err)
		}
		a.st.write = writeAtomic
		if _, err := a.Refresh(tok.RefreshToken); err != nil {
			t.Fatalf("old token after rollback: %v", err)
		}
	})
	t.Run("ambiguous consumes the old token and returns nothing", func(t *testing.T) {
		root := t.TempDir()
		a, err := OpenAuthority(root, Lifetimes{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		tok := enroll(t, a, []string{PermRead}, allSessions)
		failingWrites(a, true)
		if resp, err := a.Refresh(tok.RefreshToken); !errors.Is(err, errAmbiguousCommit) || resp.RefreshToken != "" {
			t.Fatalf("refresh: %v %+v", err, resp)
		}
		a.Close()
		b := openTestAuthority(t, root, Lifetimes{}, nil)
		// The successor committed; presenting the consumed token is reuse.
		if _, err := b.Refresh(tok.RefreshToken); !errors.Is(err, errInvalidCode) {
			t.Fatalf("consumed token: %v", err)
		}
		if b.List()[0].RevokedAt == 0 {
			t.Fatal("reuse after ambiguous commit did not revoke")
		}
	})
}

func TestRevokeBarrier(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	victim := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	other := enroll(t, a, []string{PermRead}, allSessions)
	l, err := a.Admit(context.Background(), victim.AccessToken, PermSteer)
	if err != nil {
		t.Fatal(err)
	}
	ol, err := a.Admit(context.Background(), other.AccessToken, PermRead)
	if err != nil {
		t.Fatal(err)
	}
	defer ol.Done()
	var finished atomic.Bool
	go func() { // an in-flight privileged call honoring its context
		<-l.Context().Done()
		time.Sleep(50 * time.Millisecond)
		finished.Store(true)
		l.Done()
	}()
	if err := a.Revoke(victim.ClientID, "test"); err != nil {
		t.Fatal(err)
	}
	if !finished.Load() {
		t.Fatal("Revoke returned before the in-flight operation finished")
	}
	if _, err := a.Admit(context.Background(), victim.AccessToken, PermRead); !errors.Is(err, errAuthRevoked) {
		t.Fatalf("admission after revoke: %v", err)
	}
	if _, err := a.Refresh(victim.RefreshToken); !errors.Is(err, errInvalidCode) {
		t.Fatalf("refresh after revoke: %v", err)
	}
	if ol.Context().Err() != nil {
		t.Fatal("another client's lease was cancelled")
	}
	if _, err := a.Admit(context.Background(), other.AccessToken, PermRead); err != nil {
		t.Fatalf("other client: %v", err)
	}
}

func TestLeaseEndsAtAccessExpiry(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{Access: time.Second}, nil)
	tok := enroll(t, a, []string{PermRead}, allSessions)
	l, err := a.Admit(context.Background(), tok.AccessToken, PermRead)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Done()
	select {
	case <-l.Context().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("lease outlived its access token")
	}
	if !l.Expired() || l.Revoked() || l.AuthEnded() != CodeAuthExpired {
		t.Fatalf("cause: %v", context.Cause(l.Context()))
	}
}

func TestResetIdentityRevokesAndDrainsOnEveryExit(t *testing.T) {
	for _, fail := range []string{"", "state", "identity"} {
		t.Run("fail="+fail, func(t *testing.T) {
			a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
			tok := enroll(t, a, []string{PermRead}, allSessions)
			old := a.ServerID()
			l, err := a.Admit(context.Background(), tok.AccessToken, PermRead)
			if err != nil {
				t.Fatal(err)
			}
			go func() { <-l.Context().Done(); l.Done() }()
			calls := 0
			a.st.write = func(dir, name string, data []byte) error {
				calls++
				if fail == "state" && name == grantsFile || fail == "identity" && name == identityName {
					return errNotCommitted
				}
				return writeAtomic(dir, name, data)
			}
			id, err := a.ResetIdentity()
			select {
			case <-l.done:
			default:
				t.Fatal("lease still running after ResetIdentity")
			}
			if _, aerr := a.Admit(context.Background(), tok.AccessToken, PermRead); aerr == nil {
				t.Fatal("old grant still admits")
			}
			if fail == "" {
				if err != nil || id == old || a.ServerID() != id {
					t.Fatalf("reset: %v %s", err, id)
				}
				return
			}
			if err == nil {
				t.Fatal("injected failure not reported")
			}
			if _, _, err := a.NewCode("x", []string{PermRead}, allSessions, 0); !errors.Is(err, errDegraded) {
				t.Fatalf("issuance after failed reset: %v", err)
			}
		})
	}
}

func TestIdentityChangeOnDiskRevokesGrants(t *testing.T) {
	root := t.TempDir()
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := enroll(t, a, []string{PermRead}, allSessions)
	a.Close()
	// A crash between the two writes of a reset leaves a new identity beside a
	// registry for the old one.
	if err := os.Remove(filepath.Join(root, identityName)); err != nil {
		t.Fatal(err)
	}
	b := openTestAuthority(t, root, Lifetimes{}, nil)
	if _, err := b.Refresh(tok.RefreshToken); !errors.Is(err, errInvalidCode) {
		t.Fatalf("grant from the old identity: %v", err)
	}
}
