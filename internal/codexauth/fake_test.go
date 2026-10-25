package codexauth

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as a fake native `codex app-server`: with fakeEnv set
// it speaks the stdio JSON-RPC account/* subset against $CODEX_HOME/auth.json,
// writing a refresh token the provider must never surface.
const (
	fakeEnv          = "CODEXAUTH_FAKE_SERVER"
	fakeTTL          = "CODEXAUTH_FAKE_TTL"           // access token lifetime (Go duration)
	fakeHangLogin    = "CODEXAUTH_FAKE_HANG_LOGIN"    // never complete login
	fakeCrashRead    = "CODEXAUTH_FAKE_CRASH_READ"    // exit on the first account/read
	fakeIgnoreTerm   = "CODEXAUTH_FAKE_IGNORE_TERM"   // ignore SIGTERM
	fakeSameToken    = "CODEXAUTH_FAKE_SAME_TOKEN"    // refresh rewrites the same access token
	fakeAccount      = "CODEXAUTH_FAKE_ACCOUNT"       // account id written on refresh
	fakeRefreshSleep = "CODEXAUTH_FAKE_REFRESH_SLEEP" // delay each refresh
	// fakeRevokeOnRefresh models Codex after a permanent refresh failure:
	// account/read succeeds but reports no account (verified against 0.153.4
	// and 0.157.1 with a revoked refresh token).
	fakeRevokeOnRefresh = "CODEXAUTH_FAKE_REVOKE_ON_REFRESH"
	refreshSecret       = "rt-secret-never-surface"
)

// helperOwnerEnv makes the test binary an amux-like owner process: it starts
// a Provider over the fake server, prints the native pid, and blocks until
// killed. The parent-death test SIGKILLs it.
const helperOwnerEnv = "CODEXAUTH_HELPER_OWNER"

