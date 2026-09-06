package claudecfg

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"amux/internal/cfghome"
)

func isolateAuth(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv(Env, filepath.Join(root, ".claude"))
}

func TestSharedAuthSurvivesAtomicRotationAndIgnoresDetachedFiles(t *testing.T) {
	isolateAuth(t)
	if SharedAuthEnabled() {
		t.Fatal("shared auth enabled before login")
	}
	if err := Login(func() error {
		return os.WriteFile(filepath.Join(SharedAuthDir(), CredentialsFile), []byte("first mock credential"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
	var specs []cfghome.Spec
	for _, id := range []string{"one", "two"} {
		sp := Template(id, filepath.Join(t.TempDir(), ".amux", "claude"))
		if _, err := cfghome.Seed(sp); err != nil {
			t.Fatal(err)
		}
		// An old detached file must not be used, removed, or mounted as shared.
		if err := os.WriteFile(filepath.Join(sp.Dir, CredentialsFile), []byte("stale private credential"), 0600); err != nil {
			t.Fatal(err)
		}
		if sp.AuthDir != SharedAuthDir() || len(sp.Shared) != 0 {
			t.Fatalf("unexpected auth routing: %+v", sp)
		}
		if !slices.Contains(sp.EnvEntries(), SecureStorageEnv+"="+SharedAuthDir()) {
			t.Fatalf("missing credential store override: %v", sp.EnvEntries())
		}
		binds := cfghome.Binds(sp)
		if len(binds) != 1 || !slices.Equal(binds[0], []string{"--bind", SharedAuthDir(), SharedAuthDir()}) {
			t.Fatalf("must bind the directory, not a credential inode: %v", binds)
		}
		specs = append(specs, sp)
	}
	tmp := filepath.Join(SharedAuthDir(), "rotating.tmp")
	if err := os.WriteFile(tmp, []byte("rotated mock credential"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(SharedAuthDir(), CredentialsFile)); err != nil {
		t.Fatal(err)
	}
	for _, sp := range specs {
		b, err := os.ReadFile(filepath.Join(sp.AuthDir, CredentialsFile))
		if err != nil || string(b) != "rotated mock credential" {
			t.Fatalf("agent %s missed rotation: %q, %v", sp.AgentID, b, err)
		}
		changes, err := cfghome.Scan(sp)
		if err != nil || len(changes) != 0 {
			t.Fatalf("unused legacy credentials must not cause drift: %v, %v", changes, err)
		}
	}
}

func TestLoginFailureDoesNotActivateSharedAuth(t *testing.T) {
	isolateAuth(t)
	want := errors.New("login cancelled")
	if err := Login(func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("Login = %v", err)
	}
	if SharedAuthEnabled() {
		t.Fatal("failed login activated shared auth")
	}
	if err := Login(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := Login(func() error { return want }); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if !SharedAuthEnabled() {
		t.Fatal("failed subsequent login disabled shared auth")
	}
}

func TestLoginSerializesInteractiveFlows(t *testing.T) {
	isolateAuth(t)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- Login(func() error { close(entered); <-release; return nil })
	}()
	<-entered
	err := Login(func() error { t.Error("concurrent login callback ran"); return nil })
	close(release)
	if err == nil {
		t.Error("concurrent login acquired the lock")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := Login(func() error { return nil }); err != nil {
		t.Fatalf("lock was not released: %v", err)
	}
}

func TestAuthCommandEnvUsesOneContext(t *testing.T) {
	isolateAuth(t)
	env := AuthCommandEnv([]string{
		Env + "=/private/agent", SecureStorageEnv + "=/another/account",
		"CLAUDE_CODE_OAUTH_TOKEN=mock", "ANTHROPIC_API_KEY=mock",
		"CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=9", "CLAUDECODE=1", "PATH=/usr/bin",
	})
	want := []string{"PATH=/usr/bin", Env + "=" + SharedAuthDir(), SecureStorageEnv + "=" + SharedAuthDir()}
	if !slices.Equal(env, want) {
		t.Fatalf("unexpected environment keys: %s", strings.Join(env, ", "))
	}
}
