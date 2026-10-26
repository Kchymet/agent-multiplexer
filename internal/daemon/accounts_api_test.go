package daemon

import (
	"amux/internal/accounts"
	"amux/internal/centralauth"
	"amux/internal/codexapp"
	"amux/internal/codexauth"
	"amux/internal/sessionrpc"
	"amux/internal/store"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

func loginAPIForTest(t *testing.T, d *Daemon, harness, key string) {
	t.Helper()
	id := harness + ":central"
	out := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountLogin, Mode: "api", ProfileID: id, Revision: profileRevision(t, id), Initiator: "api-test"}, true)
	if !out.OK || out.Ceremony == nil || out.Ceremony.Method != "api-key" || out.Ceremony.Mode != "api" {
		t.Fatalf("API start: %+v", out)
	}
	r := hp.AccountRequest{Operation: hp.AccountComplete, ProfileID: id, AttemptID: out.Ceremony.AttemptID, Initiator: "api-test", Secret: key}
	out = d.accountCall(t.Context(), r, true)
	if !out.OK {
		t.Fatalf("API complete: %+v", out)
	}
	r.Secret = ""
	r.Operation = hp.AccountPoll
	deadline := time.Now().Add(10 * time.Second)
	for out.Ceremony.State == "pending" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		out = d.accountCall(t.Context(), r, true)
		if !out.OK {
			t.Fatal(out)
		}
	}
	if out.Ceremony.State != "succeeded" {
		t.Fatalf("API login did not succeed: %+v", out.Ceremony)
	}
}

func TestClaudeAPIReplacementServesOnlyTheNextLaunch(t *testing.T) {
	d := isolatedAccounts(t)
	if err := accounts.Bind("new", "claude"); err != nil {
		t.Fatal(err)
	}
	running, err := accounts.BeginLaunch("new", "claude")
	if err != nil {
		t.Fatal(err)
	}
	loginAPIForTest(t, d, "claude", "SYNTHETIC-FIRST-KEY")
	cred, err := centralauth.ClaudeStore().Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := acceptClaude("new", running, cred.Generation); err != nil {
		t.Fatal("first API login refused", err)
	}
	if err := accounts.ObserveClaude("new", "runtime", []byte(`{"five_hour":{"used_percentage":42}}`)); err == nil {
		t.Fatal("API key accepted subscription usage")
	}
	loginAPIForTest(t, d, "claude", "SYNTHETIC-SECOND-KEY")
	cred, err = centralauth.ClaudeStore().Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := acceptClaude("new", running, cred.Generation); !errors.Is(err, accounts.ErrSuperseded) {
		t.Fatal("running process adopted replacement API key", err)
	}
	next, _ := accounts.BeginLaunch("new", "claude")
	if err := acceptClaude("new", next, cred.Generation); err != nil {
		t.Fatal("next launch refused the replacement API key", err)
	}
	snapshot, err := d.accountSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range snapshot.ManagedOnly().Profiles {
		if p.Harness == "claude" && (p.Mode != "api" || p.Limits.State != "not-applicable" || len(p.Limits.Windows) != 0 || p.Identity.AccountID != "") {
			t.Fatal("API projection invented subscription identity or usage")
		}
	}
}

func TestSmokeCentralAPIKeyBindingNative(t *testing.T) {
	bin := os.Getenv("AMUX_TEST_CODEX_BIN")
	if bin == "" {
		t.Skip("set AMUX_TEST_CODEX_BIN to CI-pinned Codex 0.157.1")
	}
	row := store.Session{ID: "api-workload", Agent: "codex", Dir: t.TempDir()}
	d, _, _ := sessionRuntimeFixture(t, row)
	root := t.TempDir()
	native, err := codexauth.New(codexauth.Config{Argv: []string{bin}, Home: filepath.Join(root, "central"), Env: []string{"HOME=" + root, "PATH=/usr/bin:/bin", "XDG_STATE_HOME=" + root}})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	d.centralCodex = &centralCodex{provider: native, lifetime: t.Context()}
	defer d.disconnectAccounts("", true)
	loginAPIForTest(t, d, "codex", "sk-synthetic-FIRST-SECRET")
	source := codexTokenSource{d: d}
	first, err := accounts.BeginLaunch(row.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := source.CodexChatGPTToken(context.Background(), codexapp.TokenRequest{SessionID: row.ID, Launch: first})
	if err != nil || !tok.APIKey || tok.AccountID != "" || tok.PlanType != "" {
		t.Fatal("first authorized API delivery failed/invented identity", err)
	}
	loginAPIForTest(t, d, "codex", "sk-synthetic-SECOND-SECRET")
	if _, err := source.CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: row.ID, Launch: first}); err == nil {
		t.Fatal("running process adopted the native replacement")
	}
	next, err := accounts.BeginLaunch(row.ID, "codex")
	if err != nil {
		t.Fatal(err)
	}
	tok, err = source.CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: row.ID, Launch: next})
	if err != nil || tok.AccessToken != "sk-synthetic-SECOND-SECRET" {
		t.Fatal("next launch did not receive the replacement", err)
	}
	t.Log("Native API ceremony, first delivery, running-process refusal and next-launch replacement passed without model calls")
}

func TestClaudeAPIDeliveryRequiresTypedLauncher(t *testing.T) {
	row := store.Session{ID: "typed", Agent: "claude", Dir: t.TempDir()}
	d, r, principals := sessionRuntimeFixture(t, row)
	defer d.disconnectAccounts("", true)
	loginAPIForTest(t, d, "claude", "synthetic-typed-key")
	call := launchCall(t, row.ID)
	untyped := centralCall()
	untyped.Fields[centralauth.LaunchField] = call.Fields[centralauth.LaunchField]
	out, err := r.dispatch(t.Context(), sessionrpc.DispatchRequest{Principal: principals[row.ID], Call: untyped})
	if err != nil || out.Code != "credential_type_unsupported" || len(out.Body) != 0 {
		t.Fatal("old launcher received API material", err)
	}
	out, err = r.dispatch(t.Context(), sessionrpc.DispatchRequest{Principal: principals[row.ID], Call: call})
	var delivery centralauth.Delivery
	if err != nil || out.Status != sessionrpc.StatusOK || json.Unmarshal(out.Body, &delivery) != nil || delivery.Kind != centralauth.KindClaudeAPIKey || delivery.Token != "synthetic-typed-key" {
		t.Fatal("typed delivery failed", err)
	}
}