func TestMain(m *testing.M) {
	if os.Getenv(fakeEnv) == "1" {
		runFake()
		os.Exit(0)
	}
	if home := os.Getenv(helperOwnerEnv); home != "" {
		runHelperOwner(home)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeJWT(claims map[string]any) string {
	enc := base64.RawURLEncoding
	b, _ := json.Marshal(claims)
	return enc.EncodeToString([]byte(`{"alg":"none"}`)) + "." + enc.EncodeToString(b) + "." + enc.EncodeToString([]byte("sig"))
}

func writeAuth(home string, n int, account string, ttl time.Duration) {
	auth := map[string]any{"chatgpt_plan_type": "plus", "chatgpt_account_id": account}
	doc := map[string]any{
		"OPENAI_API_KEY": nil,
		"tokens": map[string]any{
			"id_token":      fakeJWT(map[string]any{"email": "user@example.com", "https://api.openai.com/auth": auth}),
			"access_token":  fakeJWT(map[string]any{"exp": time.Now().Add(ttl).Unix(), "n": n}),
			"refresh_token": fmt.Sprintf("%s-%d", refreshSecret, n),
			"account_id":    account,
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
	}
	b, _ := json.Marshal(doc)
	tmp := filepath.Join(home, ".auth.json.tmp")
	_ = os.WriteFile(tmp, b, 0o600)
	_ = os.Rename(tmp, filepath.Join(home, authFile))
}

func counter(home, name string) int {
	b, _ := os.ReadFile(filepath.Join(home, name))
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func bump(home, name string) int {
	n := counter(home, name) + 1
	_ = os.WriteFile(filepath.Join(home, name), []byte(strconv.Itoa(n)), 0o600)
	return n
}

func runFake() {
	home := os.Getenv("CODEX_HOME")
	if os.Getenv(fakeIgnoreTerm) == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	inv, _ := json.Marshal(map[string]any{"args": os.Args[1:], "env": os.Environ(), "pid": os.Getpid(), "cwd": must(os.Getwd())})
	_ = os.WriteFile(filepath.Join(home, "fake-invocation.json"), inv, 0o600)
	bump(home, "fake-starts")
	ttl := time.Hour
	if v := os.Getenv(fakeTTL); v != "" {
		ttl, _ = time.ParseDuration(v)
	}
	account := "acct-1"
	if v := os.Getenv(fakeAccount); v != "" {
		account = v
	}
	// A credential-looking stderr line that must arrive redacted.
	fmt.Fprintf(os.Stderr, "debug auth header: Bearer %s refresh=%s\n", fakeJWT(map[string]any{"x": 1}), refreshSecret+strings.Repeat("x", 40))

	out := json.NewEncoder(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<16), 1<<22)
	loginID := ""
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Method == "" {
			if string(m.ID) == `"srv-1"` && len(m.Error) > 0 {
				bump(home, "fake-server-request-answered")
			}
			continue
		}
		reply := func(result any) { _ = out.Encode(map[string]any{"id": m.ID, "result": result}) }
		switch m.Method {
		case "initialize":
			reply(map[string]any{"userAgent": "fake"})
			// Server-initiated request the central client must refuse.
			_ = out.Encode(map[string]any{"id": "srv-1", "method": "item/tool/requestUserInput", "params": map[string]any{}})
		case "initialized":
		case "account/read":
			if os.Getenv(fakeCrashRead) == "1" && counter(home, "fake-starts") == 1 {
				os.Exit(3)
			}
			var p struct {
				RefreshToken bool `json:"refreshToken"`
			}
			_ = json.Unmarshal(m.Params, &p)
			if _, err := os.Stat(filepath.Join(home, authFile)); err != nil {
				reply(map[string]any{"account": nil, "requiresOpenaiAuth": true})
				continue
			}
			if p.RefreshToken && os.Getenv(fakeRevokeOnRefresh) == "1" {
				reply(map[string]any{"account": nil, "requiresOpenaiAuth": true})
				continue
			}
			if p.RefreshToken {
				if d, _ := time.ParseDuration(os.Getenv(fakeRefreshSleep)); d > 0 {
					time.Sleep(d)
				}
				n := bump(home, "fake-refreshes")
				if os.Getenv(fakeSameToken) == "1" {
					n = 0
				}
				writeAuth(home, n, account, ttl)
			}
			reply(map[string]any{"account": map[string]any{"type": "chatgpt", "email": "user@example.com", "planType": "plus"}, "requiresOpenaiAuth": true})
		case "account/login/start":
			loginID = "login-1"
			reply(map[string]any{"type": "chatgpt", "loginId": loginID, "authUrl": "https://auth.example/authorize?state=s"})
			if os.Getenv(fakeHangLogin) != "1" {
				writeAuth(home, 0, account, ttl)
				_ = out.Encode(map[string]any{"method": "account/login/completed", "params": map[string]any{"loginId": "other", "success": false}})
				_ = out.Encode(map[string]any{"method": "account/login/completed", "params": map[string]any{"loginId": loginID, "success": true, "error": nil}})
			}
		case "account/login/cancel":
			bump(home, "fake-cancels")
			if os.Getenv("AMUX_FAKE_LATE_LOGIN") == "1" {
				writeAuth(home, 9, "late-account", ttl)
				_ = out.Encode(map[string]any{"method": "account/login/completed", "params": map[string]any{"loginId": loginID, "success": true}})
			}
			reply(map[string]any{})
		case "account/logout":
			_ = os.Remove(filepath.Join(home, authFile))
			reply(map[string]any{})
		default:
			_ = out.Encode(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "unknown method"}})
		}
	}
}

func must[T any](v T, _ error) T { return v }

func runHelperOwner(home string) {
	env := []string{fakeEnv + "=1", "PATH=" + os.Getenv("PATH")}
	p, err := New(Config{Argv: []string{os.Args[0]}, Home: home, Env: env})
	if err != nil {
		fmt.Println("ERR", err)
		return
	}
	if err := p.Start(context.Background()); err != nil {
		fmt.Println("ERR", err)
		return
	}
	fmt.Println("PID", p.srv.cmd.Process.Pid)
	select {}
}
