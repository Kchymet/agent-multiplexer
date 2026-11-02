package tailnet_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/tailnet"
	"amux/internal/tailnet/tailnettest"
)

const host = "monstrosity-40.example-tailnet.ts.net"

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func problemKind(t *testing.T, err error) string {
	t.Helper()
	var p *tailnet.Problem
	if !errors.As(err, &p) {
		t.Fatalf("error %v (%T) is not a Problem", err, err)
	}
	return p.Kind
}

func TestDiscoverDerivesAddressAndEndpoint(t *testing.T) {
	f := tailnettest.Start(t, host+".", "fd7a:115c:a1e0::1", "100.72.81.114")
	n, err := tailnet.Discover(ctx(t), &tailnet.Client{Socket: f.Socket})
	if err != nil {
		t.Fatal(err)
	}
	if n.DNSName != host {
		t.Fatalf("DNSName %q: trailing dot not trimmed", n.DNSName)
	}
	if n.IP.String() != "100.72.81.114" {
		t.Fatalf("IP %s: want the IPv4 address first", n.IP)
	}
	if got := n.Listen(tailnet.DefaultPort); got != "100.72.81.114:8443" {
		t.Fatalf("listen %s", got)
	}
	if got := n.Endpoint(tailnet.DefaultPort); got != "https://"+host+":8443" {
		t.Fatalf("endpoint %s", got)
	}
	if got := n.Endpoint(9443); got != "https://"+host+":9443" {
		t.Fatalf("port override endpoint %s", got)
	}
	if got := n.Endpoint(443); got != "https://"+host {
		t.Fatalf("443 endpoint %s, want the default port omitted", got)
	}
	if !n.Hosted() || !n.CanIssue() {
		t.Fatalf("hosted %v canIssue %v", n.Hosted(), n.CanIssue())
	}
	for _, r := range f.Requests() {
		if r.Host != tailnet.LocalAPIHost || r.Origin != "" || r.Referer != "" {
			t.Fatalf("request %+v does not match LocalAPI's required shape", r)
		}
	}
}

func TestDiscoverIPv6Only(t *testing.T) {
	f := tailnettest.Start(t, host+".", "fd7a:115c:a1e0::1")
	n, err := tailnet.Discover(ctx(t), &tailnet.Client{Socket: f.Socket})
	if err != nil {
		t.Fatal(err)
	}
	if got := n.Listen(8443); got != "[fd7a:115c:a1e0::1]:8443" {
		t.Fatalf("listen %s", got)
	}
}

