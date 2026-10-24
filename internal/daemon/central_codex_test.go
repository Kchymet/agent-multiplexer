package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/amuxcfg"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/store"
)

func centralCodexDaemon(t *testing.T, sessions ...store.Session) *Daemon {
	t.Helper()
	d, _, _ := sessionRuntimeFixture(t, sessions...)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	d.codex = codexapp.NewManager(ctx, "")
	t.Cleanup(d.codex.Shutdown)
	return d
}

func TestCentralCodexOnlyStartsInCentralMode(t *testing.T) {
	d := centralCodexDaemon(t)
	for _, mode := range []string{amuxcfg.AuthHost, amuxcfg.AuthEnvironment} {
		if err := amuxcfg.SetAuthMode("codex", mode); err != nil {
			t.Fatal(err)
		}
		d.startCentralCodex(t.Context())
		if d.centralCodex != nil {
			t.Fatalf("%s mode started central Codex auth", mode)
		}
		if err := d.codexLogin(core.Action{Action: core.ActionCodexLogin}); err == nil || !strings.Contains(err.Error(), "codex.auth central") {
			t.Fatalf("%s mode login = %v", mode, err)
		}
		if st := d.codexAuthStatus(t.Context()); st.Mode != mode || st.Error != "" {
			t.Fatalf("%s status = %+v", mode, st)
		}
	}
}

func TestCentralCodexWithoutExecutableRefusesInsteadOfFallingBack(t *testing.T) {
	codex := store.Session{ID: "cx", Agent: "codex", Dir: t.TempDir()}
	d := centralCodexDaemon(t, codex)
	t.Setenv("AMUX_CODEX_BIN", filepath.Join(t.TempDir(), "missing-codex"))
	d.startCentralCodex(t.Context()) // central is the default
	if d.centralCodex == nil || d.centralCodex.provider != nil || d.centralCodex.err == nil {
		t.Fatalf("central Codex without an executable = %+v", d.centralCodex)
	}
	_, err := codexTokenSource{d: d}.CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: codex.ID})
	if err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("token without provider = %v", err)
	}
	if err := d.codexLogin(core.Action{Action: core.ActionCodexLogin}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("login without provider = %v", err)
	}
	if st := d.codexAuthStatus(t.Context()); st.Mode != amuxcfg.AuthCentral || st.Error == "" {
		t.Fatalf("status without provider = %+v", st)
	}
	d.closeCodex(codex.ID) // nil-safe without a provider
	d.closeCentralCodex()
}

func TestCentralCodexTokenSourceAuthorizesSessionFirst(t *testing.T) {
	claude := store.Session{ID: "cl", Agent: "claude", Dir: t.TempDir()}
	archived := store.Session{ID: "old", Agent: "codex", Dir: t.TempDir(), Archived: true}
	d := centralCodexDaemon(t, claude, archived)
	// A provider-less central owner: any request reaching it would report
	// "unavailable", so a different error proves authorization ran first.
	d.centralCodex = &centralCodex{lifetime: t.Context()}
	for _, id := range []string{claude.ID, archived.ID, "unknown"} {
		_, err := codexTokenSource{d: d}.CodexChatGPTToken(t.Context(), codexapp.TokenRequest{SessionID: id})
		if err == nil || !strings.Contains(err.Error(), "not authorized") {
			t.Fatalf("%s token = %v", id, err)
		}
	}
}

func TestCodexLoginValidatesFields(t *testing.T) {
	d := centralCodexDaemon(t)
	for _, a := range []core.Action{
		{Action: core.ActionCodexLogin, Fields: map[string]string{"cancel": "yes"}},
		{Action: core.ActionCodexLogin, Fields: map[string]string{"account": "other"}},
		{Action: core.ActionCodexLogin, ID: "cx"},
	} {
		if err := d.codexLogin(a); err == nil {
			t.Fatalf("accepted %+v", a)
		}
	}
}
