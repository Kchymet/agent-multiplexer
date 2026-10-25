package daemon

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/core"
)

// fakeCodexEnv makes this test binary act as a native `codex app-server` for
// the daemon's real codexauth.Provider. fakeCodexLogin selects the login
// outcome: "success", "fail" or "hang" (until cancelled).
const (
	fakeCodexEnv   = "AMUX_DAEMON_TEST_FAKE_CODEX"
	fakeCodexLogin = "AMUX_DAEMON_TEST_FAKE_CODEX_LOGIN"
)

func init() {
	if os.Getenv(fakeCodexEnv) == "1" {
		runFakeCodexAppServer()
		os.Exit(0)
	}
}

func runFakeCodexAppServer() {
	home := os.Getenv("CODEX_HOME")
	loggedIn := filepath.Join(home, "fake-logged-in")
	out := json.NewEncoder(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.Method == "" {
			continue
		}
		reply := func(result any) { _ = out.Encode(map[string]any{"id": m.ID, "result": result}) }
		switch m.Method {
		case "initialize":
			reply(map[string]any{"userAgent": "fake"})
		case "initialized":
		case "account/read":
			if _, err := os.Stat(loggedIn); err != nil {
				reply(map[string]any{"account": nil, "requiresOpenaiAuth": true})
				continue
			}
			reply(map[string]any{"account": map[string]any{"type": "chatgpt", "email": "user@example.com", "planType": "plus"}, "requiresOpenaiAuth": true})
		case "account/login/start":
			reply(map[string]any{"type": "chatgpt", "loginId": "login-1", "authUrl": "https://auth.example/authorize?state=fake"})
			switch os.Getenv(fakeCodexLogin) {
			case "success":
				_ = os.WriteFile(loggedIn, nil, 0o600)
				_ = out.Encode(map[string]any{"method": "account/login/completed", "params": map[string]any{"loginId": "login-1", "success": true}})
			case "fail":
				_ = out.Encode(map[string]any{"method": "account/login/completed", "params": map[string]any{"loginId": "login-1", "success": false, "error": "access denied"}})
			}
		case "account/login/cancel":
			_ = os.WriteFile(filepath.Join(home, "fake-cancelled"), nil, 0o600)
			reply(map[string]any{})
		default:
			_ = out.Encode(map[string]any{"id": m.ID, "error": map[string]any{"code": -32601, "message": "unknown method"}})
		}
	}
}

func fakeCodexDaemon(t *testing.T, outcome string) *Daemon {
	t.Helper()
	d := centralCodexDaemon(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUX_CODEX_BIN", self)
	t.Setenv(fakeCodexEnv, "1")
	t.Setenv(fakeCodexLogin, outcome)
	d.startCentralCodex(t.Context())
	if d.centralCodex == nil || d.centralCodex.provider == nil {
		t.Fatalf("central Codex did not start: %+v", d.centralCodex)
	}
	t.Cleanup(d.closeCentralCodex)
	return d
}

func waitCodexLogin(t *testing.T, d *Daemon, state string) core.CodexAuthStatus {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := d.codexAuthStatus(t.Context())
		if st.Login.State == state {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("login state = %+v, want %s", st, state)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCentralCodexLoginSucceedsThroughDaemonOwnedProvider(t *testing.T) {
	d := fakeCodexDaemon(t, "success")
	if st := d.codexAuthStatus(t.Context()); st.Account.Type != "" || st.Error != "" {
		t.Fatalf("status before login = %+v", st)
	}
	if err := d.codexLogin(core.Action{Action: core.ActionCodexLogin}); err != nil {
		t.Fatal(err)
	}
	st := waitCodexLogin(t, d, "succeeded")
	if st.Account.Type != "chatgpt" || st.Account.Email != "user@example.com" || st.Account.PlanType != "plus" {
		t.Fatalf("status after login = %+v", st)
	}
	if st.Login.URL != "" {
		t.Fatalf("completed login still exposes its URL: %+v", st)
	}
}

func TestCentralCodexLoginReportsFailure(t *testing.T) {
	d := fakeCodexDaemon(t, "fail")
	if err := d.codexLogin(core.Action{Action: core.ActionCodexLogin}); err != nil {
		t.Fatal(err)
	}
	st := waitCodexLogin(t, d, "failed")
	if st.Login.Error == "" || st.Account.Type != "" {
		t.Fatalf("failed login status = %+v", st)
	}
}

func TestCentralCodexLoginShowsURLAndCancels(t *testing.T) {
	d := fakeCodexDaemon(t, "hang")
	if err := d.codexLogin(core.Action{Action: core.ActionCodexLogin}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		st := d.codexAuthStatus(t.Context())
		if st.Login.State == "pending" && st.Login.URL == "https://auth.example/authorize?state=fake" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending login never showed its URL: %+v", st)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// A retried CLI joins the pending login instead of starting another.
	if err := d.codexLogin(core.Action{Action: core.ActionCodexLogin}); err != nil {
		t.Fatal(err)
	}
	if err := d.codexLogin(core.Action{Action: core.ActionCodexLogin, Fields: map[string]string{"cancel": "true"}}); err != nil {
		t.Fatal(err)
	}
	waitCodexLogin(t, d, "cancelled")
}
