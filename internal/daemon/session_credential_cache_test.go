package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/store"
)

// The reconcile pre-check caches the credential it read, so an idle tick does
// not reread every live session's credential file, but any authority change
// (rotation, revocation) or approaching expiry still sends it back to disk.
func TestCredentialFreshCachesUntilTheAuthorityChanges(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	clock := func() time.Time { return now }
	member := store.Session{ID: "a1", RootID: "root1", Agent: "claude"}
	d, r, _ := sessionRuntimeFixtureWithClock(t, clock, store.Session{ID: "root1", Agent: "claude"}, member)
	current := filepath.Join(d.authority.CredentialDir(access.SubjectSession, member.ID), "current")
	if !r.credentialFresh(ctx, member.ID) {
		t.Fatal("freshly issued credential is not fresh")
	}
	original, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	// A cached, still-current credential is not reread: a corrupt file is
	// invisible until the authority moves on.
	info, err := os.Stat(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(current, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(current, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !r.credentialFresh(ctx, member.ID) {
		t.Fatal("cached current credential was reread from disk")
	}
	if err := os.WriteFile(current, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(current, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}

	// Rotation changes the current principal: the cache entry no longer
	// validates and the rotated file is read instead.
	if _, err := d.authority.Rotate(ctx, access.SubjectSession, member.ID); err != nil {
		t.Fatal(err)
	}
	rotated, err := r.loadCredential(member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !r.credentialFresh(ctx, member.ID) {
		t.Fatal("rotated credential is not fresh")
	}
	if got := r.freshCredentials[member.ID].principal; got != credentialPrincipal(rotated) {
		t.Fatalf("cache holds %+v after rotation, want %+v", got, credentialPrincipal(rotated))
	}

	// Approaching expiry is due for renewal even though the cache is current.
	now = time.UnixMilli(rotated.NotAfter).Add(-credentialRenewBefore)
	if r.credentialFresh(ctx, member.ID) {
		t.Fatal("credential due for renewal reported fresh")
	}
	now = time.Now()

	// Revocation invalidates the cached principal.
	if !r.credentialFresh(ctx, member.ID) {
		t.Fatal("credential not fresh again")
	}
	if err := d.authority.Revoke(ctx, access.SubjectSession, member.ID); err != nil {
		t.Fatal(err)
	}
	if r.credentialFresh(ctx, member.ID) {
		t.Fatal("revoked credential reported fresh from the cache")
	}
	if _, ok := r.freshCredentials[member.ID]; ok {
		t.Fatal("revoked credential left in the cache")
	}
}
