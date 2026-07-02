package provider

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/harnessproto"
	"amux/internal/wiretls"
)

// newFast builds a provider tuned for tests: tiny backoff, and heartbeat scaled
// so far out that no ping fires during a test (the unbuffered net.Pipe would
// otherwise wedge on an undrained ping). Grace defaults small; callers override.
func newFast(cfg Config) *Provider {
	p := New(cfg)
	p.backoffMin = time.Millisecond
	p.backoffMax = 5 * time.Millisecond
	p.hbScale = time.Hour
	p.graceScale = 20 * time.Millisecond
	return p
}

// pipeDialer hands each dial a fresh net.Pipe, publishing the orchestrator end on
// conns so the test can drive that connection.
func pipeDialer(conns chan net.Conn) func(context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		select {
		case conns <- b:
			return a, nil
		case <-ctx.Done():
			a.Close()
			b.Close()
			return nil, ctx.Err()
		}
	}
}

func readFrame(t *testing.T, oc *harnessproto.Conn) harnessproto.HarnessMsg {
	t.Helper()
	m, err := oc.ReadHarness()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return m
}

// expectRegister reads and sanity-checks the register frame.
func expectRegister(t *testing.T, oc *harnessproto.Conn) harnessproto.HarnessMsg {
	t.Helper()
	m := readFrame(t, oc)
	if m.Type != harnessproto.HRegister {
		t.Fatalf("first frame = %q, want register", m.Type)
	}
	return m
}

// accept completes a successful registration with the given directives.
func accept(t *testing.T, oc *harnessproto.Conn, version int, adopt []harnessproto.AdoptPane, grace int) {
	t.Helper()
	reg := expectRegister(t, oc)
	v, ok := harnessproto.Negotiate(reg.Versions, []int{1, 2})
	if !ok {
		t.Fatalf("no common version with %v", reg.Versions)
	}
	if version == 0 {
		version = v
	}
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRegistered, OK: true, Version: version, ProviderID: "prov-test",
		HeartbeatSeconds: 1, GraceSeconds: grace, Adopt: adopt,
	}); err != nil {
		t.Fatalf("send registered: %v", err)
	}
}

// TestRoundTrip runs a full handshake then a spawn → output → exit exchange over
// an in-process fake orchestrator (net.Pipe).
func TestRoundTrip(t *testing.T) {
	conns := make(chan net.Conn, 1)
	p := newFast(Config{Orchestrator: "pipe", Token: "s3cr3t", Name: "box", Dial: pipeDialer(conns)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx) }()

	oc := harnessproto.NewConn(<-conns)
	reg := expectRegister(t, oc)
	if reg.Token != "s3cr3t" || reg.Name != "box" {
		t.Fatalf("register identity = %q/%q", reg.Token, reg.Name)
	}
	if reg.Capabilities == nil || reg.Capabilities.OS == "" {
		t.Fatalf("register missing capabilities: %+v", reg.Capabilities)
	}
	if len(reg.Panes) != 0 {
		t.Fatalf("cold start offered panes: %v", reg.Panes)
	}
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MRegistered, OK: true, Version: 2, HeartbeatSeconds: 1, GraceSeconds: 60,
	}); err != nil {
		t.Fatal(err)
	}

	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSpawn, PaneID: "p1", Argv: []string{"sh", "-c", "printf hi; exit 3"}, Cols: 80, Rows: 24,
	}); err != nil {
		t.Fatal(err)
	}
	data, exitErr, seqs := readPaneUntilExit(t, oc, "p1")
	if !strings.Contains(string(data), "hi") {
		t.Fatalf("output = %q, want to contain %q", data, "hi")
	}
	if !strings.Contains(exitErr, "exit status 3") {
		t.Fatalf("exit error = %q, want exit status 3", exitErr)
	}
	assertContiguousFrom1(t, seqs)

	cancel()
	<-runErr
}

