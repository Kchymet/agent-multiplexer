//go:build darwin

package daemon

import (
	"amux/internal/amuxcfg"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/credentialbroker"
	"amux/internal/launchenv"
	"amux/internal/panespec"
	"amux/internal/store"
)

// The host Keychain broker served only the retired host Claude login. Inside
// the real Seatbelt boundary, even a session whose saved setting still names
// host mode reaches no Keychain item and no host credential directory.
func TestCredentialBrokerSeatbeltRefusesRetiredHostLogin(t *testing.T) {
	candidate := filepath.Join(t.TempDir(), "amux")
	if out, err := exec.Command("go", "build", "-o", candidate, "../../cmd/amux").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v: %s", err, out)
	}
	isolateHome(t)
	if err := amuxcfg.SetAuthMode("claude", amuxcfg.AuthHost); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv(claudecfg.SecureStorageEnv, claudecfg.User().Dir)
	root := claudecfg.CredentialDirectory()
	if err := os.MkdirAll(filepath.Join(root, "projects"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "projects", "private"), []byte("host-only-history"), 0600); err != nil {
		t.Fatal(err)
	}
	s := store.Session{ID: "broker", RootID: "root", Agent: "claude", Dir: filepath.Join(core.SessionsDir(), "root", "broker")}
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PutSession(s); err != nil {
		t.Fatal(err)
	}
	d := New("", nil, time.Hour)
	d.authority, err = access.Open(filepath.Join(core.StateDir(), "access", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.authority.Close()
	grant, err := d.authority.EnsureSession(context.Background(), s.ID, s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	r := newSessionRuntime(d)
	calls := 0
	r.credentialOperation = func(context.Context, credentialbroker.Operation) (credentialbroker.Result, error) {
		calls++
		return credentialbroker.Result{Value: "must-not-be-served"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := r.open(ctx, s); err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); r.run(ctx) }()
	defer func() { cancel(); <-done; r.close() }()
	dir, overlay, argv, err := panespec.Resolve(panespec.LaunchSpec{Session: s, Access: grant}, panespec.TabTerminal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(candidate, core.SessionBinPath(s.ID)); err != nil {
		t.Fatal(err)
	}
	script := `set -u
if security find-generic-password -a "$3" -s "$2" -w >/dev/null 2>&1; then exit 20; fi
if printf 'add-generic-password -U -a "%s" -s "%s" -X "726f7461746564"\n' "$3" "$2" | security -i >/dev/null 2>&1; then
  if security find-generic-password -a "$3" -s "$2" -w >/dev/null 2>&1; then exit 21; fi
fi
if security dump-keychain >/dev/null 2>&1; then exit 22; fi
if cat "$1/projects/private" >/dev/null 2>&1; then exit 23; fi
if mkdir "$1/.oauth_refresh.lock" 2>/dev/null; then exit 24; fi
echo broker-refused
`
	argv = append(argv, "-c", script, "probe", root, credentialbroker.ClaudeServices(claudecfg.CredentialSelector())[1], credentialbroker.HostAccount())
	launchCtx, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	cmd := exec.CommandContext(launchCtx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env, err = launchenv.Build(os.Environ(), overlay, launchenv.ForRuntime(s.Agent))
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "broker-refused") {
		t.Fatalf("retired host Keychain reachable: %v: %s", err, out)
	}
	if calls != 0 {
		t.Fatalf("retired host Keychain backend reached %d times", calls)
	}
}