func TestDiscoverProblems(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		_, err := tailnet.Discover(ctx(t), &tailnet.Client{Socket: filepath.Join(t.TempDir(), "absent.sock")})
		if k := problemKind(t, err); k != tailnet.ProblemUnreachable {
			t.Fatalf("kind %s: %v", k, err)
		}
		if !errors.Is(err, tailnet.ErrUnreachable) {
			t.Fatal("not ErrUnreachable")
		}
	})
	for _, tc := range []struct {
		state, kind, fix string
	}{
		{"NeedsLogin", tailnet.ProblemLoggedOut, "tailscale up"},
		{"NeedsMachineAuth", tailnet.ProblemLoggedOut, "approve"},
		{"Stopped", tailnet.ProblemNotRunning, "tailscale up"},
		{"Starting", tailnet.ProblemNotRunning, "wait"},
	} {
		t.Run(tc.state, func(t *testing.T) {
			f := tailnettest.Start(t, host+".", "100.72.81.114")
			f.Set("BackendState", tc.state)
			_, err := tailnet.Discover(ctx(t), &tailnet.Client{Socket: f.Socket})
			if k := problemKind(t, err); k != tc.kind || !strings.Contains(err.Error(), tc.fix) {
				t.Fatalf("kind %s err %v", k, err)
			}
		})
	}
	t.Run("status denied", func(t *testing.T) {
		f := tailnettest.Start(t, host+".", "100.72.81.114")
		f.FailStatus(403, "status access denied")
		_, err := tailnet.Discover(ctx(t), &tailnet.Client{Socket: f.Socket})
		if k := problemKind(t, err); k != tailnet.ProblemSocketAccess || !strings.Contains(err.Error(), "--operator=$USER") {
			t.Fatalf("kind %s err %v", k, err)
		}
	})
	t.Run("magicdns off hosted", func(t *testing.T) {
		f := tailnettest.Start(t, host+".", "100.72.81.114")
		f.SetMagicDNS(false)
		_, err := tailnet.Discover(ctx(t), &tailnet.Client{Socket: f.Socket})
		if k := problemKind(t, err); k != tailnet.ProblemMagicDNS || !strings.Contains(err.Error(), tailnet.AdminDNSURL) {
			t.Fatalf("kind %s err %v", k, err)
		}
	})
	t.Run("magicdns off custom control", func(t *testing.T) {
		f := tailnettest.Start(t, "", "100.72.81.114")
		f.SetControlURL("https://headscale.example.org")
		_, err := tailnet.Discover(ctx(t), &tailnet.Client{Socket: f.Socket})
		if k := problemKind(t, err); k != tailnet.ProblemMagicDNS {
			t.Fatalf("kind %s err %v", k, err)
		}
		if strings.Contains(err.Error(), tailnet.AdminDNSURL) || !strings.Contains(err.Error(), "headscale.example.org") {
			t.Fatalf("custom control server must not be sent to Tailscale's admin console: %v", err)
		}
	})
	t.Run("no address", func(t *testing.T) {
		f := tailnettest.Start(t, host+".")
		_, err := tailnet.Discover(ctx(t), &tailnet.Client{Socket: f.Socket})
		if k := problemKind(t, err); k != tailnet.ProblemNoAddress {
			t.Fatalf("kind %s err %v", k, err)
		}
	})
}

func TestCertProblem(t *testing.T) {
	hosted := tailnet.Node{DNSName: host}
	if p := tailnet.CertProblem(tailnet.Node{DNSName: host, CertNames: []string{host}}); p != nil {
		t.Fatal(p)
	}
	p := tailnet.CertProblem(hosted)
	if p == nil || p.Kind != tailnet.ProblemHTTPS || !strings.Contains(p.Error(), tailnet.AdminDNSURL) || !strings.Contains(p.Error(), "--tls-cert") {
		t.Fatalf("hosted, HTTPS off: %v", p)
	}
	hs := tailnet.Node{DNSName: "box.hs.example", ControlURL: "https://headscale.example.org"}
	p = tailnet.CertProblem(hs)
	if p == nil || p.Kind != tailnet.ProblemHTTPS || strings.Contains(p.Error(), "Tailscale account") || strings.Contains(p.Error(), tailnet.AdminDNSURL) {
		t.Fatalf("custom control: %v", p)
	}
	if !strings.Contains(p.Error(), "headscale.example.org") || !strings.Contains(p.Error(), "--tls-cert") {
		t.Fatalf("custom control: %v", p)
	}
	p = tailnet.CertProblem(tailnet.Node{DNSName: host, CertNames: []string{"other.ts.net"}})
	if p == nil || p.Kind != tailnet.ProblemCertName {
		t.Fatalf("name mismatch: %v", p)
	}
}

