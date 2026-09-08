package access

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClosedAuthorityRejectsSigningWatchingAndAuthentication(t *testing.T) {
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := a.Ensure(context.Background(), SubjectSession, "a1")
	if err != nil {
		t.Fatal(err)
	}
	credential, err := LoadCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	message := []byte("amux-sessionrpc-service-v1\x00fixture")
	signature, err := a.SignIssuer(message)
	if err != nil {
		t.Fatal(err)
	}
	issuerPublic, _ := base64.RawStdEncoding.DecodeString(credential.IssuerPublicKey)
	if !ed25519.Verify(ed25519.PublicKey(issuerPublic), message, signature) {
		t.Fatal("issuer seam did not sign exact message bytes")
	}
	challenge, err := a.NewChallenge()
	if err != nil {
		t.Fatal(err)
	}
	proof, err := SignProof(credential, challenge, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	principal := Principal{KeyID: credential.KeyID, SubjectID: credential.SubjectID, Kind: credential.Kind, Generation: credential.Generation}
	request, err := SignRequest(credential, a.BootID(), []byte(`{"query":"snapshot"}`), time.Now(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	if a.IssuerKeyID() != "" {
		t.Fatal("closed authority still exposed an issuer key id")
	}
	checks := []struct {
		name string
		err  error
	}{
		{"sign", func() error { _, err := a.SignIssuer(message); return err }()},
		{"challenge", func() error { _, err := a.NewChallenge(); return err }()},
		{"tls", func() error { _, err := a.ServerTLSConfig(); return err }()},
		{"proof", func() error {
			_, err := a.VerifyProof(context.Background(), challenge, proof, SubjectSession)
			return err
		}()},
		{"request", func() error { _, err := a.Verify(context.Background(), request); return err }()},
		{"valid", a.Valid(context.Background(), principal)},
		{"watch", func() error { _, _, err := a.WatchInvalidation(principal); return err }()},
		{"current", func() error { _, err := a.Current(context.Background(), SubjectSession, "a1"); return err }()},
		{"ensure", func() error { _, err := a.Ensure(context.Background(), SubjectSession, "a1"); return err }()},
		{"rotate", func() error { _, err := a.Rotate(context.Background(), SubjectSession, "a1"); return err }()},
		{"last revoked", func() error { _, err := a.LastRevoked(context.Background(), SubjectSession, "a1"); return err }()},
		{"regrant", func() error {
			_, err := a.Regrant(context.Background(), SubjectSession, "a1", credential.Generation)
			return err
		}()},
		{"revoke current", a.RevokeCurrent(context.Background(), principal)},
	}
	for _, check := range checks {
		if !errors.Is(check.err, ErrAuthorityClosed) {
			t.Errorf("%s after Close = %v, want ErrAuthorityClosed", check.name, check.err)
		}
	}
}

func TestSignedRequestVerifyReplayAndBodyBinding(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, err := open(t.TempDir(), func() time.Time { return now }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	dir, err := a.Ensure(context.Background(), SubjectSession, "a1")
	if err != nil {
		t.Fatal(err)
	}
	cred, err := LoadCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	body := json.RawMessage(`{"action":"rename","id":"a1"}`)
	req, err := SignRequest(cred, a.BootID(), body, now, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.Verify(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if p.SubjectID != "a1" || p.Kind != SubjectSession || p.Generation != 1 {
		t.Fatalf("principal = %+v", p)
	}
	if _, err := a.Verify(context.Background(), req); !errors.Is(err, ErrReplay) {
		t.Fatalf("duplicate error = %v, want replay", err)
	}

	tampered, err := SignRequest(cred, a.BootID(), body, now, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tampered.Body = json.RawMessage(`{"action":"delete","id":"a1"}`)
	if _, err := a.Verify(context.Background(), tampered); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("tampered body error = %v", err)
	}
}

func TestReplayLedgerSurvivesRestartAndBootNonceChanges(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	root := t.TempDir()
	a1, err := open(root, func() time.Time { return now }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := a1.Ensure(context.Background(), SubjectSession, "a1")
	if err != nil {
		t.Fatal(err)
	}
	cred, _ := LoadCredential(dir)
	req, _ := SignRequest(cred, a1.BootID(), []byte(`{"query":"snapshot"}`), now, rand.Reader)
	if _, err := a1.Verify(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	oldBoot := a1.BootID()
	if err := a1.Close(); err != nil {
		t.Fatal(err)
	}

	a2, err := open(root, func() time.Time { return now }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a2.Close() })
	if a2.BootID() == a1.BootID() {
		t.Fatal("daemon restart reused boot nonce")
	}
	if _, err := a2.Verify(context.Background(), req); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("old-boot request error = %v", err)
	}
	// Even if the request is checked under its original boot (the crash window),
	// the durable ledger still prevents execution.
	a2.bootID = oldBoot
	if _, err := a2.Verify(context.Background(), req); !errors.Is(err, ErrReplay) {
		t.Fatalf("persisted duplicate error = %v", err)
	}
}

func TestRotationUsesStableDirectoryAndRevokesOldGeneration(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, err := open(t.TempDir(), func() time.Time { return now }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	dir1, err := a.Ensure(context.Background(), SubjectSession, "a1")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(dir1)
	if err != nil {
		t.Fatal(err)
	}
	old, _ := LoadCredential(dir1)
	oldReq, _ := SignRequest(old, a.BootID(), []byte(`{"action":"rename"}`), now, rand.Reader)
	dir2, err := a.Rotate(context.Background(), SubjectSession, "a1")
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(dir2)
	if dir1 != dir2 || !os.SameFile(before, after) {
		t.Fatal("rotation replaced credential directory instead of current within it")
	}
	cur, _ := LoadCredential(dir2)
	if cur.Generation != old.Generation+1 || cur.KeyID == old.KeyID {
		t.Fatalf("rotated credential = %+v, old = %+v", cur, old)
	}
	if _, err := a.Verify(context.Background(), oldReq); !errors.Is(err, ErrInvalidCredential) && !errors.Is(err, ErrRevoked) {
		t.Fatalf("old generation error = %v", err)
	}
	newReq, _ := SignRequest(cur, a.BootID(), []byte(`{"action":"rename"}`), now, rand.Reader)
	if _, err := a.Verify(context.Background(), newReq); err != nil {
		t.Fatal(err)
	}
	if err := a.Revoke(context.Background(), SubjectSession, "a1"); err != nil {
		t.Fatal(err)
	}
	if err := a.Valid(context.Background(), Principal{KeyID: cur.KeyID, SubjectID: "a1", Kind: SubjectSession, Generation: cur.Generation}); !errors.Is(err, ErrInvalidCredential) && !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked validity error = %v", err)
	}
}

func TestCurrentRecoversCommittedStagedPublicationWithoutIssuing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, err := open(t.TempDir(), func() time.Time { return now }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if _, err := a.Current(context.Background(), SubjectSession, "never"); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("never-provisioned current error = %v", err)
	}
	if _, err := a.Rotate(context.Background(), SubjectSession, "never"); !errors.Is(err, ErrNotProvisioned) {
		t.Fatalf("never-provisioned rotate error = %v", err)
	}
	if len(a.registry.Current) != 0 || len(a.registry.Records) != 0 {
		t.Fatalf("Current issued never-provisioned state: %+v", a.registry)
	}
	dir, err := a.Ensure(context.Background(), SubjectSession, "a1")
	if err != nil {
		t.Fatal(err)
	}
	old, err := LoadCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	realCommit := a.commit
	a.commit = func(path string, value any, mode os.FileMode) (bool, error) {
		committed, commitErr := realCommit(path, value, mode)
		if commitErr != nil {
			return committed, commitErr
		}
		return true, errors.New("injected post-commit failure")
	}
	if _, err := a.Rotate(context.Background(), SubjectSession, "a1"); err == nil {
		t.Fatal("uncertain rotation unexpectedly succeeded")
	}
	a.commit = realCommit

	current, err := a.Current(context.Background(), SubjectSession, "a1")
	if err != nil {
		t.Fatalf("recover committed current: %v", err)
	}
	if current.Generation != old.Generation+1 || current.KeyID == old.KeyID {
		t.Fatalf("recovered current = %+v, old = %+v", current, old)
	}
	published, err := LoadCredential(dir)
	if err != nil || published.KeyID != current.KeyID || published.Generation != current.Generation {
		t.Fatalf("published credential = %+v, err=%v", published, err)
	}
}

func TestRevokedOrExpiredCredentialCannotBeImplicitlyReissued(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, err := open(t.TempDir(), func() time.Time { return now }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if _, err := a.Ensure(context.Background(), SubjectSession, "revoked"); err != nil {
		t.Fatal(err)
	}
	if err := a.Revoke(context.Background(), SubjectSession, "revoked"); err != nil {
		t.Fatal(err)
	}
	for name, issue := range map[string]func() error{
		"current": func() error { _, err := a.Current(context.Background(), SubjectSession, "revoked"); return err },
		"ensure":  func() error { _, err := a.Ensure(context.Background(), SubjectSession, "revoked"); return err },
		"rotate":  func() error { _, err := a.Rotate(context.Background(), SubjectSession, "revoked"); return err },
	} {
		if err := issue(); !errors.Is(err, ErrRevoked) {
			t.Errorf("%s revoked error = %v", name, err)
		}
	}
	if got := a.registry.Current[currentKey(SubjectSession, "revoked")]; got != "" {
		t.Fatalf("revoked subject regained current key %q", got)
	}
	if got := len(a.registry.Records); got != 1 {
		t.Fatalf("revoked attempts changed record count to %d", got)
	}

	if _, err := a.Ensure(context.Background(), SubjectSession, "expired"); err != nil {
		t.Fatal(err)
	}
	expiredKey := a.registry.Current[currentKey(SubjectSession, "expired")]
	now = now.Add(31 * 24 * time.Hour)
	for name, issue := range map[string]func() error{
		"current": func() error { _, err := a.Current(context.Background(), SubjectSession, "expired"); return err },
		"ensure":  func() error { _, err := a.Ensure(context.Background(), SubjectSession, "expired"); return err },
		"rotate":  func() error { _, err := a.Rotate(context.Background(), SubjectSession, "expired"); return err },
	} {
		if err := issue(); !errors.Is(err, ErrExpired) {
			t.Errorf("%s expired error = %v", name, err)
		}
	}
	if got := a.registry.Current[currentKey(SubjectSession, "expired")]; got != expiredKey {
		t.Fatalf("expired subject rotated from %q to %q", expiredKey, got)
	}
	if got := len(a.registry.Records); got != 2 {
		t.Fatalf("expired attempts changed record count to %d", got)
	}
}

func TestExplicitRegrantRequiresLatestRevokedGeneration(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	root := t.TempDir()
	a, err := open(root, func() time.Time { return now }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := a.Ensure(context.Background(), SubjectSession, "a1")
	if err != nil {
		t.Fatal(err)
	}
	first, err := LoadCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	firstPrincipal := Principal{KeyID: first.KeyID, SubjectID: first.SubjectID, Kind: first.Kind, Generation: first.Generation}
	if _, err := a.LastRevoked(context.Background(), SubjectSession, "a1"); !errors.Is(err, ErrGenerationChanged) {
		t.Fatalf("active LastRevoked error = %v", err)
	}
	if _, err := a.Regrant(context.Background(), SubjectSession, "a1", first.Generation); !errors.Is(err, ErrGenerationChanged) {
		t.Fatalf("active Regrant error = %v", err)
	}
	if err := a.RevokeCurrent(context.Background(), firstPrincipal); err != nil {
		t.Fatalf("revoke exact generation: %v", err)
	}
	revoked, err := a.LastRevoked(context.Background(), SubjectSession, "a1")
	if err != nil || revoked.KeyID != first.KeyID || revoked.Generation != first.Generation || revoked.RevokedAt == 0 {
		t.Fatalf("last revoked = %+v, err=%v", revoked, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	a, err = open(root, func() time.Time { return now }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	revoked, err = a.LastRevoked(context.Background(), SubjectSession, "a1")
	if err != nil || revoked.Generation != first.Generation {
		t.Fatalf("reopened last revoked = %+v, err=%v", revoked, err)
	}
	for _, generation := range []uint64{0, first.Generation + 1} {
		if _, err := a.Regrant(context.Background(), SubjectSession, "a1", generation); !errors.Is(err, ErrGenerationChanged) {
			t.Errorf("Regrant generation %d error = %v", generation, err)
		}
	}
	if _, err := a.Regrant(context.Background(), SubjectSession, "a1", first.Generation); err != nil {
		t.Fatalf("explicit Regrant: %v", err)
	}
	second, err := LoadCredential(dir)
	if err != nil || second.Generation != first.Generation+1 || second.KeyID == first.KeyID {
		t.Fatalf("regranted credential = %+v, err=%v", second, err)
	}
	secondPrincipal := Principal{KeyID: second.KeyID, SubjectID: second.SubjectID, Kind: second.Kind, Generation: second.Generation}
	if err := a.RevokeCurrent(context.Background(), firstPrincipal); !errors.Is(err, ErrGenerationChanged) {
		t.Fatalf("stale completion revoke error = %v", err)
	}
	if err := a.Valid(context.Background(), secondPrincipal); err != nil {
		t.Fatalf("stale completion revoked replacement: %v", err)
	}
	if err := a.RevokeCurrent(context.Background(), secondPrincipal); err != nil {
		t.Fatalf("revoke regranted generation: %v", err)
	}
	latest, err := a.LastRevoked(context.Background(), SubjectSession, "a1")
	if err != nil || latest.Generation != second.Generation {
		t.Fatalf("latest revoked = %+v, err=%v", latest, err)
	}
}

func TestRegrantUncertainCommitRecoversOnlyCommittedGeneration(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, err := open(t.TempDir(), func() time.Time { return now }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	dir, err := a.Ensure(context.Background(), SubjectSession, "a1")
	if err != nil {
		t.Fatal(err)
	}
	first, _ := LoadCredential(dir)
	if err := a.Revoke(context.Background(), SubjectSession, "a1"); err != nil {
		t.Fatal(err)
	}
	realCommit := a.commit
	a.commit = func(path string, value any, mode os.FileMode) (bool, error) {
		committed, commitErr := realCommit(path, value, mode)
		if commitErr != nil {
			return committed, commitErr
		}
		return true, errors.New("injected post-commit failure")
	}
	if _, err := a.Regrant(context.Background(), SubjectSession, "a1", first.Generation); err == nil {
		t.Fatal("uncertain Regrant unexpectedly succeeded")
	}
	a.commit = realCommit
	if _, err := a.Regrant(context.Background(), SubjectSession, "a1", first.Generation); !errors.Is(err, ErrGenerationChanged) {
		t.Fatalf("retry crossed committed generation: %v", err)
	}
	current, err := a.Current(context.Background(), SubjectSession, "a1")
	if err != nil || current.Generation != first.Generation+1 {
		t.Fatalf("recover committed regrant = %+v, err=%v", current, err)
	}
	if got := len(a.registry.Records); got != 2 {
		t.Fatalf("uncertain regrant created %d records, want 2", got)
	}
}

func TestRegrantDoesNotRecoverUncommittedPublication(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, err := open(t.TempDir(), func() time.Time { return now }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	dir, err := a.Ensure(context.Background(), SubjectSession, "a1")
	if err != nil {
		t.Fatal(err)
	}
	first, _ := LoadCredential(dir)
	if err := a.Revoke(context.Background(), SubjectSession, "a1"); err != nil {
		t.Fatal(err)
	}
	realCommit := a.commit
	a.commit = func(string, any, os.FileMode) (bool, error) {
		return false, errors.New("injected pre-commit failure")
	}
	if _, err := a.Regrant(context.Background(), SubjectSession, "a1", first.Generation); err == nil {
		t.Fatal("uncommitted Regrant unexpectedly succeeded")
	}
	a.commit = realCommit
	if _, err := a.Current(context.Background(), SubjectSession, "a1"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("uncommitted generation became current: %v", err)
	}
	if got := len(a.registry.Records); got != 1 {
		t.Fatalf("uncommitted regrant changed record count to %d", got)
	}
	if _, err := a.Regrant(context.Background(), SubjectSession, "a1", first.Generation); err != nil {
		t.Fatalf("retry after definite pre-commit failure: %v", err)
	}
	current, err := a.Current(context.Background(), SubjectSession, "a1")
	if err != nil || current.Generation != first.Generation+1 {
		t.Fatalf("committed retry current = %+v, err=%v", current, err)
	}
}

func TestEnsureSessionSerializesPublicationWithClose(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state", "access", "v1")
	a, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	realCommit := a.commit
	commitEntered := make(chan struct{})
	allowCommit := make(chan struct{})
	a.commit = func(path string, value any, mode os.FileMode) (bool, error) {
		close(commitEntered)
		<-allowCommit
		return realCommit(path, value, mode)
	}
	sessionDir := filepath.Join(t.TempDir(), "session")
	ensureDone := make(chan error, 1)
	go func() {
		_, err := a.EnsureSession(context.Background(), "a1", sessionDir)
		ensureDone <- err
	}()
	<-commitEntered
	closeDone := make(chan error, 1)
	go func() { closeDone <- a.Close() }()
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before in-flight publication completed: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := Open(root); !errors.Is(err, ErrAuthorityInUse) {
		t.Fatalf("authority ownership released during publication: %v", err)
	}
	close(allowCommit)
	if err := <-ensureDone; err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := a.EnsureSession(context.Background(), "a2", filepath.Join(t.TempDir(), "session")); !errors.Is(err, ErrAuthorityClosed) {
		t.Fatalf("EnsureSession after Close = %v", err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatalf("reopen authority after serialized Close: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureSessionFreezesMailboxAndCredentialPaths(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "state", "access", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	sessionDir := filepath.Join(t.TempDir(), "sessions", "a1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := a.EnsureSession(context.Background(), "a1", sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if got.MailboxMountDir != filepath.Join(sessionDir, ".amux", "control") ||
		got.RequestsHostDir != filepath.Join(got.MailboxHostDir, "requests") ||
		got.RequestsMountDir != filepath.Join(sessionDir, ".amux", "control", "requests") ||
		got.CredentialMountDir != filepath.Join(sessionDir, ".amux", "control", "credential") ||
		!filepath.IsAbs(got.CredentialHostDir) || strings.HasPrefix(got.MailboxHostDir, sessionDir+string(os.PathSeparator)) {
		t.Fatalf("session access = %+v", got)
	}
	for _, name := range []string{"requests", "responses"} {
		info, err := os.Stat(filepath.Join(got.MailboxHostDir, name))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s: info=%v err=%v", name, info, err)
		}
	}
	info, err := os.Stat(filepath.Join(got.MailboxHostDir, CredentialDirName))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("credential mountpoint = %v, %v", info, err)
	}
	entries, err := os.ReadDir(filepath.Join(got.MailboxHostDir, CredentialDirName))
	if err != nil || len(entries) != 0 {
		t.Fatalf("host credential placeholder is not empty: %v, %v", entries, err)
	}
	var sessionContext SessionContext
	if err := readJSON(filepath.Join(got.CredentialHostDir, ContextFileName), &sessionContext); err != nil {
		t.Fatal(err)
	}
	if sessionContext.Protocol != ProtocolVersion || sessionContext.SubjectID != "a1" || sessionContext.MailboxDir != got.MailboxMountDir {
		t.Fatalf("fixed session context = %+v", sessionContext)
	}
}

func TestEnsureSessionDoesNotFollowSessionSymlink(t *testing.T) {
	root := t.TempDir()
	a, err := Open(filepath.Join(root, "state", "access", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	sessionDir := filepath.Join(root, "sessions", "a1")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sessionDir, ".amux")); err != nil {
		t.Fatal(err)
	}
	got, err := a.EnsureSession(context.Background(), "a1", sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(got.MailboxHostDir, sessionDir+string(os.PathSeparator)) {
		t.Fatalf("mailbox source is beneath attacker directory: %s", got.MailboxHostDir)
	}
	info, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("outside mode changed to %o", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(outside, "control")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside control created: %v", err)
	}
	if len(a.registry.Current) != 1 {
		t.Fatalf("daemon-private credential was not issued: %+v", a.registry.Current)
	}
}

func TestEnsureSessionRejectsReplacedMailboxChild(t *testing.T) {
	a, err := Open(filepath.Join(t.TempDir(), "state", "access", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	got, err := a.EnsureSession(context.Background(), "a1", filepath.Join(t.TempDir(), "session"))
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Rename(got.RequestsHostDir, got.RequestsHostDir+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, got.RequestsHostDir); err != nil {
		t.Fatal(err)
	}
	if _, err := a.EnsureSession(context.Background(), "a1", filepath.Join(t.TempDir(), "session")); err == nil {
		t.Fatal("replaced requests child was accepted")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("outside directory changed: entries=%v err=%v", entries, err)
	}
}

func TestSignedRequestJSONRoundTripPreservesExactBody(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, err := open(t.TempDir(), func() time.Time { return now }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	dir, _ := a.Ensure(context.Background(), SubjectSession, "a1")
	cred, _ := LoadCredential(dir)
	body := []byte("{\n  \"html\": \"<x>&\", \"space\": true\n}\n")
	req, err := SignRequest(cred, a.BootID(), body, now, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SignedRequest
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded.Body) != string(body) {
		t.Fatalf("body changed across wire: %q", decoded.Body)
	}
	if _, err := a.Verify(context.Background(), decoded); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredAndOversizedRequestsFailBeforeConsumption(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, _ := open(t.TempDir(), func() time.Time { return now }, rand.Reader)
	t.Cleanup(func() { _ = a.Close() })
	dir, _ := a.Ensure(context.Background(), SubjectSession, "a1")
	cred, _ := LoadCredential(dir)
	req, _ := SignRequest(cred, a.BootID(), []byte(`{}`), now.Add(-time.Minute), rand.Reader)
	if _, err := a.Verify(context.Background(), req); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired request error = %v", err)
	}
	if _, err := SignRequest(cred, a.BootID(), make([]byte, MaxBodyBytes+1), now, rand.Reader); !errors.Is(err, ErrInvalidCredential) {
		t.Fatalf("oversized sign error = %v", err)
	}
}

func TestReplayLedgerNeverPersistsBeyondReopenLimit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	root := t.TempDir()
	a, err := open(root, func() time.Time { return now }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := a.Ensure(context.Background(), SubjectSession, "a1")
	cred, _ := LoadCredential(dir)
	for i := 0; i < maxReplayEntries; i++ {
		id := fmt.Sprintf("%032x", i+1)
		a.replay.Entries[cred.KeyID+":"+fmt.Sprint(cred.Generation)+":"+id] = replayRecord{
			KeyID: cred.KeyID, Generation: cred.Generation, RequestID: id,
			BodyDigest: strings.Repeat("0", 64), ExpiresAt: now.Add(MaxRequestAge).UnixMilli(),
		}
	}
	req, _ := SignRequest(cred, a.BootID(), []byte(`{"query":"snapshot"}`), now, rand.Reader)
	if _, err := a.Verify(context.Background(), req); !errors.Is(err, ErrCapacity) {
		t.Fatalf("full ledger verification error = %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := open(root, func() time.Time { return now }, rand.Reader)
	if err != nil {
		t.Fatalf("bounded persisted ledger did not reopen: %v", err)
	}
	_ = reopened.Close()
}

func TestFailedAndUncertainAuthorityCommitsRemainFailClosed(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	a, err := open(t.TempDir(), func() time.Time { return now }, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	dir, _ := a.Ensure(context.Background(), SubjectSession, "a1")
	old, _ := LoadCredential(dir)
	oldPrincipal := Principal{KeyID: old.KeyID, SubjectID: old.SubjectID, Kind: old.Kind, Generation: old.Generation}
	realCommit := a.commit
	a.commit = func(string, any, os.FileMode) (bool, error) { return false, errors.New("injected pre-commit failure") }
	if _, err := a.Rotate(context.Background(), SubjectSession, "a1"); err == nil {
		t.Fatal("rotation unexpectedly succeeded")
	}
	if err := a.Valid(context.Background(), oldPrincipal); err != nil {
		t.Fatalf("pre-commit failure changed live authority: %v", err)
	}
	a.commit = func(path string, value any, mode os.FileMode) (bool, error) {
		committed, err := realCommit(path, value, mode)
		if err != nil {
			return committed, err
		}
		return true, errors.New("injected post-rename sync failure")
	}
	if _, err := a.Rotate(context.Background(), SubjectSession, "a1"); err == nil {
		t.Fatal("uncertain rotation unexpectedly returned success")
	}
	if err := a.Valid(context.Background(), oldPrincipal); err == nil {
		t.Fatal("old generation remained live after committed rotation")
	}
	a.commit = realCommit
	if _, err := a.Current(context.Background(), SubjectSession, "a1"); err != nil {
		t.Fatalf("committed staged credential was not recoverable: %v", err)
	}
	current, err := LoadCredential(dir)
	if err != nil || current.Generation != old.Generation+1 {
		t.Fatalf("recovered credential = %+v, err=%v", current, err)
	}

	principal := Principal{KeyID: current.KeyID, SubjectID: current.SubjectID, Kind: current.Kind, Generation: current.Generation}
	a.commit = func(path string, value any, mode os.FileMode) (bool, error) {
		committed, err := realCommit(path, value, mode)
		if err != nil {
			return committed, err
		}
		return true, errors.New("injected post-rename sync failure")
	}
	if err := a.Revoke(context.Background(), SubjectSession, "a1"); err == nil {
		t.Fatal("uncertain revoke unexpectedly returned success")
	}
	if err := a.Valid(context.Background(), principal); err == nil {
		t.Fatal("committed revocation was not adopted in memory")
	}
}

func TestAuthorityRootHasSingleOwner(t *testing.T) {
	root := t.TempDir()
	a, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); !errors.Is(err, ErrAuthorityInUse) {
		t.Fatalf("second authority error = %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := Open(root)
	if err != nil {
		t.Fatalf("authority did not reopen after release: %v", err)
	}
	_ = b.Close()
}
