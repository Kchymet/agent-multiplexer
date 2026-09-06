package panespec

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/store"
	"amux/internal/wsops"
)

func TestSharedAuthDirectoryIsVisibleOnlyToClaudePanes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A runtime mount can also expose the canonical spelling of XDG_DATA_HOME.
	realData := filepath.Join(home, "real-data")
	if err := os.MkdirAll(realData, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realData, filepath.Join(home, "data")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("AMUX_JAIL", "on")
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	// Only inspect generated argv; no dependency on the host's bwrap install.
	if err := os.WriteFile(filepath.Join(bin, "bwrap"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := claudecfg.Login(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"claude", "codex"} {
		for _, tab := range []int{TabAgent, TabTerminal, TabEditor} {
			s := store.Session{ID: "one", Agent: kind, Dir: filepath.Join(home, "agent")}
			argv := scope(s.Dir, tab, s, []string{"/usr/bin/true"}, nil)
			mask, bind, canonicalMask := -1, -1, -1
			for i := 0; i < len(argv)-2; i++ {
				if argv[i] == "--tmpfs" && argv[i+1] == core.AuthDir() {
					mask = i
				}
				if argv[i] == "--tmpfs" && argv[i+1] == filepath.Join(realData, "amux", "auth") {
					canonicalMask = i
				}
				if slices.Equal(argv[i:i+3], []string{"--bind", claudecfg.SharedAuthDir(), claudecfg.SharedAuthDir()}) {
					bind = i
				}
			}
			if mask < 0 || canonicalMask < 0 {
				t.Fatalf("%s tab %d exposes auth root: %v", kind, tab, argv)
			}
			if kind == "claude" {
				if bind <= mask {
					t.Fatalf("Claude tab %d must bind auth after the mask: %v", tab, argv)
				}
				env := wsops.AgentEnv(s)
				if !slices.Contains(env, claudecfg.SecureStorageEnv+"="+claudecfg.SharedAuthDir()) || !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=") {
					t.Fatalf("Claude auth environment incomplete: %v", env)
				}
			} else if bind >= 0 {
				t.Fatalf("%s inherited Claude credentials", kind)
			}
		}
	}
}

// Keep two real mount namespaces alive while the host replaces the credentials.
// A file bind would retain the old inode; a directory bind must see the new one.
func TestRunningClaudeScopesObserveCredentialReplacement(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap unavailable")
	}
	if out, err := exec.Command("bwrap", "--ro-bind", "/", "/", "--unshare-user", "--", "/bin/true").CombinedOutput(); err != nil {
		t.Skipf("bubblewrap unavailable: %v: %s", err, out)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "on")
	if err := claudecfg.Login(func() error {
		return os.WriteFile(filepath.Join(claudecfg.SharedAuthDir(), claudecfg.CredentialsFile), []byte("old"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var processes []*exec.Cmd
	var inputs []io.WriteCloser
	for _, id := range []string{"one", "two"} {
		s := store.Session{ID: id, Agent: "claude", Dir: filepath.Join(home, id)}
		if err := os.MkdirAll(s.Dir, 0700); err != nil {
			t.Fatal(err)
		}
		script := `set -eu
cd "$CLAUDE_SECURESTORAGE_CONFIG_DIR"
test "$(cat .credentials.json)" = old
echo ready
read -r signal
test "$(cat .credentials.json)" = rotated
test -d .oauth_refresh.lock
echo seen > "$1"
`
		argv := scope(s.Dir, TabAgent, s, []string{"/bin/sh", "-c", script, "auth-test", id}, nil)
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Env = append(os.Environ(), wsops.AgentEnv(s)...)
		cmd.Stderr = os.Stderr
		in, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
			t.Fatalf("scope did not load initial credentials: %q, %v", line, err)
		}
		processes, inputs = append(processes, cmd), append(inputs, in)
	}
	tmp := filepath.Join(claudecfg.SharedAuthDir(), "replacement")
	if err := os.WriteFile(tmp, []byte("rotated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(claudecfg.SharedAuthDir(), claudecfg.CredentialsFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(claudecfg.SharedAuthDir(), ".oauth_refresh.lock"), 0700); err != nil {
		t.Fatal(err)
	}
	for i, cmd := range processes {
		if _, err := io.WriteString(inputs[i], "continue\n"); err != nil {
			t.Fatal(err)
		}
		inputs[i].Close()
		if err := cmd.Wait(); err != nil {
			t.Fatalf("running scope missed shared credential/lock update: %v", err)
		}
	}
	for _, id := range []string{"one", "two"} {
		if b, err := os.ReadFile(filepath.Join(claudecfg.SharedAuthDir(), id)); err != nil || string(b) != "seen\n" {
			t.Fatalf("scope %s did not write to the shared directory: %v", id, err)
		}
	}
}