func TestCertPairAndErrors(t *testing.T) {
	ca := tailnettest.NewCA(t)
	f := tailnettest.Start(t, host+".", "100.72.81.114")
	c := &tailnet.Client{Socket: f.Socket}
	f.ServeCerts(t, ca, 90*24*time.Hour)
	certPEM, keyPEM, err := c.CertPair(ctx(t), host, tailnet.RenewBefore)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tailnet.ValidatePair(certPEM, keyPEM, host, time.Now()); err != nil {
		t.Fatal(err)
	}
	reqs := f.Requests()
	if q := reqs[len(reqs)-1].Query; !strings.Contains(q, "type=pair") || !strings.Contains(q, "min_validity=720h0m0s") {
		t.Fatalf("query %q", q)
	}
	node := tailnet.Node{DNSName: host}
	for _, tc := range []struct {
		code int
		body string
		hdr  http.Header
		kind string
		want string
	}{
		{403, "cert access denied", nil, tailnet.ProblemCertDenied, "sudo tailscale set --operator=$USER"},
		{429, "rate limited", http.Header{"Retry-After": {"3600"}}, tailnet.ProblemRateLimited, "retry after 3600"},
		{500, "your Tailscale account does not support getting TLS certs", nil, tailnet.ProblemHTTPS, tailnet.AdminDNSURL},
		{500, "acme: boom", nil, tailnet.ProblemOther, "acme: boom"},
	} {
		f.OnCert(func(string, string) (int, string, http.Header) { return tc.code, tc.body, tc.hdr })
		_, _, err := c.CertPair(ctx(t), host, 0)
		err = tailnet.CertError(err, node)
		if k := problemKind(t, err); k != tc.kind || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("HTTP %d: kind %s err %v", tc.code, k, err)
		}
	}
	// A malformed answer (certificate half carrying a key) is refused.
	f.OnCert(func(string, string) (int, string, http.Header) {
		c, k := ca.Issue(t, host, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		return 200, string(k) + string(c) + string(k), nil
	})
	if _, _, err := c.CertPair(ctx(t), host, 0); err == nil {
		t.Fatal("accepted a certificate half carrying a private key")
	}
}

func newStore(t *testing.T, ca *tailnettest.CA) tailnet.CertStore {
	return tailnet.CertStore{Dir: filepath.Join(t.TempDir(), "tls", "tailnet", host), Host: host, Roots: ca.Pool}
}

func TestCertStoreInstallLoadAndPermissions(t *testing.T) {
	ca := tailnettest.NewCA(t)
	s := newStore(t, ca)
	now := time.Now()
	c, k := ca.Issue(t, host, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	info, err := s.Install(c, k, now)
	if err != nil {
		t.Fatal(err)
	}
	if info.CertFile != s.CertFile() || info.KeyFile != s.KeyFile() {
		t.Fatalf("info %+v", info)
	}
	st, err := os.Stat(s.KeyFile())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %o", st.Mode().Perm())
	}
	if d, _ := os.Stat(s.Dir); d.Mode().Perm() != 0o700 {
		t.Fatalf("store dir mode %o", d.Mode().Perm())
	}
	// tls.LoadX509KeyPair through the stable paths works (what validate uses).
	if _, err := tls.LoadX509KeyPair(s.CertFile(), s.KeyFile()); err != nil {
		t.Fatal(err)
	}
	if _, got, err := s.Load(now); err != nil || !got.NotAfter.Equal(info.NotAfter) {
		t.Fatalf("load %+v %v", got, err)
	}
}

