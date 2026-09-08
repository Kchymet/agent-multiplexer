package mux

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/muxclient"
	"amux/internal/muxproto"
	"amux/internal/vterm"
)

func TestHelloIsRequiredBeforePrimaryAccess(t *testing.T) {
	var snapshots int
	srv := New(testPrimary{snapshot: func(context.Context) ([]core.Session, error) {
		snapshots++
		return []core.Session{{ID: "secret"}}, nil
	}})
	srv.token = "required"
	server, peer := net.Pipe()
	defer peer.Close()
	go srv.handleClient(server)
	c := muxproto.NewConn(peer)
	if err := c.WriteClient(muxproto.ClientMsg{Type: muxproto.CSubscribe}); err != nil {
		t.Fatal(err)
	}
	m, err := c.ReadServer()
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != muxproto.SWelcome || m.OK || m.Error != muxproto.ErrUnauthorized {
		t.Fatalf("pre-hello response = %+v", m)
	}
	if snapshots != 0 {
		t.Fatalf("pre-hello subscribe reached primary %d time(s)", snapshots)
	}
}

func TestHelloRequiresFreshPrimaryAuthentication(t *testing.T) {
	called := make(chan struct{}, 1)
	srv := New(testPrimary{snapshot: func(context.Context) ([]core.Session, error) {
		called <- struct{}{}
		return nil, errors.New("primary unavailable")
	}})
	srv.token = "required"
	server, peer := net.Pipe()
	defer peer.Close()
	go srv.handleClient(server)
	c := muxproto.NewConn(peer)
	if err := c.WriteClient(muxproto.ClientMsg{Type: muxproto.CHello, Version: muxproto.Version, Token: "required"}); err != nil {
		t.Fatal(err)
	}
	m, err := c.ReadServer()
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != muxproto.SWelcome || m.OK || m.Error != muxproto.ErrUnauthorized {
		t.Fatalf("primary-down hello response = %+v", m)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("hello did not revalidate primary authority")
	}
}

