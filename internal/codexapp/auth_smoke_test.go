package codexapp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSmokeRealAppServerCentralAuth checks the external-token wire against an
// ACTUAL `codex app-server`. It is opt-in: AMUX_TEST_CODEX_BIN names the
// binary (CI pins 0.157.1); absent, it skips. It uses only synthetic unsigned
// JWTs, an isolated HOME, and a local fake ChatGPT backend, so no real
// credential, login, or model inference is involved.
//
// Asserted:
//   - the real server accepts account/login/start chatgptAuthTokens and
//     reports authMode chatgptAuthTokens;
//   - nothing is written to auth.json (the tokens are memory-only);
//   - the injected token reaches the backend, and when the backend answers
//     401 the server asks amux to refresh and retries with the new token;
//   - no token appears in the event log.
func TestSmokeRealAppServerCentralAuth(t *testing.T) {
	bin := os.Getenv("AMUX_TEST_CODEX_BIN")
	if bin == "" {
		t.Skip("set AMUX_TEST_CODEX_BIN to a pinned codex binary to run")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", home)
	t.Setenv("XDG_RUNTIME_DIR", home)
	first, next := smokeJWT("first"), smokeJWT("next")

	backend := &smokeBackend{reject: first, account: "acct-smoke"}
	// Workspace routing requires an HTTPS origin, so the fake backend speaks
	// TLS and Codex trusts it through CODEX_CA_CERTIFICATE (passed by the
	// launch environment's overlay, as a corporate CA would be).
	caPEM, leaf := smokeCA(t)
	srv := httptest.NewUnstartedServer(backend)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	defer srv.Close()
	caPath := filepath.Join(home, "fake-backend-ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	// file storage would expose any persistence of the injected tokens.
	cfg := fmt.Sprintf("chatgpt_base_url = %q\ncli_auth_credentials_store = \"file\"\n", srv.URL+"/backend-api/")
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var reqs []TokenRequest
	provider := providerFunc(func(_ context.Context, r TokenRequest) (ChatGPTToken, error) {
		mu.Lock()
		reqs = append(reqs, r)
		mu.Unlock()
		tok := first
		if r.Refresh {
			tok = next
		}
		return ChatGPTToken{AccessToken: tok, AccountID: "acct-smoke", PlanType: "pro"}, nil
	})
	sup := New(Config{
		SessionID:    "auth-smoke",
		Bin:          bin,
		Dir:          home,
		Endpoint:     "unix://" + filepath.Join(home, "app.sock"),
		EventLogPath: filepath.Join(home, "events.jsonl"),
		ChatGPTAuth:  provider,
		Env:          []string{"CODEX_CA_CERTIFICATE=" + caPath},
	})
	if err := sup.Start(ctx, nil); err != nil {
		t.Fatalf("Start with central auth against the real binary: %v", err)
	}
	defer sup.Close()
	t.Logf("real binary accepted chatgptAuthTokens; thread %s", sup.ThreadID())

	acct, err := sup.rpc.call(ctx, "account/read", map[string]any{})
	if err != nil {
		t.Fatalf("account/read: %v", err)
	}
	if !strings.Contains(string(acct), `"chatgpt"`) {
		t.Fatalf("account/read does not report a ChatGPT account: %s", acct)
	}
	if !waitFor(func() bool {
		data, _ := os.ReadFile(sup.cfg.EventLogPath)
		return strings.Contains(string(data), `"authMode":"chatgptAuthTokens"`)
	}, 5*time.Second) {
		data, _ := os.ReadFile(sup.cfg.EventLogPath)
		backend.mu.Lock()
		t.Logf("backend requests so far: %v", backend.paths)
		backend.mu.Unlock()
		t.Fatalf("server never reported authMode chatgptAuthTokens; events:\n%s", data)
	}

	// A backend call carrying the token: the fake rejects the first token, so
	// the server must ask amux for a refresh and retry with the next one.
	if res, err := sup.rpc.call(ctx, "account/rateLimits/read", nil); err != nil {
		t.Logf("account/rateLimits/read: %v (outcome of the fake backend's reply, not the auth wire)", err)
	} else {
		t.Logf("account/rateLimits/read: %s", res)
	}
	seen := backend.seen()
	backend.mu.Lock()
	t.Logf("backend requests: %v", backend.paths)
	backend.mu.Unlock()
	if !containsToken(seen, first) {
		t.Fatalf("injected token never reached the backend (saw %d requests)", len(seen))
	}
	mu.Lock()
	refreshed := false
	for _, r := range reqs {
		refreshed = refreshed || r.Refresh
	}
	mu.Unlock()
	if !refreshed || !containsToken(seen, next) {
		t.Fatalf("401 did not drive a refresh+retry: refreshed=%v retried=%v", refreshed, containsToken(seen, next))
	}

	var stored []string
	_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "auth.json" {
			stored = append(stored, p)
		}
		return nil
	})
	if len(stored) > 0 {
		t.Fatalf("external tokens were persisted: %v", stored)
	}
	// Memory-only: no file Codex wrote (state DBs, logs, rollouts) holds either
	// token, raw or as the JWT payload segment.
	_ = sup.Close()
	var holders []string
	_ = filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		for _, tok := range []string{first, next} {
			if strings.Contains(string(data), strings.Split(tok, ".")[1]) {
				holders = append(holders, p)
				break
			}
		}
		return nil
	})
	if len(holders) > 0 {
		t.Fatalf("token material written to disk: %v", holders)
	}
	if data, _ := os.ReadFile(sup.cfg.EventLogPath); strings.Contains(string(data), first) || strings.Contains(string(data), next) {
		t.Fatal("token leaked into the event log")
	}
}

