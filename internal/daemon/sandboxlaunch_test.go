package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/codexapp"
	"amux/internal/panespec"
	"amux/internal/store"
)

// TestSandboxedAppServerLaunch is the "actual sandboxed launch" acceptance the
// audit demanded: it does NOT assert on argv strings, it EXECUTES the
// bwrap-wrapped command panespec.AppServerCommand produces against a real
// `codex app-server` and proves the executable is reachable inside the sandbox and
// the endpoint is reachable across the sandbox's network namespace (the supervisor
// completes its handshake). Opt-in and self-skipping: needs AMUX_CODEX_APP_SERVER_SMOKE=1,
// a `codex` and `bwrap` on PATH, and the jail enabled.
func TestSandboxedAppServerLaunch(t *testing.T) {
	if os.Getenv("AMUX_CODEX_APP_SERVER_SMOKE") == "" {
		t.Skip("set AMUX_CODEX_APP_SERVER_SMOKE=1 (with codex+bwrap) to run the sandboxed launch test")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("codex not on PATH: %v", err)
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skipf("bwrap required for a real sandboxed launch: %v", err)
	}
	if v := os.Getenv("AMUX_JAIL"); v == "off" {
		t.Skip("AMUX_JAIL=off disables the sandbox under test")
	}

	// Isolated amux data + an isolated CODEX_HOME template source (no host creds).
	t.Setenv("AMUX_CODEX_BIN", bin)
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A codex session whose worktree is a real dir (rw-bound in the scope).
	wt := filepath.Join(data, "worktree")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	if err := db.PutSession(store.Session{ID: "sbx", Agent: "codex", Dir: wt}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	_ = db.Close()

	dir, env, argv, endpoint, err := panespec.AppServerCommand("sbx")
	if err != nil {
		t.Fatalf("AppServerCommand: %v", err)
	}
	t.Logf("endpoint: %s", endpoint)
	t.Logf("sandboxed argv: %s", strings.Join(argv, " "))

	// The endpoint must live inside the resolved launch dir's private .amux/ (which
	// scope binds read-write), NOT under $XDG_RUNTIME_DIR (/run, bound read-only).
	if !strings.HasPrefix(endpoint, "unix://"+dir+"/") {
		t.Fatalf("endpoint %q is not inside the bound launch dir %q", endpoint, dir)
	}
	// It must actually be sandboxed (bwrap-wrapped) and carry the real codex + the
	// endpoint — otherwise this proves nothing about OS isolation.
	if !strings.Contains(argv[0], "bwrap") {
		t.Fatalf("argv[0]=%q is not bwrap — the launch is not sandboxed", argv[0])
	}
	if !containsArg(argv, bin) || !containsArg(argv, endpoint) || !containsArg(argv, "app-server") {
		t.Fatalf("sandboxed argv missing codex/app-server/endpoint: %v", argv)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sup := codexapp.New(codexapp.Config{SessionID: "sbx", Dir: dir, Env: env, Endpoint: endpoint})
	if err := sup.Start(ctx, argv); err != nil {
		t.Fatalf("sandboxed launch (exec bwrap-wrapped codex + WS handshake): %v", err)
	}
	defer sup.Close()

	if id := sup.ThreadID(); id == "" {
		t.Fatal("no thread id: the app-server did not initialize inside the sandbox")
	} else {
		t.Logf("sandboxed app-server initialized inside bwrap; thread id = %s (executable + endpoint reachable in-scope)", id)
	}
}

func containsArg(argv []string, s string) bool {
	for _, a := range argv {
		if a == s {
			return true
		}
	}
	return false
}