func TestAuthenticatedMuxRelaysOnlyAfterOneHello(t *testing.T) {
	dispatched := make(chan core.Action, 1)
	srv := New(testPrimary{
		snapshot: func(context.Context) ([]core.Session, error) {
			return []core.Session{{ID: "a1"}}, nil
		},
		dispatch: func(_ context.Context, a core.Action) (string, error) {
			dispatched <- a
			return "created", nil
		},
	})
	srv.token = "required"
	server, peer := net.Pipe()
	defer peer.Close()
	go srv.handleClient(server)
	c := muxproto.NewConn(peer)
	if err := c.WriteClient(muxproto.ClientMsg{Type: muxproto.CHello, Version: muxproto.Version, Token: "required"}); err != nil {
		t.Fatal(err)
	}
	if welcome, err := c.ReadServer(); err != nil || !welcome.OK {
		t.Fatalf("welcome = %+v, %v", welcome, err)
	}
	if err := c.WriteClient(muxproto.ClientMsg{Type: muxproto.CAction, Action: core.ActionRefresh}); err != nil {
		t.Fatal(err)
	}
	if result, err := c.ReadServer(); err != nil || !result.OK || result.NewID != "created" {
		t.Fatalf("result = %+v, %v", result, err)
	}
	select {
	case got := <-dispatched:
		if got.Action != core.ActionRefresh {
			t.Fatalf("dispatched %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("action did not reach primary")
	}
	if err := c.WriteClient(muxproto.ClientMsg{Type: muxproto.CHello, Version: muxproto.Version, Token: "required"}); err != nil {
		t.Fatal(err)
	}
	_ = peer.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.ReadServer(); err == nil {
		t.Fatal("repeated hello did not close the connection")
	}
}

func TestMuxActionRelayRejectsDaemonInternalVocabulary(t *testing.T) {
	dispatched := make(chan core.Action, 1)
	srv := New(testPrimary{dispatch: func(_ context.Context, action core.Action) (string, error) {
		dispatched <- action
		return "", nil
	}})
	cl := &client{out: make(chan muxproto.ServerMsg, 4), done: make(chan struct{}), panes: map[string]string{}, obuf: map[string]*paneOut{}, wake: make(chan struct{}, 1)}
	srv.clients[cl] = true

	for _, action := range []string{"", "daemon.shutdown", "session.recreate", core.ActionQuery, core.ActionPaneOpen} {
		if !srv.handleMsg(cl, muxproto.ClientMsg{Type: muxproto.CAction, Action: action, ID: "a1"}) {
			t.Fatalf("unsupported action %q closed an authenticated connection", action)
		}
		select {
		case got := <-dispatched:
			t.Fatalf("unsupported action %q reached primary as %+v", action, got)
		default:
		}
		select {
		case result := <-cl.out:
			if result.Type != muxproto.SResult || result.OK || result.Error != "unsupported action" {
				t.Fatalf("unsupported action %q result = %+v", action, result)
			}
		default:
			t.Fatalf("unsupported action %q produced no rejection", action)
		}
	}
}

func TestBlankMuxCredentialAndPlaintextListenersFailClosed(t *testing.T) {
	t.Setenv("AMUX_MUX_TOKEN", "")
	if err := RunWithPrimary(testPrimary{}); err == nil {
		t.Fatal("blank mux credential started a listener")
	}
	for _, spec := range []string{"tcp:127.0.0.1:1", "127.0.0.1:1"} {
		if ln, err := Listen(spec); err == nil {
			ln.Close()
			t.Fatalf("plaintext listener %q was accepted", spec)
		}
	}
	t.Setenv("AMUX_MUX_TOKEN", "configured")
	if err := RunWithPrimary(nil); err == nil || !strings.Contains(err.Error(), "authenticated primary daemon relay") {
		t.Fatalf("nil primary relay error = %v", err)
	}
	called := false
	err := RunWithPrimary(testPrimary{snapshot: func(context.Context) ([]core.Session, error) {
		called = true
		return nil, errors.New("primary auth failed")
	}})
	if !called || err == nil || !strings.Contains(err.Error(), "authenticate primary daemon relay") {
		t.Fatalf("primary preflight: called=%v err=%v", called, err)
	}
}

// genCert writes a self-signed cert/key for 127.0.0.1 into dir. The cert is its
// own CA, so a client that trusts it verifies the handshake.
func genCert(t *testing.T, dir string) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "amux-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// TestTLSListenSpec drives the "tls:" listen spec end-to-end through the real UI
// client: a client trusting the server's CA completes the hello/welcome
// handshake, one that doesn't is rejected at the TLS layer. Only the accept path
// is wired here (acceptLoop handles hello without the harness).
func TestTLSListenSpec(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := genCert(t, dir)
	t.Setenv("AMUX_TLS_CERT", certFile)
	t.Setenv("AMUX_TLS_KEY", keyFile)
	t.Setenv("AMUX_MUX_TOKEN", "test-token")

	ln, err := Listen("tls:127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := New(testPrimary{})
	go srv.acceptLoop(ln)
	addr := ln.Addr().String()

	t.Run("accept trusted", func(t *testing.T) {
		t.Setenv("AMUX_TLS_CA", certFile)
		c, err := muxclient.Dial("tls:"+addr, muxclient.Handlers{})
		if err != nil {
			t.Fatalf("trusted TLS client rejected: %v", err)
		}
		defer c.Close()
		if host, _ := os.Hostname(); c.Server() != host {
			t.Fatalf("welcome server identity = %q, want hostname %q", c.Server(), host)
		}
	})

	t.Run("reject untrusted", func(t *testing.T) {
		t.Setenv("AMUX_TLS_CA", "") // system roots only: self-signed cert is untrusted
		if c, err := muxclient.Dial("tls:"+addr, muxclient.Handlers{}); err == nil {
			_ = c.Close()
			t.Fatal("client accepted an untrusted server cert")
		}
	})
}

// TestClientAuth covers token auth on hello/welcome: a matching token is welcomed,
// a wrong or missing one is rejected loudly (Dial returns an error). The compare
// is constant-time in muxproto.TokenOK.
func TestClientAuth(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "auth.sock")
	certFile, keyFile := genCert(t, dir)
	t.Setenv("AMUX_TLS_CERT", certFile)
	t.Setenv("AMUX_TLS_KEY", keyFile)
	t.Setenv("AMUX_TLS_CA", certFile)
	t.Setenv("AMUX_TLS_SERVERNAME", "localhost")
	ln, err := Listen("unix:" + sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := New(testPrimary{})
	srv.token = "s3cr3t" // server requires this token
	go srv.acceptLoop(ln)

	t.Run("reject wrong token", func(t *testing.T) {
		t.Setenv("AMUX_MUX_TOKEN", "wrong")
		if c, err := muxclient.Dial("unix:"+sock, muxclient.Handlers{}); err == nil {
			_ = c.Close()
			t.Fatal("connection accepted with a wrong token")
		}
	})
	t.Run("reject missing token", func(t *testing.T) {
		t.Setenv("AMUX_MUX_TOKEN", "")
		if c, err := muxclient.Dial("unix:"+sock, muxclient.Handlers{}); err == nil {
			_ = c.Close()
			t.Fatal("connection accepted with no token")
		}
	})
	t.Run("accept correct token", func(t *testing.T) {
		t.Setenv("AMUX_MUX_TOKEN", "s3cr3t")
		c, err := muxclient.Dial("unix:"+sock, muxclient.Handlers{})
		if err != nil {
			t.Fatalf("correct token rejected: %v", err)
		}
		_ = c.Close()
	})
}

// newTestClient builds a client wired to conn for exercising the write pump
// directly (no server accept loop).
func newTestClient(conn *muxproto.Conn) *client {
	return &client{
		conn:  conn,
		out:   make(chan muxproto.ServerMsg, 256),
		done:  make(chan struct{}),
		panes: map[string]string{},
		obuf:  map[string]*paneOut{},
		wake:  make(chan struct{}, 1),
	}
}

// TestMuxPaneOutputLossless is the ghost-text regression for the mux stack. Pane
// output is a stateful terminal byte stream: dropping any of it corrupts the
// client's emulator. net.Pipe is synchronous, so the reader is as slow as it
// gets; the coalescing buffer must still deliver every byte in order with no
// resync for a sub-cap stream.
func TestMuxPaneOutputLossless(t *testing.T) {
	srv, cli := net.Pipe()
	cl := newTestClient(muxproto.NewConn(srv))
	go cl.writeLoop()
	defer cl.stop()
	rd := muxproto.NewConn(cli)
	defer rd.Close()

	const chunks = 4000
	var want bytes.Buffer
	for i := 0; i < chunks; i++ {
		want.WriteString(fmt.Sprintf("<%d>", i))
	}
	go func() {
		for i := 0; i < chunks; i++ {
			cl.paneOutput("p", []byte(fmt.Sprintf("<%d>", i)))
		}
	}()

	var got bytes.Buffer
	deadline := time.After(5 * time.Second)
	done := make(chan error, 1)
	go func() {
		for got.Len() < want.Len() {
			m, err := rd.ReadServer()
			if err != nil {
				done <- err
				return
			}
			switch m.Type {
			case muxproto.SPaneReset:
				done <- fmt.Errorf("unexpected resync for a %d-byte stream", want.Len())
				return
			case muxproto.SPaneOutput:
				got.Write(m.Data)
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-deadline:
		t.Fatalf("timed out after %d/%d bytes", got.Len(), want.Len())
	}
	if !bytes.Equal(got.Bytes(), want.Bytes()) {
		t.Fatalf("stream corrupted: got %d bytes, want %d", got.Len(), want.Len())
	}
}

// TestMuxPaneResyncEmulatorConsistent is the slow-client integrity check. A
// stalled reader lets the backlog blow past paneOutCap; rather than drop bytes
// from the middle (which would ghost), the buffer trims to the recent tail and
// flags a reset. A client that clears its emulator and applies that tail must
// render exactly what a client that saw the whole stream would — the retained
// tail carries the agent's most recent full repaint.
func TestMuxPaneResyncEmulatorConsistent(t *testing.T) {
	cl := &client{obuf: map[string]*paneOut{}, wake: make(chan struct{}, 1), done: make(chan struct{})}

	// A stream far larger than the cap, ending in a full-screen repaint: ESC c
	// (terminal reset) then the final content. The repaint lives in the last few
	// bytes, so it survives the trim.
	filler := bytes.Repeat([]byte("noise line that will be scrolled away\r\n"), (paneOutCap/32)+1)
	repaint := []byte("\x1bcFINAL SCREEN\r\nsecond line")
	full := append(append([]byte{}, filler...), repaint...)

	cl.paneOutput("p", full)
	b := cl.obuf["p"]
	if !b.reset {
		t.Fatal("oversized backlog did not trigger a resync")
	}
	if len(b.data) != paneOutKeep {
		t.Fatalf("kept %d bytes, want the paneOutKeep tail (%d)", len(b.data), paneOutKeep)
	}
	if !bytes.HasSuffix(b.data, repaint) {
		t.Fatal("retained tail must end in the most recent output (the repaint)")
	}

	// Reference emulator: fed the entire stream.
	ref := vterm.New(80, 24)
	ref.Feed(full)
	// Resync emulator: clears on reset, then applies only the retained tail.
	resync := vterm.New(80, 24)
	resync.Reset()
	resync.Feed(b.data)

	if ref.Render() != resync.Render() {
		t.Fatalf("emulator diverged after resync:\n--- full ---\n%s\n--- resync ---\n%s", ref.Render(), resync.Render())
	}
}
