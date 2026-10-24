package codexapp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"amux/internal/codexauth"
)

// TestSmokeCentralProviderToSession drives the whole central-auth lifecycle
// against REAL Codex binaries: a codexauth.Provider broker (native managed
// login, launched with the daemon's agent.Argv prefix) refreshes through a
// local fake OAuth endpoint (CODEX_REFRESH_TOKEN_URL_OVERRIDE), and a real
// session App Server receives its tokens only through the codexapp adapter.
// Opt-in via AMUX_TEST_CODEX_BIN; synthetic tokens and local TLS fakes only.
//
// Asserted: the session starts on the central token; Codex's 401 recovery
// asks amux for a refresh; the broker performs exactly one OAuth rotation
// (presenting the seeded refresh token) and persists the rotated one; the
// session retries with the rotated access token; no token or refresh token
// appears outside the central home; broker stderr carries no refresh token.
func TestSmokeCentralProviderToSession(t *testing.T) {
	bin := os.Getenv("AMUX_TEST_CODEX_BIN")
	if bin == "" {
		t.Skip("set AMUX_TEST_CODEX_BIN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", root)
	t.Setenv("XDG_RUNTIME_DIR", root)

	jwt1, jwt2 := smokeJWT("gen1"), smokeJWT("gen2")
	idTok := smokeJWT("id")
	b := &e2eBackend{reject: jwt1, next: jwt2, id: idTok}
	caPEM, leaf := smokeCA(t)
	srv := httptest.NewUnstartedServer(b)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{leaf}}
	srv.StartTLS()
	defer srv.Close()
	caPath := filepath.Join(root, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	cfgTOML := "chatgpt_base_url = \"" + srv.URL + "/backend-api/\"\n"

	// Central home seeded as a completed managed ChatGPT login.
	central := filepath.Join(root, "auth", "codex")
	if err := os.MkdirAll(central, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(filepath.Dir(central), 0o700)
	auth := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token": idTok, "access_token": jwt1, "refresh_token": "rt-ONE-SECRET", "account_id": "acct-smoke",
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
	}
	ab, _ := json.Marshal(auth)
	if err := os.WriteFile(filepath.Join(central, "auth.json"), ab, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(central, "config.toml"), []byte(cfgTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	var smu sync.Mutex
	p, err := codexauth.New(codexauth.Config{
		Argv: brokerArgv(bin), Home: central,
		Env: append(os.Environ(),
			"CODEX_REFRESH_TOKEN_URL_OVERRIDE="+srv.URL+"/oauth/token",
			"CODEX_CA_CERTIFICATE="+caPath),
		Stderr: writerFunc(func(p []byte) (int, error) { smu.Lock(); defer smu.Unlock(); return stderr.Write(p) }),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.Start(ctx); err != nil {
		t.Fatalf("broker start: %v", err)
	}
	shim := providerFunc(func(ctx context.Context, r TokenRequest) (ChatGPTToken, error) {
		t0 := time.Now()
		tk, err := p.SessionToken(ctx, r.SessionID, r.Refresh, r.PreviousAccountID)
		t.Logf("shim: refresh=%v -> gen=%d err=%v after %v", r.Refresh, tk.Generation, err, time.Since(t0).Round(time.Millisecond))
		if err != nil {
			return ChatGPTToken{}, err
		}
		return ChatGPTToken{AccessToken: tk.AccessToken, AccountID: tk.AccountID, PlanType: tk.PlanType}, nil
	})

	sessHome := filepath.Join(root, "session")
	if err := os.MkdirAll(filepath.Join(sessHome, ".codex"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessHome, ".codex", "config.toml"), []byte(cfgTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	sup := New(Config{
		SessionID: "e2e", Bin: bin, Dir: sessHome,
		Endpoint:     "unix://" + filepath.Join(root, "s.sock"),
		EventLogPath: filepath.Join(root, "events.jsonl"),
		ChatGPTAuth:  shim,
		Env:          []string{"CODEX_CA_CERTIFICATE=" + caPath, "CODEX_HOME=" + filepath.Join(sessHome, ".codex")},
	})
	start := time.Now()
	b.mu.Lock()
	t.Logf("before session start: oauth refreshes=%d", b.refreshes)
	b.mu.Unlock()
	if err := sup.Start(ctx, nil); err != nil {
		t.Fatalf("session start: %v", err)
	}
	defer sup.Close()
	t.Logf("session started in %v with the central token", time.Since(start))

	t0 := time.Now()
	// Codex reports rateLimits 401s rather than recovering them; the refresh is
	// driven by its own background backend calls, which the fake rejects for
	// the seeded token. The fake's {} body makes the read itself fail to decode.
	_, rlErr := sup.rpc.call(ctx, "account/rateLimits/read", nil)
	t.Logf("rateLimits/read after %v: err=%v (fake body; not an assertion)", time.Since(t0), rlErr)

	waitFor(func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.saw[jwt2] }, 20*time.Second)
	b.mu.Lock()
	t.Logf("requests carrying the rotated token: %v", b.nextPaths)
	refreshes, sawNext := b.refreshes, b.saw[jwt2]
	gotRT := append([]string(nil), b.refreshTokens...)
	b.mu.Unlock()
	for i, rt := range gotRT {
		t.Logf("oauth refresh %d presented %s", i+1, strings.TrimSuffix(strings.TrimPrefix(rt, "rt-"), "-SECRET"))
	}
	t.Logf("oauth refreshes=%d refresh tokens presented=%d session retried with rotated token=%v", refreshes, len(gotRT), sawNext)
	if len(gotRT) != 1 || gotRT[0] != "rt-ONE-SECRET" {
		t.Fatalf("broker did not present the seeded refresh token exactly once")
	}
	if refreshes != 1 {
		t.Fatalf("central OAuth refreshes = %d, want exactly 1", refreshes)
	}
	if !sawNext {
		t.Fatal("session never retried with the centrally rotated token")
	}
	nb, _ := os.ReadFile(filepath.Join(central, "auth.json"))
	if !strings.Contains(string(nb), "rt-TWO-SECRET") {
		t.Fatal("broker did not persist the rotated refresh token")
	}
	_ = sup.Close()
	var leaks []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(path, central) {
			return nil
		}
		data, _ := os.ReadFile(path)
		for _, s := range []string{strings.Split(jwt1, ".")[1], strings.Split(jwt2, ".")[1], "rt-ONE-SECRET", "rt-TWO-SECRET"} {
			if strings.Contains(string(data), s) {
				leaks = append(leaks, path)
				break
			}
		}
		return nil
	})
	if len(leaks) > 0 {
		t.Fatalf("token material outside the central home: %v", leaks)
	}
	smu.Lock()
	if strings.Contains(stderr.String(), "SECRET") {
		t.Fatal("broker stderr leaked a refresh token")
	}
	smu.Unlock()
}

// brokerArgv is what the daemon would pass: agent.Argv("codex", "") with its
// TUI/approval/sandbox flags, unless AMUX_E2E_BARE_ARGV is set.
func brokerArgv(bin string) []string {
	if os.Getenv("AMUX_E2E_BARE_ARGV") != "" {
		return []string{bin}
	}
	return []string{bin, "-c", `tui.alternate_screen="always"`, "-c", `approval_policy="on-request"`, "-c", `approvals_reviewer="auto_review"`, "--sandbox", "workspace-write"}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

type e2eBackend struct {
	reject, next, id string
	mu               sync.Mutex
	refreshes        int
	refreshTokens    []string
	saw              map[string]bool
	nextPaths        []string
	refreshAt        []time.Time
}

func (b *e2eBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	b.mu.Lock()
	if b.saw == nil {
		b.saw = map[string]bool{}
	}
	b.saw[tok] = true
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/oauth/token":
		body, _ := io.ReadAll(r.Body)
		var req struct {
			RefreshToken string `json:"refresh_token"`
		}
		_ = json.Unmarshal(body, &req)
		b.mu.Lock()
		b.refreshes++
		b.refreshTokens = append(b.refreshTokens, req.RefreshToken)
		b.refreshAt = append(b.refreshAt, time.Now())
		b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id_token": b.id, "access_token": b.next, "refresh_token": "rt-TWO-SECRET"})
		return
	case "/backend-api/wham/accounts/check":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accounts": []map[string]any{{"id": "acct-smoke", "plan_type": "pro", "name": "e2e",
				"workspace_backend_origin": "NO_CONSTRAINT", "account_routing_override": "NO_CONSTRAINT"}},
			"account_ordering": []string{"acct-smoke"}, "default_account_id": "acct-smoke",
		})
		return
	}
	if tok == b.next {
		b.mu.Lock()
		b.nextPaths = append(b.nextPaths, r.Method+" "+r.URL.Path)
		b.mu.Unlock()
	}
	if tok == b.reject {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"expired"}}`))
		return
	}
	_, _ = w.Write([]byte(`{}`))
}
