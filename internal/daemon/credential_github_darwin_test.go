//go:build darwin

package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/credentialbroker"
	"amux/internal/launchenv"
	"amux/internal/panespec"
	"amux/internal/store"

	"github.com/cli/go-gh/v2/pkg/config"
)

func TestGitHubBrokerSeatbeltRoundTrip(t *testing.T) { testGitHubSandbox(t, false) }

func TestGitHubBrokerHostAuth(t *testing.T) {
	if os.Getenv("AMUX_TEST_HOST_AUTH") != "1" || os.Getenv("AMUX_TEST_NETWORK") != "1" {
		t.Skip("set AMUX_TEST_HOST_AUTH=1 AMUX_TEST_NETWORK=1 to verify the host GitHub login")
	}
	testGitHubSandbox(t, true)
}

// Exercise the protected CLI alias and absolute-path Git helper override through
// production sandbox profiles and signed mailboxes. The live variant compares
// GitHub's authenticated identity with the host without printing any credential.
func testGitHubSandbox(t *testing.T, live bool) {
	candidate := filepath.Join(t.TempDir(), "amux")
	if out, err := exec.Command("go", "build", "-o", candidate, "../../cmd/amux").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v: %s", err, out)
	}
	binary, err := os.ReadFile(candidate)
	if err != nil {
		t.Fatal(err)
	}
	hostEnv, hostConfig := os.Environ(), config.ConfigDir()
	want := "synthetic-user"
	if live {
		cmd := exec.Command("gh", "api", "--hostname", "github.com", "user", "--jq", ".login")
		out, err := cmd.Output()
		if err != nil || len(out) == 0 {
			t.Fatal("host GitHub authentication failed")
		}
		want = strings.TrimSpace(string(out))
	}
	isolateHome(t)
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("AMUX_EDITOR", "/bin/sh")
	t.Setenv("AMUX_CLAUDE_BIN", "/bin/sh")
	t.Setenv("AMUX_CODEX_BIN", "/bin/sh")
	t.Setenv("GH_HOST", "github.com")
	if live {
		t.Setenv("GH_CONFIG_DIR", hostConfig)
	} else {
		configDir := filepath.Join(os.Getenv("HOME"), ".config", "gh")
		if err := os.MkdirAll(configDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configDir, "hosts.yml"), []byte("github.com:\n  user: synthetic-user\n"), 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GH_CONFIG_DIR", configDir)
		binDir := t.TempDir()
		script := "#!/bin/sh\n[ \"$GH_TOKEN\" = synthetic-token ] || exit 99\ncase \"$1\" in api) echo synthetic-user;; auth) exit 0;; *) exit 98;; esac\n"
		if err := os.WriteFile(filepath.Join(binDir, "gh"), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	}
	// A native absolute helper (as written by gh auth setup-git) would bypass
	// PATH. This sentinel must be replaced by the protected global config.
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".gitconfig"), []byte("[credential \"https://github.com\"]\n helper = !/usr/bin/false\n"), 0600); err != nil {
		t.Fatal(err)
	}
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
		if live {
			return (credentialbroker.GitHub{Env: hostEnv}).Execute(ctx, o)
		}
		return credentialbroker.Result{Value: "synthetic-token"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.run(ctx) }()
	defer func() { cancel(); <-done; r.close() }()
	for _, kind := range []string{"claude", "codex"} {
		for _, tab := range []int{panespec.TabAgent, panespec.TabTerminal, panespec.TabEditor} {
			t.Run(fmt.Sprintf("%s/tab%d", kind, tab), func(t *testing.T) {
				s := store.Session{ID: store.NewUUID(), RootID: "root", Agent: kind}
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
				dir, overlay, argv, err := panespec.Resolve(panespec.LaunchSpec{Session: s, Access: grant}, tab)
				if err != nil {
					t.Fatal(err)
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
				script := `set -eu
test -z "${GH_TOKEN+x}${GITHUB_TOKEN+x}${GH_ENTERPRISE_TOKEN+x}${GITHUB_ENTERPRISE_TOKEN+x}"
test "$(gh api --hostname github.com user --jq .login)" = "$1"
gh auth status >/dev/null 2>&1
token=$(gh auth token --hostname github.com)
test -n "$token"
credential=$(printf 'protocol=https\nhost=github.com\n\n' | GIT_TERMINAL_PROMPT=0 git credential fill)
case "$credential" in *"password=$token"*) ;; *) exit 20;; esac
unset token credential
if gh auth login >/dev/null 2>&1; then exit 21; fi
if gh auth token --hostname unrelated.example >/dev/null 2>&1; then exit 22; fi
if gh auth token --hostname github.com --user other >/dev/null 2>&1; then exit 23; fi
if printf bad > "$GIT_CONFIG_GLOBAL" 2>/dev/null; then exit 24; fi
test -z "${GH_TOKEN+x}${GITHUB_TOKEN+x}"
echo github-broker-ok
`
				found := false
				for i, arg := range argv {
					if arg == "/bin/sh" {
						argv = append(argv[:i+1], "-c", script, "probe", want)
						found = true
						break
					}
				}
				if !found {
					t.Fatal("test payload missing")
				}
				launchCtx, stop := context.WithTimeout(ctx, 30*time.Second)
				defer stop()
				cmd := exec.CommandContext(launchCtx, argv[0], argv[1:]...)
				cmd.Dir = dir
				cmd.Env, err = launchenv.Build(os.Environ(), overlay, launchenv.ForRuntime(kind))
				if err != nil {
					t.Fatal(err)
				}
				out, err := cmd.Output()
				if err != nil || strings.TrimSpace(string(out)) != "github-broker-ok" {
					// Do not dump subprocess output on failure: Git's credential
					// protocol and gh auth token intentionally carry secrets.
					t.Fatalf("sandbox GitHub identity/credential check failed: %v", err)
				}
			})
		}
	}
}
