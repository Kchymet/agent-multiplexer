package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"amux/internal/access"
	"amux/internal/claudecfg"
	"amux/internal/credentialbroker"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

func TestGitHubBrokerAllHarnessesReadOnly(t *testing.T) {
	claude := store.Session{ID: "claude", Agent: "claude", Dir: t.TempDir()}
	codex := store.Session{ID: "codex", Agent: "codex", Dir: t.TempDir()}
	archived := store.Session{ID: "archived", Agent: "codex", Dir: t.TempDir(), Archived: true}
	d, r, principals := sessionRuntimeFixture(t, claude, codex, archived)
	dir := t.TempDir()
	t.Setenv("GH_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte("github.com:\n  user: selected\n"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.credentialOperation = func(_ context.Context, o credentialbroker.Operation) (credentialbroker.Result, error) {
		calls++
		return credentialbroker.Result{Value: "synthetic"}, nil
	}
	o := credentialbroker.Operation{Verb: credentialbroker.Read, Service: "gh:github.com", Account: credentialbroker.GitHubAccount}
	call := sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: o.Verb, Fields: o.Fields()}
	ctx := context.Background()
	for _, id := range []string{claude.ID, codex.ID} {
		res, _ := r.dispatch(ctx, sessionrpc.DispatchRequest{Principal: principals[id], Call: call})
		if res.Status != sessionrpc.StatusOK {
			t.Fatal("active session denied GitHub credentials")
		}
	}
	for _, tc := range []struct {
		id string
		op credentialbroker.Operation
	}{
		{archived.ID, o},
		{claude.ID, credentialbroker.Operation{Verb: credentialbroker.Read, Service: o.Service, Account: "other-user"}},
		{codex.ID, credentialbroker.Operation{Verb: credentialbroker.Read, Service: "gh:unconfigured.example", Account: o.Account}},
		{claude.ID, credentialbroker.Operation{Verb: credentialbroker.Write, Service: o.Service, Account: o.Account, Value: "synthetic"}},
		{codex.ID, credentialbroker.Operation{Verb: credentialbroker.Delete, Service: o.Service, Account: o.Account}},
	} {
		bad := call
		bad.Verb, bad.Fields = tc.op.Verb, tc.op.Fields()
		if tc.op.Verb != credentialbroker.Read {
			bad.Route = access.RouteAction
		}
		res, _ := r.dispatch(ctx, sessionrpc.DispatchRequest{Principal: principals[tc.id], Call: bad})
		if res.Status != sessionrpc.StatusDenied || calls != 2 {
			t.Fatal("unauthorized GitHub operation reached backend")
		}
	}
	if err := d.authority.Revoke(ctx, access.SubjectSession, codex.ID); err != nil {
		t.Fatal(err)
	}
	res, _ := r.dispatch(ctx, sessionrpc.DispatchRequest{Principal: principals[codex.ID], Call: call})
	if res.Status != sessionrpc.StatusDenied || calls != 2 {
		t.Fatal("revoked session reached GitHub credentials")
	}
}

func TestCredentialBrokerRestrictsHarnessAccountAndOperations(t *testing.T) {
	claude := store.Session{ID: "claude", Agent: "claude", Dir: t.TempDir()}
	codex := store.Session{ID: "codex", Agent: "codex", Dir: t.TempDir()}
	d, r, principals := sessionRuntimeFixture(t, claude, codex)
	ctx := context.Background()
	selected := credentialbroker.ClaudeServices(claudecfg.CredentialSelector())[1]
	o := credentialbroker.Operation{Verb: credentialbroker.Read, Service: selected, Account: credentialbroker.HostAccount()}
	call := sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteQuery, Verb: o.Verb, Fields: o.Fields()}
	value := "synthetic host token"
	calls := 0
	r.credentialOperation = func(_ context.Context, op credentialbroker.Operation) (credentialbroker.Result, error) {
		calls++
		switch op.Verb {
		case credentialbroker.Read:
			return credentialbroker.Result{Value: value}, nil
		case credentialbroker.Write:
			value = op.Value
		case credentialbroker.Delete:
			value = ""
		}
		return credentialbroker.Result{}, nil
	}
	for _, tc := range []struct {
		name      string
		mutate    func(*sessionrpc.Call)
		principal access.Principal
	}{
		{"other harness", func(*sessionrpc.Call) {}, principals[codex.ID]},
		{"other account", func(c *sessionrpc.Call) { c.Fields["account"] = "other" }, principals[claude.ID]},
		{"other service", func(c *sessionrpc.Call) { c.Fields["service"] = "unrelated-password" }, principals[claude.ID]},
		{"other selector", func(c *sessionrpc.Call) { c.Fields["service"] = "Claude Code-credentials-other" }, principals[claude.ID]},
		{"other session", func(c *sessionrpc.Call) { c.ID = codex.ID }, principals[claude.ID]},
		{"keychain path", func(c *sessionrpc.Call) { c.Fields["path"] = "/host/keychain" }, principals[claude.ID]},
		{"write via query", func(c *sessionrpc.Call) { c.Verb = credentialbroker.Write; c.Fields["value"] = "synthetic" }, principals[claude.ID]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := call
			bad.Fields = o.Fields()
			tc.mutate(&bad)
			if err := r.authorize(ctx, tc.principal, bad); err == nil {
				t.Fatal("unauthorized broker operation admitted")
			}
			res, _ := r.dispatch(ctx, sessionrpc.DispatchRequest{Principal: tc.principal, Call: bad})
			if res.Status != sessionrpc.StatusDenied || calls != 0 {
				t.Fatal("unauthorized operation reached backend")
			}
		})
	}
	for _, verb := range []string{credentialbroker.Read, credentialbroker.Write, credentialbroker.Read, credentialbroker.Delete} {
		o.Verb = verb
		o.Value = "rotated synthetic token"
		call.Verb, call.Fields = verb, o.Fields()
		call.Route = access.RouteAction
		if verb == credentialbroker.Read {
			call.Route = access.RouteQuery
		}
		if err := r.authorize(ctx, principals[claude.ID], call); err != nil {
			t.Fatal(err)
		}
		res, err := r.dispatch(ctx, sessionrpc.DispatchRequest{Principal: principals[claude.ID], Call: call})
		if err != nil || res.Status != sessionrpc.StatusOK {
			t.Fatalf("broker operation failed: %v", err)
		}
		if verb == credentialbroker.Read {
			var result credentialbroker.Result
			if json.Unmarshal(res.Body, &result) != nil || result.Value != value {
				t.Fatal("broker did not read current shared credentials")
			}
		}
	}
	if calls != 4 {
		t.Fatal("unexpected broker calls")
	}
	if err := d.authority.Revoke(ctx, access.SubjectSession, claude.ID); err != nil {
		t.Fatal(err)
	}
	res, _ := r.dispatch(ctx, sessionrpc.DispatchRequest{Principal: principals[claude.ID], Call: call})
	if res.Status != sessionrpc.StatusDenied || calls != 4 {
		t.Fatal("revoked session reached credentials")
	}
}
