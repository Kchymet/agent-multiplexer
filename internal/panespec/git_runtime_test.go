package panespec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/git"
	"amux/internal/launchenv"
	"amux/internal/store"
)

func TestRuntimeGitObjectGrantIsExactReadOnly(t *testing.T) {
	requireRuntimeIsolation(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "on")

	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitFixture(t, source, "init", "-q", "-b", "main")
	runGitFixture(t, source, "config", "user.name", "amux test")
	runGitFixture(t, source, "config", "user.email", "amux@example.invalid")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("authorized-base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitFixture(t, source, "add", "README.md")
	runGitFixture(t, source, "commit", "-q", "-m", "base")

	managed := filepath.Join(root, "sessions")
	s := store.Session{ID: "git-owner", RootID: "root", Agent: "codex", Repo: "repo", Dir: filepath.Join(managed, "root", "git-owner")}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(s.Dir, "repo")
	layout := filepath.Join(root, "layout.json")
	key := git.SourceKey(source)
	if err := git.AddCheckout(context.Background(), git.CheckoutRequest{
		Source: source, Path: checkout, Branch: "amux/runtime-test", RepoKey: key,
		PoolRoot: filepath.Join(root, "pool"), StagingRoot: filepath.Join(root, "staging"),
		ManagedRoot: managed, LayoutPath: layout, AllowLocalSource: true,
	}); err != nil {
		t.Fatal(err)
	}
	mounts, err := git.ReadObjectMounts(layout, checkout, managed)
	if err != nil || len(mounts) == 0 {
		t.Fatalf("read typed object closure: mounts=%+v err=%v", mounts, err)
	}
	spec := testLaunchSpec(t, s)
	spec.GitObjects = mounts
	if _, err := validateLaunchSpec(spec); err != nil {
		t.Fatal(err)
	}
	poolConfig := filepath.Join(filepath.Dir(mounts[0].ObjectsHostDir), "config")
	if _, err := os.Stat(poolConfig); err != nil {
		t.Fatalf("fixture lacks planted pool config: %v", err)
	}
	script := `set -eu
test "$(git -C "$1" show HEAD:README.md)" = authorized-base
test ! -e "$2"
if touch "$3/session-write" 2>/dev/null; then exit 31; fi
`
	argv, err := scope(checkout, TabAgent, s, spec.Access, spec.GitObjects, []string{"/bin/sh", "-c", script, "probe", checkout, poolConfig, mounts[0].ObjectsMountDir})
	if err != nil {
		t.Fatal(err)
	}
	overlay := platformLaunchEnv(spec)
	if isolationPlatform == "darwin" {
		// Exercise Apple's Git shim even when the runner also has Homebrew Git.
		overlay = append(overlay, "PATH=/usr/bin:/bin")
	}
	launchEnv, err := launchenv.Build([]string{"HOME=" + home, "PATH=/usr/bin:/bin"}, overlay, launchenv.ForRuntime(s.Agent))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = launchEnv
	cmd.Dir = checkout
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pooled worktree through exact read-only Git object closure: %v: %s", err, out)
	}
}

func runGitFixture(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
