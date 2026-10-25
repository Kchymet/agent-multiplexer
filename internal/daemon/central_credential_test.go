package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/amuxcfg"
	"amux/internal/centralauth"
	"amux/internal/claudecfg"
	"amux/internal/credentialbroker"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

const mockCentralToken = "mock-central-token-for-daemon-tests"

func centralCall() sessionrpc.Call {
	return sessionrpc.Call{
		Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: centralauth.QueryVerb,
		Fields: map[string]string{centralauth.HarnessField: string(centralauth.Claude)},
	}
}

func TestCentralCredentialDeliveredOnlyToAuthorizedClaudeSessions(t *testing.T) {
	claude := store.Session{ID: "claude", Agent: "claude", Dir: t.TempDir()}
	codex := store.Session{ID: "codex", Agent: "codex", Dir: t.TempDir()}
	archived := store.Session{ID: "archived", Agent: "claude", Dir: t.TempDir(), Archived: true}
	d, r, principals := sessionRuntimeFixture(t, claude, codex, archived)
	ctx := context.Background()
	dispatch := func(p access.Principal, c sessionrpc.Call) sessionrpc.DispatchResult {
		t.Helper()
		res, err := r.dispatch(ctx, sessionrpc.DispatchRequest{Principal: p, Call: c})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(res.Body)+res.Code, "/") {
			t.Fatalf("response exposes a path: %+v", res)
		}
		return res
	}

	// Central is the default; before a host login there is nothing to deliver.
	if res := dispatch(principals[claude.ID], centralCall()); res.Status != sessionrpc.StatusFailed || res.Code != "central_login_required" {
		t.Fatalf("unconfigured = %+v", res)
	}
	tok, _ := centralauth.NewSecret(mockCentralToken)
	if _, err := centralauth.ClaudeStore().Save(tok, time.Now()); err != nil {
		t.Fatal(err)
	}
	res := dispatch(principals[claude.ID], centralCall())
	var got centralauth.Delivery
	if res.Status != sessionrpc.StatusOK || json.Unmarshal(res.Body, &got) != nil || got.Token != mockCentralToken || got.Generation != 1 {
		t.Fatalf("authorized delivery = %+v", res)
	}

	// A present but insecure credential is not evidence that login is missing.
	path := filepath.Join(centralauth.ClaudeStore().Dir, "credential.json")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if res := dispatch(principals[claude.ID], centralCall()); res.Code != "central_credential_unavailable" || len(res.Body) != 0 {
		t.Fatalf("unreadable store = %+v", res)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		principal access.Principal
		mutate    func(*sessionrpc.Call)
	}{
		{"codex session", principals[codex.ID], func(*sessionrpc.Call) {}},
		{"archived session", principals[archived.ID], func(*sessionrpc.Call) {}},
		{"other harness", principals[claude.ID], func(c *sessionrpc.Call) { c.Fields[centralauth.HarnessField] = "codex" }},
		{"extra field", principals[claude.ID], func(c *sessionrpc.Call) { c.Fields["session"] = codex.ID }},
		{"named session", principals[claude.ID], func(c *sessionrpc.Call) { c.ID = codex.ID }},
		{"named target", principals[claude.ID], func(c *sessionrpc.Call) { c.Target = codex.ID }},
		{"action route", principals[claude.ID], func(c *sessionrpc.Call) { c.Route = access.RouteAction }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := centralCall()
			tc.mutate(&c)
			if err := r.authorize(ctx, tc.principal, c); err == nil {
				t.Fatal("unauthorized central credential request admitted")
			}
			if res := dispatch(tc.principal, c); res.Status != sessionrpc.StatusDenied || len(res.Body) != 0 {
				t.Fatalf("unauthorized request = %+v", res)
			}
		})
	}

	// A settings layer that selects another method blocks delivery.
	project := claudeProjectSettings(t, claude.Dir, `{"apiKeyHelper":"/bin/echo synthetic"}`)
	if res := dispatch(principals[claude.ID], centralCall()); res.Status != sessionrpc.StatusDenied || res.Code != "central_credential_conflict" {
		t.Fatalf("conflicting settings delivery = %+v", res)
	}
	if err := os.Remove(project); err != nil {
		t.Fatal(err)
	}

	// A later global mode change must not change this workload's pinned source.
	for _, mode := range []string{amuxcfg.AuthHost, amuxcfg.AuthEnvironment} {
		if err := amuxcfg.SetAuthMode("claude", mode); err != nil {
			t.Fatal(err)
		}
		if res := dispatch(principals[claude.ID], centralCall()); res.Status != sessionrpc.StatusOK {
			t.Fatalf("%s mode changed existing central delivery = %+v", mode, res)
		}
	}
	if err := amuxcfg.SetAuthMode("claude", ""); err != nil {
		t.Fatal(err)
	}
	if err := d.authority.Revoke(ctx, access.SubjectSession, claude.ID); err != nil {
		t.Fatal(err)
	}
	if res := dispatch(principals[claude.ID], centralCall()); res.Status != sessionrpc.StatusDenied || len(res.Body) != 0 {
		t.Fatalf("revoked session delivery = %+v", res)
	}
}

func TestCentralModeRefusesLegacyClaudeKeychainBroker(t *testing.T) {
	claude := store.Session{ID: "claude", Agent: "claude", Dir: t.TempDir()}
	_, r, principals := sessionRuntimeFixture(t, claude)
	ctx := context.Background()
	r.credentialOperation = func(context.Context, credentialbroker.Operation) (credentialbroker.Result, error) {
		t.Fatal("central mode reached the host Keychain")
		return credentialbroker.Result{}, nil
	}
	for _, mode := range []string{amuxcfg.AuthCentral, amuxcfg.AuthEnvironment} {
		if err := amuxcfg.SetAuthMode("claude", mode); err != nil {
			t.Fatal(err)
		}
		for _, verb := range []string{credentialbroker.Read, credentialbroker.Write, credentialbroker.Delete} {
			o := credentialbroker.Operation{
				Verb: verb, Service: credentialbroker.ClaudeServices(claudecfg.CredentialSelector())[1],
				Account: credentialbroker.HostAccount(), Value: "synthetic",
			}
			route := access.RouteAction
			if verb == credentialbroker.Read {
				route = access.RouteQuery
			}
			c := sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: route, Verb: verb, Fields: o.Fields()}
			res, _ := r.dispatch(ctx, sessionrpc.DispatchRequest{Principal: principals[claude.ID], Call: c})
			if res.Status != sessionrpc.StatusDenied {
				t.Fatalf("%s %s = %+v", mode, verb, res)
			}
		}
	}
}

func claudeProjectSettings(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
