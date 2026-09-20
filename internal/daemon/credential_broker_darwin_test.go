//go:build darwin

package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/cfghome"
	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/credentialbroker"
	"amux/internal/launchenv"
	"amux/internal/panespec"
	"amux/internal/store"
)

// Uses auth status only: no tokens are printed, no login is modified and no
// model requests are made. The production launch environment and policy must
// see the same account as the host, with a fresh private configuration home.
func TestCredentialBrokerClaudeHostAuth(t *testing.T) {
	if os.Getenv("AMUX_TEST_HOST_AUTH") != "1" {
		t.Skip("set AMUX_TEST_HOST_AUTH=1 to verify the host Claude login")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	hostEnv := os.Environ()
	hostConfig := claudecfg.User()
	selector := claudecfg.CredentialSelector()
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("SHELL", bin)
	t.Setenv("AMUX_EDITOR", bin)
	candidate := filepath.Join(t.TempDir(), "amux")
	if out, err := exec.Command("go", "build", "-o", candidate, "../../cmd/amux").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v: %s", err, out)
	}
	binary, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv(claudecfg.Env, hostConfig.Dir)
	t.Setenv(claudecfg.SecureStorageEnv, selector)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	d := New("", nil, time.Hour)
	d.authority, err = access.Open(filepath.Join(core.StateDir(), "access", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.authority.Close()
	r := newSessionRuntime(d)
	r.credentialOperation = func(ctx context.Context, o credentialbroker.Operation) (credentialbroker.Result, error) {
		result, err := (credentialbroker.Keychain{Env: hostEnv}).Execute(ctx, o)
		t.Logf("broker operation %s exit=%d error=%v", o.Verb, result.ExitCode, err)
		return result, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.run(ctx) }()
	defer func() { cancel(); <-done; r.close() }()
	type status struct {
		LoggedIn   bool   `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
		Email      string `json:"email"`
		OrgID      string `json:"orgId"`
	}
	check := func(argv, env []string, dir string) status {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env, cmd.Dir = env, dir
		out, err := cmd.Output()
		var got status
		if json.Unmarshal(out, &got) != nil {
			t.Fatalf("auth status output invalid (command error: %v)", err)
		}
		if err != nil || !got.LoggedIn {
			t.Fatalf("Claude is not logged in (command error: %v)", err)
		}
		return got
	}
	want := check([]string{bin, "auth", "status"}, hostEnv, t.TempDir())
	for _, tab := range []int{panespec.TabAgent, panespec.TabTerminal, panespec.TabEditor} {
		s := store.Session{ID: store.NewUUID(), RootID: "root", Agent: "claude"}
		s.Dir = filepath.Join(core.SessionsDir(), s.RootID, s.ID)
		if err := os.MkdirAll(s.Dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
		grant, err := d.authority.EnsureSession(ctx, s.ID, s.Dir)
		if err != nil {
			t.Fatal(err)
		}
		spec := panespec.LaunchSpec{Session: s, Access: grant}
		config := claudecfg.Template(s.ID, s.Dir)
		config.Entries[0].Src = hostConfig.ConfigPath()
		if _, err := cfghome.Seed(config); err != nil {
			t.Fatal(err)
		}
		_, overlay, argv, err := panespec.Resolve(spec, tab)
		if err != nil {
			t.Fatal(err)
		}
		// Keep the production policy and environment, replacing only the Claude
		// payload arguments with its non-interactive auth status subcommand.
		realBin, err := filepath.EvalSymlinks(bin)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for i, arg := range argv {
			if arg == realBin || arg == bin {
				argv = append(argv[:i+1], "auth", "status")
				found = true
				break
			}
		}
		if !found {
			t.Fatal("Claude payload missing")
		}
		tool := core.SessionBinPath(s.ID)
		if err := os.Chmod(tool, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tool, binary, 0500); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(tool, 0500); err != nil {
			t.Fatal(err)
		}
		env, err := launchenv.Build(os.Environ(), overlay, launchenv.ForRuntime(s.Agent))
		if err != nil {
			t.Fatal(err)
		}
		if got := check(argv, env, s.Dir); got != want {
			t.Fatalf("tab %d did not inherit host identity", tab)
		}
		t.Logf("tab %d inherits the host Claude identity", tab)
	}
}

func TestCredentialBrokerSeatbeltRoundTrip(t *testing.T) {
	candidate := filepath.Join(t.TempDir(), "amux")
	if out, err := exec.Command("go", "build", "-o", candidate, "../../cmd/amux").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v: %s", err, out)
	}
	isolateHome(t)
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
	value := "initial"
	r.credentialOperation = func(_ context.Context, o credentialbroker.Operation) (credentialbroker.Result, error) {
		switch o.Verb {
		case credentialbroker.Read:
			if value == "" {
				return credentialbroker.Result{ExitCode: 44}, nil
			}
			return credentialbroker.Result{Value: value}, nil
		case credentialbroker.Write:
			value = o.Value
		case credentialbroker.Delete:
			value = ""
		}
		return credentialbroker.Result{}, nil
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
	script := `set -eu
test "$(security find-generic-password -a "$3" -s "$2" -w)" = initial
printf 'add-generic-password -U -a "%s" -s "%s" -X "726f7461746564"\n' "$3" "$2" | security -i
test "$(security find-generic-password -a "$3" -s "$2" -w)" = rotated
security delete-generic-password -a "$3" -s "$2"
if security find-generic-password -a "$3" -s "$2" -w >/dev/null; then exit 20; fi
if security find-generic-password -a "$3" -s unrelated-password -w >/dev/null; then exit 21; fi
if security dump-keychain >/dev/null; then exit 22; fi
mkdir "$1/.storage-write.lock"
rmdir "$1/.storage-write.lock"
mkdir "$1/.oauth_refresh.lock"
rmdir "$1/.oauth_refresh.lock"
mkdir "$1.lock"
rmdir "$1.lock"
if cat "$1/projects/private" >/dev/null 2>&1; then exit 23; fi
if mkdir "$1/unrelated" 2>/dev/null; then exit 24; fi
echo broker-ok
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
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "broker-ok") {
		t.Fatalf("broker round trip: %v: %s", err, out)
	}
}