// TestReconnectAdoptReplay proves a pane survives a disconnect and its output
// replays byte-exact from the orchestrator's afterSeq on adopt.
func TestReconnectAdoptReplay(t *testing.T) {
	conns := make(chan net.Conn, 1)
	p := newFast(Config{Orchestrator: "pipe", Dial: pipeDialer(conns)})
	p.graceScale = time.Second // keep the pane alive across the reconnect
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx) }()

	// Connection 1: register, spawn a pane that emits a known stream, read the
	// first output frame, then drop the connection.
	oc1 := harnessproto.NewConn(<-conns)
	accept(t, oc1, 2, nil, 60)
	if err := oc1.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSpawn, PaneID: "p1",
		Argv: []string{"sh", "-c", "printf AAA; sleep 0.05; printf BBB; sleep 0.05; printf CCC; sleep 5"},
	}); err != nil {
		t.Fatal(err)
	}
	var got []byte
	var afterSeq int64
	first := readFrame(t, oc1)
	if first.Type != harnessproto.HOutput {
		t.Fatalf("first pane frame = %q, want output", first.Type)
	}
	got = append(got, first.Data...)
	afterSeq = first.Seq
	oc1.Close() // simulate a network drop; the pane keeps running under grace

	// Connection 2: the pane is offered for resume; adopt it from afterSeq and
	// collect the replayed remainder.
	oc2 := harnessproto.NewConn(<-conns)
	reg := expectRegister(t, oc2)
	if len(reg.Panes) != 1 || reg.Panes[0].PaneID != "p1" || !reg.Panes[0].Running {
		t.Fatalf("resume offer = %+v, want one running p1", reg.Panes)
	}
	accept2 := harnessproto.MuxMsg{
		Type: harnessproto.MRegistered, OK: true, Version: 2, HeartbeatSeconds: 1, GraceSeconds: 60,
		Adopt: []harnessproto.AdoptPane{{PaneID: "p1", AfterSeq: afterSeq}},
	}
	// accept2 replaces the register frame we already consumed above.
	if err := oc2.WriteMux(accept2); err != nil {
		t.Fatal(err)
	}

	// Read replayed frames until we've reconstructed the full stream.
	deadline := time.Now().Add(3 * time.Second)
	nextSeq := afterSeq
	for !strings.Contains(string(got), "AAABBBCCC") && time.Now().Before(deadline) {
		m := readFrame(t, oc2)
		if m.Type != harnessproto.HOutput || m.PaneID != "p1" {
			continue
		}
		if m.Seq != nextSeq+1 {
			t.Fatalf("replay gap: got seq %d after %d", m.Seq, nextSeq)
		}
		nextSeq = m.Seq
		got = append(got, m.Data...)
	}
	if string(got) != "AAABBBCCC" {
		t.Fatalf("reconstructed stream = %q, want AAABBBCCC", got)
	}

	cancel()
	<-runErr
}

// TestGraceExpiryKills proves panes are terminated when the grace window elapses
// without a reconnect: the next registration offers none.
func TestGraceExpiryKills(t *testing.T) {
	conns := make(chan net.Conn, 1)
	p := newFast(Config{Orchestrator: "pipe", Dial: pipeDialer(conns)})
	// A short grace (5ms) with a much longer reconnect backoff (>=40ms) guarantees
	// the pane is killed before the next registration snapshots its offer.
	p.graceScale = 5 * time.Millisecond
	p.backoffMin = 80 * time.Millisecond
	p.backoffMax = 80 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx) }()

	oc1 := harnessproto.NewConn(<-conns)
	accept(t, oc1, 2, nil, 1) // graceSeconds=1 → 5ms
	if err := oc1.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSpawn, PaneID: "p1", Argv: []string{"sh", "-c", "printf x; sleep 30"},
	}); err != nil {
		t.Fatal(err)
	}
	if m := readFrame(t, oc1); m.Type != harnessproto.HOutput { // pane is up and running
		t.Fatalf("want output, got %q", m.Type)
	}
	oc1.Close() // grace (5ms) expires long before the reconnect (>=40ms) redials

	oc2 := harnessproto.NewConn(<-conns)
	reg := expectRegister(t, oc2)
	if len(reg.Panes) != 0 {
		t.Fatalf("after grace expiry, offered panes = %+v, want none", reg.Panes)
	}
	cancel()
	<-runErr
}

