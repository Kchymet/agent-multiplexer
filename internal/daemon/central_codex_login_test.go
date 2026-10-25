package daemon

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
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
			reply(map[string]any{"type": "chatgpt", "loginId": "login-1", "authUrl": "https://auth.openai.com/authorize?state=fake"})
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

func TestCentralCodexLoginCompletesOnProtectedConnection(t *testing.T) {
	for _, outcome := range []string{"success", "fail"} {
		t.Run(outcome, func(t *testing.T) {
			d := fakeCodexDaemon(t, outcome)
			r := hp.AccountRequest{Operation: hp.AccountLogin, ProfileID: "codex:central", Revision: profileRevision(t, "codex:central"), Initiator: "owner"}
			out := d.accountCall(t.Context(), r, true)
			if !out.OK || out.Ceremony == nil {
				t.Fatal(out)
			}
			r.Operation = hp.AccountPoll
			r.Revision = 0
			r.AttemptID = out.Ceremony.AttemptID
			deadline := time.Now().Add(5 * time.Second)
			for {
				out = d.accountCall(t.Context(), r, true)
				if out.Ceremony.State != "pending" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("pending native login")
				}
				time.Sleep(10 * time.Millisecond)
			}
			want := "succeeded"
			if outcome == "fail" {
				want = "failed"
			}
			if out.Ceremony.State != want || out.Ceremony.AuthURL != "" {
				t.Fatal(out)
			}
			if outcome == "success" {
				st := d.codexAuthStatus(t.Context())
				if st.Account.Type != "chatgpt" || st.Login.URL != "" {
					t.Fatal(st)
				}
			}
		})
	}
}

func TestCentralCodexLoginURLIsInitiatorOnlyAndCancellationObservable(t *testing.T) {
	d := fakeCodexDaemon(t, "hang")
	r := hp.AccountRequest{Operation: hp.AccountLogin, ProfileID: "codex:central", Revision: profileRevision(t, "codex:central"), Initiator: "owning-connection"}
	out := d.accountCall(t.Context(), r, true)
	if !out.OK || out.Ceremony == nil {
		t.Fatal(out)
	}
	r.Operation = hp.AccountPoll
	r.Revision = 0
	r.AttemptID = out.Ceremony.AttemptID
	deadline := time.Now().Add(5 * time.Second)
	for {
		out = d.accountCall(t.Context(), r, true)
		if out.Ceremony != nil && out.Ceremony.AuthURL != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no protected ceremony URL")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := d.codexAuthStatus(t.Context()); st.Login.URL != "" {
		t.Fatal("legacy status leaked URL")
	}
	other := r
	other.Initiator = "other-connection"
	if d.accountCall(t.Context(), other, true).OK {
		t.Fatal("other connection polled URL")
	}
	r.Operation = hp.AccountCancel
	if out = d.accountCall(t.Context(), r, true); !out.OK || out.Ceremony.State != "cancelled" {
		t.Fatal(out)
	}
	r.Operation = hp.AccountPoll
	if out = d.accountCall(t.Context(), r, true); !out.OK || out.Ceremony.State != "cancelled" || out.Ceremony.AuthURL != "" {
		t.Fatal(out)
	}
}
