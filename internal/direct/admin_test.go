package direct

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/access"
)

// hostAuthority stands in for the daemon: a real access.FileAuthority whose
// host listener admits only SubjectHost proofs, exactly like daemon.authenticateHost.
type hostAuthority struct {
	fa      *access.FileAuthority
	host    access.Credential
	session access.Credential
}

func newHostAuthority(t *testing.T) *hostAuthority {
	t.Helper()
	fa, err := access.Open(filepath.Join(t.TempDir(), "access"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fa.Close() })
	ctx := context.Background()
	if _, err := fa.EnsureHost(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := fa.Ensure(ctx, access.SubjectSession, "agent-a"); err != nil {
		t.Fatal(err)
	}
	host, err := access.LoadCredential(fa.CredentialDir(access.SubjectHost, access.LocalHostSubject))
	if err != nil {
		t.Fatal(err)
	}
	session, err := access.LoadCredential(fa.CredentialDir(access.SubjectSession, "agent-a"))
	if err != nil {
		t.Fatal(err)
	}
	return &hostAuthority{fa: fa, host: host, session: session}
}

func (h *hostAuthority) verify(ctx context.Context, prove ProveFunc) error {
	ch, err := h.fa.NewChallenge()
	if err != nil {
		return err
	}
	proof, err := prove(ch)
	if err != nil {
		return err
	}
	_, err = h.fa.VerifyProof(ctx, ch, proof, access.SubjectHost)
	return err
}

func startAdmin(t *testing.T, root string, h *hostAuthority) *Authority {
	t.Helper()
	a := openTestAuthority(t, root, Lifetimes{}, nil)
	srv := &AdminServer{Authority: a, Endpoints: []string{testEndpoint}, Verify: h.verify}
	if err := srv.Listen(root); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Serve(ctx)
	return a
}

func shortRoot(t *testing.T) string {
	// Unix socket paths are short on macOS; keep the root near /tmp.
	dir, err := os.MkdirTemp("", "dx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "d")
}

func TestAdminEnrollListRevokeWithHostProof(t *testing.T) {
	h := newHostAuthority(t)
	root := shortRoot(t)
	a := startAdmin(t, root, h)
	ctx := context.Background()

	st, err := os.Stat(AdminSocketPath(root))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("admin socket mode: %v %v", st.Mode(), err)
	}
	resp, err := AdminCall(ctx, root, h.host, AdminRequest{Op: OpEnroll, Label: "Pixel", Permissions: []string{PermRead},
		Resource: ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := DecodeBundle(resp.Bundle, time.Now())
	if err != nil || b.ServerID != a.ServerID() || b.Endpoints[0] != testEndpoint {
		t.Fatalf("bundle: %v %+v", err, b)
	}
	tok, err := a.RedeemCode(b.Code, "Pixel", ClientAndroid)
	if err != nil {
		t.Fatal(err)
	}
	list, err := AdminCall(ctx, root, h.host, AdminRequest{Op: OpList})
	if err != nil || len(list.Clients) != 1 || list.Clients[0].ClientID != tok.ClientID {
		t.Fatalf("list: %v %+v", err, list)
	}
	if _, err := AdminCall(ctx, root, h.host, AdminRequest{Op: OpRevoke, ClientID: tok.ClientID}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Refresh(tok.RefreshToken); err == nil {
		t.Fatal("revoked client refreshed")
	}
}

func TestAdminRefusesNonHostCredentialsWithoutWriting(t *testing.T) {
	h := newHostAuthority(t)
	root := shortRoot(t)
	a := startAdmin(t, root, h)
	tok := enroll(t, a, []string{PermRead}, allSessions)
	before, err := os.ReadFile(filepath.Join(root, grantsFile))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// A session credential relabelled as host: the client-side kind check is
	// bypassed, and the daemon's host-only verification must refuse it.
	forged := h.session
	forged.Kind = access.SubjectHost
	for _, req := range []AdminRequest{
		{Op: OpEnroll, Label: "x", Permissions: []string{PermRead}, Resource: allSessions},
		{Op: OpRevoke, ClientID: tok.ClientID},
		{Op: OpResetIdentity},
		{Op: OpList},
	} {
		if _, err := AdminCall(ctx, root, forged, req); err == nil {
			t.Errorf("%s with a session credential succeeded", req.Op)
		}
		if _, err := AdminCall(ctx, root, h.session, req); err == nil {
			t.Errorf("%s with an unmodified session credential succeeded", req.Op)
		}
	}
	after, _ := os.ReadFile(filepath.Join(root, grantsFile))
	if !bytes.Equal(before, after) {
		t.Fatal("a refused admin caller changed the registry")
	}
	if _, err := a.Admit(ctx, tok.AccessToken, PermRead); err != nil {
		t.Fatalf("existing client harmed by refused admin calls: %v", err)
	}
}

func TestOfflineAdminRequiresHostAndExclusiveStore(t *testing.T) {
	h := newHostAuthority(t)
	root := shortRoot(t)
	a, err := OpenAuthority(root, Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok := enroll(t, a, []string{PermRead}, allSessions)
	ctx := context.Background()
	if _, err := OfflineAdminister(ctx, root, h.host, h.verify, AdminRequest{Op: OpList}); err == nil {
		t.Fatal("offline admin wrote beside a running owner")
	}
	a.Close()
	before, _ := os.ReadFile(filepath.Join(root, grantsFile))
	forged := h.session
	forged.Kind = access.SubjectHost
	if _, err := OfflineAdminister(ctx, root, forged, h.verify, AdminRequest{Op: OpRevoke, ClientID: tok.ClientID}); err == nil {
		t.Fatal("offline revoke with a session credential succeeded")
	}
	after, _ := os.ReadFile(filepath.Join(root, grantsFile))
	if !bytes.Equal(before, after) {
		t.Fatal("refused offline admin changed the registry")
	}
	if _, err := OfflineAdminister(ctx, root, h.host, h.verify, AdminRequest{Op: OpEnroll}); err == nil {
		t.Fatal("offline enrollment without endpoints succeeded")
	}
	if _, err := OfflineAdminister(ctx, root, h.host, h.verify, AdminRequest{Op: OpRevoke, ClientID: tok.ClientID}); err != nil {
		t.Fatal(err)
	}
	b := openTestAuthority(t, root, Lifetimes{}, nil)
	if _, err := b.Refresh(tok.RefreshToken); err == nil {
		t.Fatal("offline revoke not durable")
	}
}

func TestBundleRejectsMalformedInput(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	good := Bundle{V: 1, ServerID: "id", Endpoints: []string{testEndpoint}, Code: nonce(), ExpiresAt: now.Unix() + 60}
	for name, mut := range map[string]func(*Bundle){
		"version":  func(b *Bundle) { b.V = 2 },
		"expired":  func(b *Bundle) { b.ExpiresAt = now.Unix() },
		"code":     func(b *Bundle) { b.Code = "short" },
		"endpoint": func(b *Bundle) { b.Endpoints = []string{"https://x.test/path"} },
		"http":     func(b *Bundle) { b.Endpoints = []string{"http://x.test"} },
	} {
		b := good
		mut(&b)
		s, _ := EncodeBundle(b)
		if _, err := DecodeBundle(s, now); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	s, _ := EncodeBundle(good)
	if _, err := DecodeBundle(s, now); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineAdminNeverCreatesAStore(t *testing.T) {
	h := newHostAuthority(t)
	root := shortRoot(t)
	for _, op := range []string{OpList, OpRevoke, OpResetIdentity} {
		if _, err := OfflineAdminister(context.Background(), root, h.host, h.verify, AdminRequest{Op: op, ClientID: "c_x"}); err != ErrNoStore {
			t.Errorf("%s on a missing store: %v", op, err)
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("offline admin created the store")
	}
}
