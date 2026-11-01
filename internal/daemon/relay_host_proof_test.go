package daemon_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"testing"

	"amux/internal/access"
	"amux/internal/daemon"
	"amux/internal/direct"
)

// TestDirectAdminThroughRealHostListener covers the production admin proof
// path end to end: the direct admin socket relays the real daemon host
// listener's challenge (daemon.RelayHostProof), the caller signs it with its
// credential, and the daemon decides. A host credential is admitted; a session
// credential is refused with no registry write.
func TestDirectAdminThroughRealHostListener(t *testing.T) {
	tmp, err := os.MkdirTemp("", "rhp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	t.Setenv("HOME", tmp)
	sock := filepath.Join(tmp, "d.sock")
	t.Setenv("AMUX_SOCK", sock)

	authority, err := access.Open(access.AuthorityRoot())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { authority.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hostDir, err := authority.EnsureHost(ctx)
	if err != nil {
		t.Fatal(err)
	}
	host, _ := access.LoadCredential(hostDir)
	sessionDir, _ := authority.Ensure(ctx, access.SubjectSession, "agent-a")
	session, _ := access.LoadCredential(sessionDir)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go daemon.ServeHostForTest(ctx, authority, ln)

	// The relay alone: host admitted, session refused.
	if err := daemon.RelayHostProof(ctx, func(ch access.SocketChallenge) (access.SocketProof, error) {
		return access.SignProof(host, ch, rand.Reader)
	}); err != nil {
		t.Fatalf("host proof: %v", err)
	}
	if err := daemon.RelayHostProof(ctx, func(ch access.SocketChallenge) (access.SocketProof, error) {
		return access.SignProof(session, ch, rand.Reader)
	}); err == nil {
		t.Fatal("session proof admitted by the host listener")
	}

	// Through the direct admin channel, exactly as `amux direct` runs it.
	root := filepath.Join(tmp, "direct")
	a, err := direct.OpenAuthority(root, direct.Lifetimes{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	srv := &direct.AdminServer{Authority: a, Endpoints: []string{"https://amux.test:8443"}, Verify: func(ctx context.Context, prove direct.ProveFunc) error { return daemon.RelayHostProof(ctx, prove) }}
	if err := srv.Listen(root); err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ctx)
	resp, err := direct.AdminCall(ctx, root, host, direct.AdminRequest{Op: direct.OpEnroll, Label: "Pixel",
		Permissions: []string{direct.PermRead}, Resource: direct.ResourceScope{Kind: direct.ResourceAllSessions}})
	if err != nil || resp.Bundle == "" {
		t.Fatalf("host enroll: %v %+v", err, resp)
	}
	before, err := os.ReadFile(filepath.Join(root, "grants.json"))
	if err != nil {
		t.Fatal(err)
	}
	forged := session
	forged.Kind = access.SubjectHost // bypass the client-side kind check
	for _, op := range []string{direct.OpEnroll, direct.OpList, direct.OpRevoke, direct.OpResetIdentity} {
		if _, err := direct.AdminCall(ctx, root, forged, direct.AdminRequest{Op: op, Label: "x",
			Permissions: []string{direct.PermRead}, Resource: direct.ResourceScope{Kind: direct.ResourceAllSessions}, ClientID: "c_x"}); err == nil {
			t.Errorf("%s with a session credential succeeded", op)
		}
	}
	after, _ := os.ReadFile(filepath.Join(root, "grants.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("refused admin caller changed the registry")
	}
}
