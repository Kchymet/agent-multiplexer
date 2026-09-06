package daemon

import (
	"context"
	"io"
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

	// The endpoint is outside shared worktrees and only its own parent gets a
	// writable bind after the dedicated socket root is masked.
	expected, err := panespec.AppServerEndpoint("sbx")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != expected || strings.HasPrefix(endpoint, "unix://"+dir+"/") {
		t.Fatalf("unexpected private endpoint: got %q expected %q outside %q", endpoint, expected, dir)
	}
	socketDir := filepath.Dir(strings.TrimPrefix(endpoint, "unix://"))
	if !containsArg(argv, socketDir) {
		t.Fatalf("own socket directory not mounted: %v", argv)
	}

	// It must actually be sandboxed (bwrap-wrapped) and carry the real codex + the
	// endpoint — otherwise this proves nothing about OS isolation.
	if !strings.Contains(argv[0], "bwrap") {
		t.Fatalf("argv[0]=%q is not bwrap — the launch is not sandboxed", argv[0])
	}
	resolved, resolveErr := filepath.EvalSymlinks(bin)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if (!containsArg(argv, bin) && !containsArg(argv, resolved)) || !containsArg(argv, endpoint) || !containsArg(argv, "app-server") {
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

// TestSandboxedSymlinkedCodexLaunch proves the provisioning fix against the real
// binary in a PRODUCTION-SHAPED layout: AMUX_CODEX_BIN is a launcher SYMLINK
// (~/.local/bin/codex-style) whose target lives in a different home subtree
// (~/.codex/packages/standalone/<ver>/bin/codex) — not the audit-reference path.
// The scope must bind the resolved package root (narrowly, never all of ~/.codex)
// and exec the resolved real path; an actual bwrap launch then completes the
// handshake. Opt-in + self-skipping (needs codex + bwrap).
func TestSandboxedSymlinkedCodexLaunch(t *testing.T) {
	if os.Getenv("AMUX_CODEX_APP_SERVER_SMOKE") == "" {
		t.Skip("set AMUX_CODEX_APP_SERVER_SMOKE=1 (with codex+bwrap) to run")
	}
	realCodex, err := exec.LookPath("codex")
	if err != nil {
		t.Skipf("codex not on PATH: %v", err)
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skipf("bwrap required: %v", err)
	}
	if os.Getenv("AMUX_JAIL") == "off" {
		t.Skip("AMUX_JAIL=off disables the sandbox under test")
	}
	// Resolve the real runnable ELF (the codex on PATH may itself be a symlink).
	realCodex, err = filepath.EvalSymlinks(realCodex)
	if err != nil {
		t.Fatal(err)
	}

	// A temporary home keeps all install/config fixtures separate from the host.
	fakeHome, err := os.MkdirTemp("", "amux-cxhome-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fakeHome) })
	t.Setenv("HOME", fakeHome)

	pkgRoot := filepath.Join(fakeHome, ".codex", "packages", "standalone", "releases", "0.153.4")
	pkgBin := filepath.Join(pkgRoot, "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(pkgBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(realCodex, pkgBin); err != nil {
		source, err := os.Open(realCodex)
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		target, err := os.OpenFile(pkgBin, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0755)
		if err != nil {
			t.Fatal(err)
		}
		_, copyErr := io.Copy(target, source)
		closeErr := target.Close()
		if copyErr != nil {
			t.Fatal(copyErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	launcherDir := filepath.Join(fakeHome, ".local", "bin")
	if err := os.MkdirAll(launcherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(launcherDir, "codex")
	current := filepath.Join(fakeHome, ".codex", "packages", "standalone", "current")
	if err := os.Symlink(pkgRoot, current); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(current, "bin", "codex"), launcher); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUX_CODEX_BIN", launcher) // production-shaped: a launcher symlink, NOT the audit-reference path

	// Isolated amux data + CODEX_HOME template.
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("\n"), 0600); err != nil {
		t.Fatal(err)
	}

	wt := filepath.Join(data, "worktree")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := db.PutSession(store.Session{ID: "sym", Agent: "codex", Dir: wt}); err != nil {
		t.Fatalf("put session: %v", err)
	}
	_ = db.Close()

	dir, env, argv, endpoint, err := panespec.AppServerCommand("sym")
	if err != nil {
		t.Fatalf("AppServerCommand: %v", err)
	}
	joined := strings.Join(argv, " ")
	t.Logf("sandboxed argv: %s", joined)

	// The resolved package root must be bound; the whole ~/.codex must NOT be.
	if !containsArg(argv, pkgRoot) {
		t.Fatalf("argv does not bind the resolved package root %q", pkgRoot)
	}
	if containsArg(argv, filepath.Join(fakeHome, ".codex")) {
		t.Fatal("argv binds the whole ~/.codex — must stay narrow to the package")
	}
	// It must execute the resolved package binary, and stay inside bubblewrap.
	if !strings.Contains(argv[0], "bwrap") {
		t.Fatalf("not sandboxed: %v", argv)
	}
	separator := -1
	for i, arg := range argv {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || len(argv[separator+1:]) != 4 || argv[separator+1] != pkgBin {
		t.Fatalf("unexpected sandbox command suffix: %v", argv)
	}
	// Check visibility and write restrictions behaviorally in that exact namespace,
	// with only the final command replaced by a shell probe. No real credentials.
	hidden := filepath.Join(fakeHome, ".codex", "private-host-state")
	sibling := filepath.Join(fakeHome, ".codex", "packages", "standalone", "other-release")
	localOther := filepath.Join(fakeHome, ".local", "unrelated-state")
	resource := filepath.Join(pkgRoot, "package-resource")
	for _, path := range []string{hidden, sibling, localOther, resource} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	probeArgv := append([]string{}, argv[:separator+1]...)
	probeArgv = append(probeArgv, "/bin/sh", "-ceu", `
 test -x "$1"
 test -r "$2"
 test ! -e "$3"
 test ! -e "$4"
 test ! -e "$5"
 if touch "$6/must-not-write" 2>/dev/null; then exit 1; fi
 touch writable-worktree
 `, "probe", pkgBin, resource, hidden, sibling, localOther, pkgRoot)
	probe := exec.Command(probeArgv[0], probeArgv[1:]...)
	probe.Dir, probe.Env = dir, env
	if out, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("sandbox visibility/write probe: %v: %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "writable-worktree")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sup := codexapp.New(codexapp.Config{SessionID: "sym", Dir: dir, Env: env, Endpoint: endpoint})
	if err := sup.Start(ctx, argv); err != nil {
		t.Fatalf("sandboxed launch of a symlinked-standalone codex: %v", err)
	}
	defer sup.Close()
	if sup.ThreadID() == "" {
		t.Fatal("no thread id: symlinked-standalone codex did not initialize in-scope")
	}
	t.Logf("symlinked-standalone codex launched in-scope via resolved-target bind; thread id = %s", sup.ThreadID())
}