// smokeCA issues a throwaway CA and a 127.0.0.1 leaf signed by it. webpki
// (rustls) rejects a CA certificate presented as the server's own leaf, which
// is what httptest's built-in certificate is.
func smokeCA(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "amux smoke CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
}

// smokeJWT builds an unsigned JWT with ChatGPT-shaped claims. It is not a
// credential for any real service.
func smokeJWT(tag string) string {
	enc := base64.RawURLEncoding.EncodeToString
	header, _ := json.Marshal(map[string]any{"alg": "none", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{
		"exp":                            time.Now().Add(time.Hour).Unix(),
		"email":                          "smoke@example.invalid",
		"jti":                            tag,
		"https://api.openai.com/auth":    map[string]any{"chatgpt_account_id": "acct-smoke", "chatgpt_plan_type": "pro", "chatgpt_user_id": "user-smoke"},
		"https://api.openai.com/profile": map[string]any{"email": "smoke@example.invalid"},
	})
	return enc(header) + "." + enc(claims) + ".sig"
}

// smokeBackend records bearer tokens and answers 401 to the rejected one.
type smokeBackend struct {
	reject  string
	account string
	mu      sync.Mutex
	tokens  []string
	paths   []string
}

func (b *smokeBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	b.mu.Lock()
	if tok != "" {
		b.tokens = append(b.tokens, tok)
	}
	b.paths = append(b.paths, r.Method+" "+r.URL.Path+" auth="+map[bool]string{true: "yes", false: "no"}[tok != ""])
	b.mu.Unlock()
	if r.URL.Path == "/backend-api/wham/accounts/check" {
		// Routing discovery for the injected workspace. The first token is
		// accepted here so login completes; later calls exercise the refresh.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accounts": []map[string]any{{
				"id": b.account, "plan_type": "pro", "name": "smoke",
				"workspace_backend_origin": "NO_CONSTRAINT", "account_routing_override": "NO_CONSTRAINT",
			}},
			"account_ordering":   []string{b.account},
			"default_account_id": b.account,
		})
		return
	}
	if tok == b.reject {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"expired"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{}`))
}

func (b *smokeBackend) seen() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.tokens...)
}

func containsToken(seen []string, tok string) bool {
	for _, s := range seen {
		if s == tok {
			return true
		}
	}
	return false
}
