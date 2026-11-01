package direct

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// writeCert creates a CA-signed-equivalent self-signed leaf for name.
func writeCert(t *testing.T, dir, name string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, _ := x509.MarshalECPrivateKey(key)
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	_ = os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600)
	cert, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

// TestCloudAbsentEndToEnd drives the whole S1 client path against a real TLS
// listener: paste bundle, hello verification, enrollment, inventory, transcript
// stream and a prompt. Every connection the client opens is recorded: the only
// destination is the direct listener. No Harness or other cloud endpoint exists
// in the configuration at all.
func TestCloudAbsentEndToEnd(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1") // must not be consulted
	h := newHostAuthority(t)
	root := shortRoot(t)
	certFile, keyFile, pool := writeCert(t, t.TempDir(), "amux.test")
	f := newFake(testRows()...)
	ready := make(chan net.Addr, 1)
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, ServeConfig{
			Root: root, Listen: "127.0.0.1:0", CertFile: certFile, KeyFile: keyFile,
			Endpoints: []string{testEndpoint}, AllowOrigins: []string{testOrigin},
			Backend: f, Verify: h.verify,
			Ready: func(addr net.Addr, _ string) { ready <- addr },
		})
	}()
	var addr net.Addr
	select {
	case addr = <-ready:
	case err := <-served:
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	})

	var dialMu sync.Mutex
	var dialed []string
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
			dialMu.Lock()
			dialed = append(dialed, a)
			dialMu.Unlock()
			if a != testHost {
				return nil, &net.OpError{Op: "dial", Err: os.ErrPermission}
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr.String())
		},
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "amux.test"},
	}
	client := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(tr.CloseIdleConnections)

	// Operator: enroll over the host-only admin channel.
	adm, err := AdminCall(context.Background(), root, h.host, AdminRequest{Op: OpEnroll, Label: "Pixel",
		Permissions: []string{PermRead, PermSteer}, Resource: ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"wg1"}}})
	if err != nil {
		t.Fatal(err)
	}
	// Client: decode the pasted bundle, prove the destination, then redeem.
	b, err := DecodeBundle(adm.Bundle, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	hreq := HelloRequest{Protocol: Protocol, ClientNonce: nonce(), Endpoint: b.Endpoints[0]}
	body, _ := json.Marshal(hreq)
	resp, err := client.Post(b.Endpoints[0]+PathHello, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	var hr HelloResponse
	_ = json.NewDecoder(resp.Body).Decode(&hr)
	resp.Body.Close()
	if _, err := VerifyHello(hr.Proof, b.ServerID, hreq, time.Now()); err != nil {
		t.Fatalf("hello: %v", err)
	}
	resp, err = client.PostForm(b.Endpoints[0]+PathToken, url.Values{"grant_type": {GrantTypeEnrollmentCode},
		"code": {b.Code}, "client_label": {"Pixel"}, "client_kind": {ClientAndroid}})
	if err != nil {
		t.Fatal(err)
	}
	var tok TokenResponse
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if tok.AccessToken == "" || tok.ServerID != b.ServerID {
		t.Fatalf("token: %+v", tok)
	}
	get := func(path string) *http.Response {
		req, _ := http.NewRequest("GET", b.Endpoints[0]+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
		r, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := get(PathSession)
	var sr SessionsResponse
	_ = json.NewDecoder(r.Body).Decode(&sr)
	r.Body.Close()
	if len(sr.Sessions) != 2 {
		t.Fatalf("inventory: %+v", sr)
	}
	stream := get(PathSession + "/a1/stream?after=0")
	t.Cleanup(func() { stream.Body.Close() })
	events := sse(t, stream)
	<-f.streamStarted
	f.feed("a1") <- hp.RuntimeEventBatch{Seq: 1, Runtime: "claude", Events: []hp.RuntimeEvent{{Type: hp.TypeTurnStart}}}
	if e := nextEvent(t, events); e[0] != "events" {
		t.Fatalf("stream: %v", e)
	}
	req, _ := http.NewRequest("POST", b.Endpoints[0]+PathSession+"/a1/actions", strings.NewReader(actionBody(VerbPrompt, "hi", "e2e")))
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	r, err = client.Do(req)
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("prompt: %v %v", err, r)
	}
	r.Body.Close()

	// A client that does not trust the certificate never reaches the server.
	untrusted := &http.Client{Transport: &http.Transport{DialContext: tr.DialContext, TLSClientConfig: &tls.Config{ServerName: "amux.test"}}}
	if _, err := untrusted.Post(b.Endpoints[0]+PathHello, "application/json", strings.NewReader(string(body))); err == nil {
		t.Fatal("untrusted TLS accepted")
	}

	dialMu.Lock()
	defer dialMu.Unlock()
	for _, d := range dialed {
		if d != testHost {
			t.Fatalf("client dialed %s; only the direct endpoint may be contacted", d)
		}
	}
}

func TestServeRefusesUnsafeConfigurationBeforeBinding(t *testing.T) {
	h := newHostAuthority(t)
	certFile, keyFile, _ := writeCert(t, t.TempDir(), "amux.test")
	base := ServeConfig{Root: shortRoot(t), Listen: "127.0.0.1:0", CertFile: certFile, KeyFile: keyFile,
		Endpoints: []string{testEndpoint}, Backend: newFake(), Verify: h.verify}
	for name, mut := range map[string]func(*ServeConfig){
		"public bind":      func(c *ServeConfig) { c.Listen = "0.0.0.0:0" },
		"no tls":           func(c *ServeConfig) { c.CertFile, c.KeyFile = "", "" },
		"http endpoint":    func(c *ServeConfig) { c.Endpoints = []string{"http://amux.test"} },
		"path endpoint":    func(c *ServeConfig) { c.Endpoints = []string{"https://amux.test/x"} },
		"wildcard origin":  func(c *ServeConfig) { c.AllowOrigins = []string{"*"} },
		"no host verifier": func(c *ServeConfig) { c.Verify = nil },
		"no endpoints":     func(c *ServeConfig) { c.Endpoints = nil },
	} {
		cfg := base
		mut(&cfg)
		cfg.Ready = func(net.Addr, string) { t.Errorf("%s: listener became ready", name) }
		if err := Serve(context.Background(), cfg); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
