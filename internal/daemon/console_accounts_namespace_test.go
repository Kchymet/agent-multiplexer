//go:build linux || darwin

package daemon

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/centralauth"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/launchenv"
	"amux/internal/panespec"
)

// Called by the existing mandatory Linux/macOS command-surface CI gate.
func testConsoleAccountCommandSurface(t *testing.T) {
	if err := panespec.IsolationSupport(); err != nil {
		if os.Getenv("AMUX_REQUIRE_NAMESPACE_TEST") == "1" {
			t.Fatal(err)
		}
		t.Skipf("protected namespace unavailable: %v", err)
	}
	candidate := filepath.Join(t.TempDir(), "amux")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", candidate, "../../cmd/amux").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	isolateHome(t)
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("SHELL", "/bin/sh")
	d := New("", nil, time.Hour)
	var err error
	d.authority, err = access.Open(filepath.Join(core.StateDir(), "access", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	r := newSessionRuntime(d)
	if err = r.start(ctx); err != nil {
		cancel()
		d.authority.Close()
		t.Fatal(err)
	}
	defer func() { cancel(); d.drainServingConnections(); r.close(); d.authority.Close() }()
	spec, err := d.launchSpecFor(ctx, console.ID)
	if err != nil {
		t.Fatal(err)
	}
	script := `set -eu
amux auth accounts --json > account-before.json
printf '%s\n' SYNTHETIC-CONSOLE-CLI-TOKEN | amux auth login claude --token-stdin
amux auth status claude > account-after.txt
amux auth default claude
amux auth logout claude
amux auth accounts --json > account-logged-out.json
`
	scriptPath := filepath.Join(console.Dir(), "account-test.sh")
	if err = os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	dir, env, argv, err := panespec.Resolve(spec, panespec.TabTerminal)
	if err != nil {
		t.Fatal(err)
	}
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == "--ro-bind" && argv[i+1] == self {
			argv[i+1] = candidate
		}
	}
	if runtime.GOOS == "darwin" {
		replaceNativeTestTool(t, candidate, console.ID)
	}
	argv = append(argv, scriptPath)
	commandCtx, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	command := exec.CommandContext(commandCtx, argv[0], argv[1:]...)
	command.Dir = dir
	command.Env, err = launchenv.Build(os.Environ(), env, launchenv.ForRuntime(spec.Session.Agent))
	if err != nil {
		t.Fatal(err)
	}
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("console CLI: %v\n%s", err, out)
	}
	if bytes.Contains(out, []byte("SYNTHETIC-CONSOLE-CLI-TOKEN")) {
		t.Fatal("console CLI printed completion token")
	}
	st, err := centralauth.ClaudeStore().Status()
	if err != nil || st.Configured {
		t.Fatal("console logout did not fence/delete credential", err)
	}
	for _, subdir := range []string{"requests", "responses"} {
		entries, err := os.ReadDir(filepath.Join(spec.Access.MailboxHostDir, subdir))
		if err != nil || len(entries) != 0 {
			t.Fatalf("console ceremony used durable mailbox: %s %v %v", subdir, entries, err)
		}
	}
}