// TestBadTokenExits proves a bad-token rejection exits loudly without retrying.
func TestBadTokenExits(t *testing.T) {
	conns := make(chan net.Conn, 4)
	p := newFast(Config{Orchestrator: "pipe", Token: "wrong", Dial: pipeDialer(conns)})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx) }()

	oc := harnessproto.NewConn(<-conns)
	expectRegister(t, oc)
	if err := oc.WriteMux(harnessproto.MuxMsg{Type: harnessproto.MRegistered, OK: false, Error: harnessproto.ErrBadToken}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runErr:
		if err == nil || !strings.Contains(err.Error(), harnessproto.ErrBadToken) {
			t.Fatalf("Run returned %v, want a bad-token terminal error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on bad token (it retried?)")
	}
	// No reconnect must have been attempted.
	select {
	case <-conns:
		t.Fatal("provider redialed after a terminal token error")
	default:
	}
}

// TestVersionMismatchExits proves that no common protocol version exits loudly.
func TestVersionMismatchExits(t *testing.T) {
	conns := make(chan net.Conn, 4)
	p := newFast(Config{Orchestrator: "pipe", Dial: pipeDialer(conns)})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx) }()

	oc := harnessproto.NewConn(<-conns)
	reg := expectRegister(t, oc)
	if _, ok := harnessproto.Negotiate(reg.Versions, []int{3}); ok {
		t.Fatal("unexpected version overlap")
	}
	if err := oc.WriteMux(harnessproto.MuxMsg{Type: harnessproto.MRegistered, OK: false, Error: harnessproto.ErrBadVersion}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-runErr:
		if err == nil || !strings.Contains(err.Error(), harnessproto.ErrBadVersion) {
			t.Fatalf("Run returned %v, want an unsupported-version terminal error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on version mismatch")
	}
}

// TestRoundTripTLS runs the handshake and a spawn/output/exit over a real TLS
// connection on localhost, exercising the default (non-injected) dialer.
func TestRoundTripTLS(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := genCert(t, dir)
	srvCfg, err := wiretls.ServerConfig(certFile, keyFile, "")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srvCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()

	p := New(Config{Orchestrator: "tls://" + ln.Addr().String(), CAFile: certFile})
	p.hbScale = time.Hour
	p.backoffMin = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- p.Run(ctx) }()

	oc := harnessproto.NewConn(<-accepted)
	accept(t, oc, 2, nil, 60)
	if err := oc.WriteMux(harnessproto.MuxMsg{
		Type: harnessproto.MSpawn, PaneID: "p1", Argv: []string{"sh", "-c", "printf hi"},
	}); err != nil {
		t.Fatal(err)
	}
	data, _, seqs := readPaneUntilExit(t, oc, "p1")
	if !strings.Contains(string(data), "hi") {
		t.Fatalf("output = %q", data)
	}
	assertContiguousFrom1(t, seqs)
	cancel()
	<-runErr
}

// ---- helpers ----

func readPaneUntilExit(t *testing.T, oc *harnessproto.Conn, paneID string) (data []byte, exitErr string, seqs []int64) {
	t.Helper()
	for {
		m := readFrame(t, oc)
		if m.PaneID != paneID {
			continue
		}
		switch m.Type {
		case harnessproto.HOutput:
			data = append(data, m.Data...)
			seqs = append(seqs, m.Seq)
		case harnessproto.HExit:
			seqs = append(seqs, m.Seq)
			return data, m.Error, seqs
		}
	}
}

func assertContiguousFrom1(t *testing.T, seqs []int64) {
	t.Helper()
	if len(seqs) == 0 || seqs[0] != 1 {
		t.Fatalf("seqs = %v, want to start at 1", seqs)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("seqs not contiguous: %v", seqs)
		}
	}
}

// genCert writes a self-signed cert/key valid for 127.0.0.1 + localhost, doubling
// as its own CA (mirrors internal/wiretls' test helper).
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
