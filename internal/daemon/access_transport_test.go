package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/access"
)

func TestServeSendsNoProtectedFrameBeforeClientProof(t *testing.T) {
	d := New("", nil, time.Hour)
	var err error
	d.authority, err = access.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.authority.Close() })
	dir, _ := d.authority.EnsureHost(context.Background())
	credential, _ := access.LoadCredential(dir)

	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.serve(ctx, server); close(done) }()
	config, _ := access.ClientTLSConfig(credential)
	secure := tls.Client(client, config)
	if err := secure.Handshake(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(secure)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var challenge access.SocketChallenge
	if err := json.Unmarshal(line, &challenge); err != nil || challenge.Type != access.FrameChallenge {
		t.Fatalf("first application frame = %q, err=%v", line, err)
	}
	_ = secure.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if line, err := reader.ReadBytes('\n'); err == nil {
		t.Fatalf("protected pre-auth frame = %s", line)
	} else if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("pre-auth read error = %v", err)
	}
	_ = secure.Close()
	<-done
}

func TestHostListenerRejectsSessionPrincipalWithoutSnapshot(t *testing.T) {
	d := New("", nil, time.Hour)
	var err error
	d.authority, err = access.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.authority.Close() })
	dir, _ := d.authority.Ensure(context.Background(), access.SubjectSession, "session-a")
	credential, _ := access.LoadCredential(dir)

	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.serve(ctx, server); close(done) }()
	config, _ := access.ClientTLSConfig(credential)
	secure := tls.Client(client, config)
	if err := secure.Handshake(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(secure)
	line, _ := reader.ReadBytes('\n')
	var challenge access.SocketChallenge
	if err := json.Unmarshal(line, &challenge); err != nil {
		t.Fatal(err)
	}
	proof, _ := access.SignProof(credential, challenge, rand.Reader)
	if err := json.NewEncoder(secure).Encode(proof); err != nil {
		t.Fatal(err)
	}
	line, err = reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var welcome access.SocketWelcome
	if err := json.Unmarshal(line, &welcome); err != nil || welcome.OK {
		t.Fatalf("session welcome = %+v, err=%v", welcome, err)
	}
	if strings.Contains(string(line), "snapshot") {
		t.Fatalf("denial leaked snapshot: %s", line)
	}
	_ = secure.Close()
	<-done
}

func TestOwnedSocketNeverRemovesUnmanagedEndpoint(t *testing.T) {
	root := t.TempDir()
	stable := filepath.Join(root, "daemon.sock")
	legacy, err := net.Listen("unix", stable)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, _, err := listenOwnedUnix(stable, strings.Repeat("a", 64)); err == nil {
		t.Fatal("unmanaged listener was replaced")
	}
	if info, err := os.Lstat(stable); err != nil || info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("unmanaged endpoint changed: info=%v err=%v", info, err)
	}
}

func TestOwnedSocketRotatesOnlyManagedSymlink(t *testing.T) {
	stable := filepath.Join(t.TempDir(), "daemon.sock")
	first, cleanupFirst, err := listenOwnedUnix(stable, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(stable); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("stable endpoint is not a symlink: info=%v err=%v", info, err)
	}
	cleanupFirst()
	second, cleanupSecond, err := listenOwnedUnix(stable, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSecond()
	if first == second {
		t.Fatal("listener was unexpectedly reused")
	}
	conn, err := net.Dial("unix", stable)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
}

func TestCommittedRevokeAbortsBlockedAndQueuedFrames(t *testing.T) {
	a, err := access.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	dir, err := a.EnsureHost(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	credential, err := access.LoadCredential(dir)
	if err != nil {
		t.Fatal(err)
	}
	principal := access.Principal{
		KeyID: credential.KeyID, SubjectID: credential.SubjectID,
		Kind: credential.Kind, Generation: credential.Generation,
	}
	invalidated, cancelWatch, err := a.WatchInvalidation(principal)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelWatch()
	server, client := net.Pipe()
	defer client.Close()
	var once sync.Once
	entered := make(chan struct{})
	cl := newAuthenticatedConnState(server, func() bool {
		once.Do(func() { close(entered) })
		return a.Valid(context.Background(), principal) == nil
	})
	defer cl.shutdown()
	go func() {
		<-invalidated
		_ = server.Close()
	}()
	cl.send(map[string]string{"type": "protected-first"})
	cl.send(map[string]string{"type": "protected-queued"})
	<-entered // writer passed validity and is blocked because client has not read
	if err := a.Revoke(context.Background(), access.SubjectHost, access.LocalHostSubject); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	// The first frame crossed the write barrier before revocation and may be
	// partial or complete; transport bytes cannot be recalled. The second frame
	// was only queued and must never begin writing after revocation commits.
	got, _ := io.ReadAll(client)
	if bytes.Contains(got, []byte("protected-queued")) {
		t.Fatalf("queued protected frame drained after committed revoke: %q", got)
	}
}
