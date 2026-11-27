package direct

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A rotation whose answer was lost: the client never saw the successor, so
// it presents the token it had again. Within RefreshGrace that rotates again
// instead of revoking the grant, and the lost answer's tokens are retired.
func TestRefreshGraceRecoversALostAnswer(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, c.now)
	first := enroll(t, a, []string{PermRead}, allSessions)
	lost, err := a.Refresh(first.RefreshToken) // the answer never arrives
	if err != nil {
		t.Fatal(err)
	}
	c.add(10 * time.Second)
	again, err := a.Refresh(first.RefreshToken)
	if err != nil {
		t.Fatalf("retry within the grace: %v", err)
	}
	if again.RefreshToken == lost.RefreshToken || again.AccessToken == lost.AccessToken {
		t.Fatal("the retry did not rotate")
	}
	if _, err := a.Admit(context.Background(), lost.AccessToken, PermRead); err == nil {
		t.Fatal("the lost answer's access token stayed usable")
	}
	l, err := a.Admit(context.Background(), again.AccessToken, PermRead)
	if err != nil {
		t.Fatalf("new access token: %v", err)
	}
	l.Done()
	if st := a.RefreshStats(); st.OK != 1 || st.GraceRetry != 1 || st.Reused != 0 {
		t.Fatalf("stats: %+v", st)
	}
	// The retired successor is consumed history: presenting it is reuse and
	// revokes the grant, so reuse detection survives the grace.
	if _, err := a.Refresh(lost.RefreshToken); !errors.Is(err, errInvalidCode) || refreshReason(err) != RefreshReasonReused {
		t.Fatalf("retired successor: %v", err)
	}
	if a.List()[0].RevokedAt == 0 {
		t.Fatal("presenting the retired successor did not revoke")
	}
}

// Using anything the rotation issued proves its answer arrived: from then on
// the consumed token is plain reuse, exactly as without the grace.
func TestRefreshGraceEndsOnceTheAnswerIsUsed(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	first := enroll(t, a, []string{PermRead}, allSessions)
	second, err := a.Refresh(first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	l, err := a.Admit(context.Background(), second.AccessToken, PermRead)
	if err != nil {
		t.Fatal(err)
	}
	l.Done()
	if _, err := a.Refresh(first.RefreshToken); refreshReason(err) != RefreshReasonReused {
		t.Fatalf("reuse after the answer was used: %v", err)
	}
	if a.List()[0].RevokedAt == 0 {
		t.Fatal("reuse did not revoke")
	}
}

func TestRefreshGraceExpires(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, c.now)
	first := enroll(t, a, []string{PermRead}, allSessions)
	if _, err := a.Refresh(first.RefreshToken); err != nil {
		t.Fatal(err)
	}
	c.add(RefreshGrace + time.Second)
	if _, err := a.Refresh(first.RefreshToken); refreshReason(err) != RefreshReasonReused {
		t.Fatalf("after the grace: %v", err)
	}
}

// Retries never extend the window, and only the token consumed by the latest
// rotation qualifies: an older one is reuse.
func TestRefreshGraceIsBounded(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, c.now)
	first := enroll(t, a, []string{PermRead}, allSessions)
	if _, err := a.Refresh(first.RefreshToken); err != nil {
		t.Fatal(err)
	}
	c.add(RefreshGrace / 2)
	if _, err := a.Refresh(first.RefreshToken); err != nil {
		t.Fatalf("first retry: %v", err)
	}
	c.add(RefreshGrace/2 + time.Second) // past the first rotation's window
	if _, err := a.Refresh(first.RefreshToken); refreshReason(err) != RefreshReasonReused {
		t.Fatalf("a retry extended the window: %v", err)
	}

	b := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	one := enroll(t, b, []string{PermRead}, allSessions)
	two, err := b.Refresh(one.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Refresh(two.RefreshToken); err != nil { // a normal rotation: one is now older history
		t.Fatal(err)
	}
	if _, err := b.Refresh(one.RefreshToken); refreshReason(err) != RefreshReasonReused {
		t.Fatalf("an older consumed token was accepted: %v", err)
	}
}

// A restart forgets every grace (it is memory only): the consumed token is
// reuse again, as before the grace existed.
func TestRefreshGraceDoesNotSurviveRestart(t *testing.T) {
	root := t.TempDir()
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := enroll(t, a, []string{PermRead}, allSessions)
	if _, err := a.Refresh(first.RefreshToken); err != nil {
		t.Fatal(err)
	}
	a.Close()
	b := openTestAuthority(t, root, Lifetimes{}, nil)
	if _, err := b.Refresh(first.RefreshToken); refreshReason(err) != RefreshReasonReused {
		t.Fatalf("after restart: %v", err)
	}
}

func TestRefreshRefusalReasons(t *testing.T) {
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	a := openTestAuthority(t, t.TempDir(), Lifetimes{Idle: time.Hour}, c.now)
	if _, err := a.Refresh("amxr_unknown"); refreshReason(err) != RefreshReasonUnknown || !errors.Is(err, errInvalidCode) {
		t.Fatalf("unknown: %v", err)
	}
	tok := enroll(t, a, []string{PermRead}, allSessions)
	c.add(2 * time.Hour)
	if _, err := a.Refresh(tok.RefreshToken); refreshReason(err) != RefreshReasonExpired {
		t.Fatalf("idle expiry: %v", err)
	}
	tok2 := enroll(t, a, []string{PermRead}, allSessions)
	if err := a.Revoke(tok2.ClientID, "test"); err != nil {
		t.Fatal(err)
	}
	// Revocation forgets the grant's refresh hash, so its token reads as unknown.
	if _, err := a.Refresh(tok2.RefreshToken); refreshReason(err) != RefreshReasonUnknown {
		t.Fatalf("revoked: %v", err)
	}
	if st := a.RefreshStats(); st.Unknown != 2 || st.Expired != 1 {
		t.Fatalf("stats: %+v", st)
	}
}