func TestCertStoreFailurePreservesCommittedPair(t *testing.T) {
	ca := tailnettest.NewCA(t)
	s := newStore(t, ca)
	now := time.Now()
	c, k := ca.Issue(t, host, now.Add(-time.Hour), now.Add(60*24*time.Hour))
	first, err := s.Install(c, k, now)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Readlink(filepath.Join(s.Dir, "current"))
	// Wrong name, expired, mismatched key, untrusted chain: none may replace it.
	wrongC, wrongK := ca.Issue(t, "other.ts.net", now.Add(-time.Hour), now.Add(90*24*time.Hour))
	oldC, oldK := ca.Issue(t, host, now.Add(-48*time.Hour), now.Add(-time.Hour))
	other := tailnettest.NewCA(t)
	untrC, untrK := other.Issue(t, host, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	for name, p := range map[string][2][]byte{
		"wrong name": {wrongC, wrongK}, "expired": {oldC, oldK}, "mismatched key": {c, wrongK}, "untrusted": {untrC, untrK},
	} {
		if _, err := s.Install(p[0], p[1], now.Add(time.Second)); err == nil {
			t.Fatalf("%s: installed", name)
		}
	}
	// A crash after a version was staged but before the switch: the pointer,
	// not the newest directory, defines the committed pair.
	newC, newK := ca.Issue(t, host, now.Add(-time.Hour), now.Add(80*24*time.Hour))
	staged := filepath.Join(s.Dir, "v9999999999999999999")
	if err := os.MkdirAll(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(staged, "cert.pem"), newC, 0o644)
	os.WriteFile(filepath.Join(staged, "key.pem"), newK, 0o600)
	os.MkdirAll(filepath.Join(s.Dir, ".new-crashed"), 0o700)
	after, _ := os.Readlink(filepath.Join(s.Dir, "current"))
	if after != before {
		t.Fatalf("pointer moved: %s -> %s", before, after)
	}
	_, got, err := s.Load(now)
	if err != nil || !got.NotAfter.Equal(first.NotAfter) {
		t.Fatalf("load after failures %+v %v", got, err)
	}
}

func TestCertStorePrunesOldVersions(t *testing.T) {
	ca := tailnettest.NewCA(t)
	s := newStore(t, ca)
	now := time.Now()
	for i := 0; i < 5; i++ {
		c, k := ca.Issue(t, host, now.Add(-time.Hour), now.Add(time.Duration(60+i)*24*time.Hour))
		if _, err := s.Install(c, k, now.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	vs, _ := filepath.Glob(filepath.Join(s.Dir, "v*"))
	if len(vs) != 3 {
		t.Fatalf("%d versions kept, want 3", len(vs))
	}
}

func writePair(t *testing.T, dir, name string, c, k []byte) (string, string) {
	t.Helper()
	cf, kf := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if err := os.WriteFile(cf, c, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kf, k, 0o600); err != nil {
		t.Fatal(err)
	}
	return cf, kf
}

func TestFindReusable(t *testing.T) {
	ca := tailnettest.NewCA(t)
	dir := t.TempDir()
	now := time.Now()
	c, k := ca.Issue(t, host, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	cf, _ := writePair(t, dir, "server", c, k)
	nc, nk := ca.Issue(t, host, now.Add(-time.Hour), now.Add(10*24*time.Hour))
	writePair(t, dir, "near-expiry", nc, nk)
	oc, ok := ca.Issue(t, "other.ts.net", now.Add(-time.Hour), now.Add(200*24*time.Hour))
	writePair(t, dir, "other", oc, ok)
	_, mk := ca.Issue(t, host, now.Add(-time.Hour), now.Add(300*24*time.Hour))
	writePair(t, dir, "mismatch", c, mk)
	info, found := tailnet.FindReusable(dir, host, tailnet.RenewBefore, now)
	if !found || info.CertFile != cf {
		t.Fatalf("found %v %+v, want server.crt", found, info)
	}
	if _, found := tailnet.FindReusable(dir, host, 100*24*time.Hour, now); found {
		t.Fatal("reused a pair with less validity than required")
	}
	// Reuse reads only: the source files are unchanged.
	if b, _ := os.ReadFile(cf); string(b) != string(c) {
		t.Fatal("source certificate changed")
	}
}

// tlsServer serves TLS with the reloader and echoes lines on each connection.
func tlsServer(t *testing.T, r *tailnet.Reloader) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{GetCertificate: r.GetCertificate, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go io.Copy(c, c)
		}
	}()
	return ln.Addr().String()
}

func dialLeafNotAfter(t *testing.T, addr string, ca *tailnettest.CA) (*tls.Conn, time.Time) {
	t.Helper()
	c, err := tls.Dial("tcp", addr, &tls.Config{RootCAs: ca.Pool, ServerName: host})
	if err != nil {
		t.Fatal(err)
	}
	return c, c.ConnectionState().PeerCertificates[0].NotAfter
}

func echo(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != msg {
		t.Fatalf("echo %q %v", buf, err)
	}
}

func TestRenewerRenewsInPlaceAndStreamsContinue(t *testing.T) {
	ca := tailnettest.NewCA(t)
	s := newStore(t, ca)
	now := time.Now()
	c, k := ca.Issue(t, host, now.Add(-time.Hour), now.Add(20*24*time.Hour)) // inside the renewal window
	if _, err := s.Install(c, k, now); err != nil {
		t.Fatal(err)
	}
	pair, info, err := s.Load(now)
	if err != nil {
		t.Fatal(err)
	}
	rl := tailnet.NewReloader(pair)
	addr := tlsServer(t, rl)
	stream, oldNotAfter := dialLeafNotAfter(t, addr, ca)
	defer stream.Close()
	echo(t, stream, "before\n")

	fetches := 0
	var states []tailnet.CertState
	r := &tailnet.Renewer{Mode: tailnet.CertTailscale, Target: rl, Store: s, Now: func() time.Time { return now },
		Fetch: func(_ context.Context, min time.Duration) ([]byte, []byte, error) {
			fetches++
			if min != tailnet.RenewBefore {
				t.Errorf("min validity %s", min)
			}
			c, k := ca.Issue(t, host, now.Add(-time.Hour), now.Add(90*24*time.Hour))
			return c, k, nil
		},
		OnChange: func(cs tailnet.CertState) { states = append(states, cs) }}
	r.Prime(info)
	if err := r.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Fatalf("%d fetches", fetches)
	}
	// The open stream keeps working on its negotiated certificate…
	echo(t, stream, "after renewal\n")
	// …and new handshakes get the renewed one.
	c2, newNotAfter := dialLeafNotAfter(t, addr, ca)
	c2.Close()
	if !newNotAfter.After(oldNotAfter) {
		t.Fatalf("new handshake got %s, old %s", newNotAfter, oldNotAfter)
	}
	if st := states[len(states)-1]; st.LastError != "" || st.LastRenewal.IsZero() || !st.NotAfter.Equal(newNotAfter) {
		t.Fatalf("state %+v", st)
	}
	// Plenty of validity now: the next check does not fetch.
	if err := r.Check(context.Background()); err != nil || fetches != 1 {
		t.Fatalf("second check fetched (%d) %v", fetches, err)
	}
}

func TestRenewerFailureKeepsServingCurrentCert(t *testing.T) {
	ca := tailnettest.NewCA(t)
	s := newStore(t, ca)
	now := time.Now()
	c, k := ca.Issue(t, host, now.Add(-time.Hour), now.Add(10*24*time.Hour))
	if _, err := s.Install(c, k, now); err != nil {
		t.Fatal(err)
	}
	pair, info, _ := s.Load(now)
	rl := tailnet.NewReloader(pair)
	before, _ := os.Readlink(filepath.Join(s.Dir, "current"))
	for name, fetch := range map[string]func(context.Context, time.Duration) ([]byte, []byte, error){
		"fetch error": func(context.Context, time.Duration) ([]byte, []byte, error) {
			return nil, nil, errors.New("tailscaled unreachable")
		},
		"invalid pair": func(context.Context, time.Duration) ([]byte, []byte, error) {
			c, k := ca.Issue(t, "wrong.ts.net", now, now.Add(90*24*time.Hour))
			return c, k, nil
		},
	} {
		r := &tailnet.Renewer{Mode: tailnet.CertTailscale, Target: rl, Store: s, Fetch: fetch, Now: func() time.Time { return now }}
		r.Prime(info)
		if err := r.Check(context.Background()); err == nil {
			t.Fatalf("%s: no error", name)
		}
		if st := r.State(); st.LastError == "" || !st.NextCheck.Equal(now.Add(time.Hour)) {
			t.Fatalf("%s: state %+v (want error and a retry in 1h)", name, st)
		}
		if !rl.Current().Leaf.NotAfter.Equal(info.NotAfter) {
			t.Fatalf("%s: served certificate changed", name)
		}
		if after, _ := os.Readlink(filepath.Join(s.Dir, "current")); after != before {
			t.Fatalf("%s: committed pair changed", name)
		}
	}
}

func TestRenewerPicksUpNewerStoredVersion(t *testing.T) {
	ca := tailnettest.NewCA(t)
	s := newStore(t, ca)
	now := time.Now()
	c, k := ca.Issue(t, host, now.Add(-time.Hour), now.Add(60*24*time.Hour))
	s.Install(c, k, now)
	pair, info, _ := s.Load(now)
	rl := tailnet.NewReloader(pair)
	r := &tailnet.Renewer{Mode: tailnet.CertTailscale, Target: rl, Store: s, Now: func() time.Time { return now }}
	r.Prime(info)
	c2, k2 := ca.Issue(t, host, now.Add(-time.Hour), now.Add(85*24*time.Hour))
	if _, err := s.Install(c2, k2, now.Add(time.Second)); err != nil { // a later enable
		t.Fatal(err)
	}
	if err := r.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rl.Current().Leaf.NotAfter.After(info.NotAfter) {
		t.Fatal("newer stored version not picked up")
	}
}

func TestRenewerReloadsExplicitFiles(t *testing.T) {
	// A private CA, as explicit certificates may use: no public-root check.
	ca := tailnettest.NewCA(t)
	dir := t.TempDir()
	now := time.Now()
	c, k := ca.Issue(t, "amux.home.example", now.Add(-time.Hour), now.Add(10*24*time.Hour))
	cf, kf := writePair(t, dir, "explicit", c, k)
	pair, info, err := tailnet.LoadPair(cf, kf, "", now)
	if err != nil {
		t.Fatal(err)
	}
	rl := tailnet.NewReloader(pair)
	r := &tailnet.Renewer{Mode: tailnet.CertExplicit, Target: rl, CertFile: cf, KeyFile: kf, Now: func() time.Time { return now }}
	r.Prime(info)
	if err := r.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Half-replaced (new cert, old key): keep serving, report.
	c2, k2 := ca.Issue(t, "amux.home.example", now.Add(-time.Hour), now.Add(90*24*time.Hour))
	os.WriteFile(cf, c2, 0o644)
	if err := r.Check(context.Background()); err == nil {
		t.Fatal("mismatched pair accepted")
	}
	if !rl.Current().Leaf.NotAfter.Equal(info.NotAfter) {
		t.Fatal("served certificate changed on a failed reload")
	}
	os.WriteFile(kf, k2, 0o600)
	if err := r.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rl.Current().Leaf.NotAfter.After(info.NotAfter) {
		t.Fatal("replaced explicit pair not reloaded")
	}
}

func TestOrchestratorOrigin(t *testing.T) {
	for _, tc := range []struct{ in, sni, want string }{
		{"harness.gcp.example.dev:443", "", "https://harness.gcp.example.dev:443"},
		{"tls:harness.example:7443", "", "https://harness.example:7443"},
		{"tls://harness.example:7443", "", "https://harness.example:7443"},
		{"wss://harness.example", "", "https://harness.example"},
		{"wss://harness.example:8443/", "", "https://harness.example:8443"},
		{"ws://localhost:3000", "", "http://localhost:3000"},
		{"https://harness.example", "", "https://harness.example"},
		{"harness.example:443", "harness.example", "https://harness.example:443"},
	} {
		got, err := tailnet.OrchestratorOrigin(tc.in, tc.sni)
		if err != nil || got != tc.want {
			t.Errorf("%q: %q %v, want %q", tc.in, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ in, sni string }{
		{"", ""}, {"wss://harness.example/relay", ""}, {"harness.example:443", "other.example"}, {"user@h:1", ""},
	} {
		if got, err := tailnet.OrchestratorOrigin(tc.in, tc.sni); err == nil {
			t.Errorf("%q/%q: derived %q, want refusal", tc.in, tc.sni, got)
		}
	}
}

func TestPortProblem(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	if k := problemKind(t, tailnet.PortProblem(addr, func() bool { return false })); k != tailnet.ProblemPortInUse {
		t.Fatalf("kind %s", k)
	}
	if err := tailnet.PortProblem(addr, func() bool { return true }); err != nil {
		t.Fatalf("owned listener reported as a conflict: %v", err)
	}
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	fa := free.Addr().String()
	free.Close()
	if err := tailnet.PortProblem(fa, nil); err != nil {
		t.Fatal(err)
	}
}

func TestSocketResolution(t *testing.T) {
	t.Setenv(tailnet.EnvSocket, "")
	if tailnet.Socket("") != tailnet.DefaultSocket() {
		t.Fatal("default")
	}
	t.Setenv(tailnet.EnvSocket, "/tmp/x.sock")
	if tailnet.Socket("") != "/tmp/x.sock" || tailnet.Socket("/explicit") != "/explicit" {
		t.Fatal("precedence")
	}
}
