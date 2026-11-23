package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// daemonLikeEnv reproduces what a detached daemon sees: a minimal PATH without
// the user's ~/.local/bin, and a login shell whose interactive startup prints
// a banner and reports claude as an alias (Claude Code's old local installer
// wrote one) rather than a path. Claude Code's and Codex's native installers
// put both binaries in ~/.local/bin. It returns HOME.
func daemonLikeEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("AMUX_CLAUDE_BIN", "")
	t.Setenv("AMUX_CODEX_BIN", "")
	bin := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for kind, version := range map[string]string{"claude": "2.1.330 (Claude Code)", "codex": "codex-cli 0.171.0"} {
		if err := os.WriteFile(filepath.Join(bin, kind), []byte("#!/bin/sh\necho '"+version+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	shell := filepath.Join(home, "zsh")
	script := "#!/bin/sh\n" +
		"echo 'Last login: Sun Nov 22 19:58:01 on ttys004'\n" +
		"echo 'direnv: loading ~/src/.envrc'\n" +
		"case \"$2\" in *claude*) echo 'alias claude=" + home + "/.claude/local/claude' ;; esac\n" +
		"exit 0\n"
	if err := os.WriteFile(shell, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shell)
	return home
}

// Adoption must find the user's binary exactly as a launch does, even from a
// daemon environment where PATH misses it and the login shell's output is not
// a bare path. Before the resolver validated shell output it returned the
// shell's noise as the "path": launches fell back to the bare name and
// adoption saw no binary, declining with "not installed or logged in".
func TestAdoptInputResolvesLikeALaunchFromADaemonEnvironment(t *testing.T) {
	home := daemonLikeEnv(t)
	oldConfig := configuredBin
	configuredBin = func(string) string { return "" }
	t.Cleanup(func() { configuredBin = oldConfig })
	ctx := context.Background()
	for _, kind := range []string{"claude", "codex"} {
		want := filepath.Join(home, ".local", "bin", kind)
		if got := ResolveBinary(ctx, kind); got.Path != want {
			t.Errorf("%s launch resolves %q; want %q", kind, got.Path, want)
		}
		in := AdoptInput(ctx, kind)
		if in.OnPath != want || in.PathVersion == "" || !in.Used() {
			t.Errorf("%s AdoptInput = %+v; want OnPath %q with a version", kind, in, want)
		}
	}
}

func TestShellResolvedPathTakesOnlyAnExecutablePath(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "claude")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		exe + "\n": exe,
		"Last login: on /dev/ttys004\n" + exe + "\n": exe,
		"alias claude=" + exe + "\n":                 "",
		"claude\n":                                   "", // a shell function
		"/no/such/claude\n":                          "",
		"":                                           "",
	}
	for out, want := range cases {
		if got := shellResolvedPath(out); got != want {
			t.Errorf("shellResolvedPath(%q) = %q; want %q", out, got, want)
		}
	}
}
