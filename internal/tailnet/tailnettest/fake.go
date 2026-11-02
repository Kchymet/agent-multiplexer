// Package tailnettest is a fake tailscaled LocalAPI and a throwaway
// certificate authority for tests of tailnet mode. Nothing here contacts a
// real tailscaled, a real control server or a real CA.
package tailnettest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// CA is a throwaway certificate authority.
type CA struct {
	Cert *x509.Certificate
	key  *ecdsa.PrivateKey
	Pool *x509.CertPool
	n    int64
	mu   sync.Mutex
}

// NewCA makes a CA valid around now.
func NewCA(t testing.TB) *CA {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "amux test root"},
		NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(c)
	return &CA{Cert: c, key: k, Pool: pool, n: 1}
}

// Issue returns a PEM leaf certificate and key for host, valid from
// notBefore to notAfter.
func (ca *CA) Issue(t testing.TB, host string, notBefore, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	ca.mu.Lock()
	ca.n++
	serial := ca.n
	ca.mu.Unlock()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: host},
		DNSNames: []string{host}, NotBefore: notBefore, NotAfter: notAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &k.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

// Request is one request the fake saw.
type Request struct {
	Method, Path, Host, Origin, Referer string
	Query                               string
}

// LocalAPI is a fake tailscaled LocalAPI on a unix socket. It enforces the
// same request shape the real one does (Host local-tailscaled.sock, no
// Origin or Referer).
type LocalAPI struct {
	Socket string

	mu         sync.Mutex
	status     map[string]any
	prefs      map[string]any
	statusCode int    // non-zero: /status answers this code
	statusBody string // with this body
	certFn     func(domain, minValidity string) (code int, body string, header http.Header)
	requests   []Request
	srv        *http.Server
}

// Start serves a fake LocalAPI for a Running node.
func Start(t testing.TB, dnsName string, ips ...string) *LocalAPI {
	t.Helper()
	dir, err := os.MkdirTemp("", "tsapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &LocalAPI{Socket: filepath.Join(dir, "tailscaled.sock")}
	f.SetRunning(dnsName, ips...)
	f.prefs = map[string]any{"ControlURL": "https://controlplane.tailscale.com"}
	ln, err := net.Listen("unix", f.Socket)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = &http.Server{Handler: http.HandlerFunc(f.serve)}
	go f.srv.Serve(ln)
	t.Cleanup(func() { f.srv.Close() })
	return f
}

// SetRunning makes the node Running with this identity; HTTPS certificates
// are available for dnsName (trailing dot trimmed) unless SetCertDomains
// changes it.
func (f *LocalAPI) SetRunning(dnsName string, ips ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := strings.TrimSuffix(dnsName, ".")
	suffix := ""
	if i := strings.IndexByte(name, '.'); i >= 0 {
		suffix = name[i+1:]
	}
	f.status = map[string]any{
		"BackendState":   "Running",
		"Self":           map[string]any{"DNSName": dnsName, "TailscaleIPs": ips},
		"CertDomains":    []string{name},
		"CurrentTailnet": map[string]any{"Name": suffix, "MagicDNSSuffix": suffix, "MagicDNSEnabled": true},
	}
}

// Set changes one top-level status field.
func (f *LocalAPI) Set(key string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status[key] = v
}

// SetMagicDNS turns MagicDNS on or off in the reported tailnet.
func (f *LocalAPI) SetMagicDNS(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ct, ok := f.status["CurrentTailnet"].(map[string]any); ok {
		ct["MagicDNSEnabled"] = on
	}
}

// SetControlURL sets the prefs ControlURL (e.g. a Headscale server).
func (f *LocalAPI) SetControlURL(u string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prefs["ControlURL"] = u
}

// FailStatus makes /status answer code with body (0 restores it).
func (f *LocalAPI) FailStatus(code int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCode, f.statusBody = code, body
}

// OnCert sets the /cert responder.
func (f *LocalAPI) OnCert(fn func(domain, minValidity string) (int, string, http.Header)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.certFn = fn
}

// ServeCerts answers /cert with pairs issued by ca for validity.
func (f *LocalAPI) ServeCerts(t testing.TB, ca *CA, validity time.Duration) {
	f.OnCert(func(domain, _ string) (int, string, http.Header) {
		c, k := ca.Issue(t, domain, time.Now().Add(-time.Hour), time.Now().Add(validity))
		return 200, string(k) + string(c), nil
	})
}

// Requests returns what the fake saw.
func (f *LocalAPI) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// CertRequests counts /cert requests.
func (f *LocalAPI) CertRequests() int {
	n := 0
	for _, r := range f.Requests() {
		if strings.HasPrefix(r.Path, "/localapi/v0/cert/") {
			n++
		}
	}
	return n
}

func (f *LocalAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, Request{Method: r.Method, Path: r.URL.Path, Host: r.Host, Origin: r.Header.Get("Origin"), Referer: r.Referer(), Query: r.URL.RawQuery})
	status, prefs, code, body, certFn := f.status, f.prefs, f.statusCode, f.statusBody, f.certFn
	sb, _ := json.Marshal(status)
	pb, _ := json.Marshal(prefs)
	f.mu.Unlock()
	if r.Referer() != "" || r.Header.Get("Origin") != "" || (r.Host != "" && r.Host != "local-tailscaled.sock") {
		http.Error(w, "invalid localapi request", http.StatusForbidden)
		return
	}
	switch {
	case r.URL.Path == "/localapi/v0/status":
		if code != 0 {
			http.Error(w, body, code)
			return
		}
		w.Write(sb)
	case r.URL.Path == "/localapi/v0/prefs":
		w.Write(pb)
	case strings.HasPrefix(r.URL.Path, "/localapi/v0/cert/"):
		if r.URL.Query().Get("type") != "pair" {
			http.Error(w, "want pair", 400)
			return
		}
		if certFn == nil {
			http.Error(w, "your Tailscale account does not support getting TLS certs", 500)
			return
		}
		c, b, h := certFn(strings.TrimPrefix(r.URL.Path, "/localapi/v0/cert/"), r.URL.Query().Get("min_validity"))
		for k, v := range h {
			w.Header()[k] = v
		}
		if c != 200 {
			http.Error(w, b, c)
			return
		}
		w.Write([]byte(b))
	default:
		http.NotFound(w, r)
	}
}
