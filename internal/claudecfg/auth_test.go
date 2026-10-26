package claudecfg

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"amux/internal/amuxcfg"
	"amux/internal/cfghome"
)

func isolateAuth(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv(Env, filepath.Join(root, ".claude"))
}

func selectAuth(t *testing.T, mode string) {
	t.Helper()
	if err := amuxcfg.SetAuthMode("claude", mode); err != nil {
		t.Fatal(err)
	}
}

// The retired shared mode is inert: even with its saved setting and an
// enabled older shared login, no workload mounts or selects that store.
func TestRetiredSharedAuthIsNeverMounted(t *testing.T) {
	isolateAuth(t)
	selectAuth(t, amuxcfg.AuthShared)
	if err := Login(func() error {
		return os.WriteFile(filepath.Join(SharedAuthDir(), CredentialsFile), []byte("mock shared credential"), 0600)
	}); err != nil {
		t.Fatal(err)
	}
	sp := Template("one", t.TempDir())
	if _, err := cfghome.Seed(sp); err != nil {
		t.Fatal(err)
	}
	if sp.AuthDir != "" || len(sp.Shared) != 0 || len(cfghome.Binds(sp)) != 0 {
		t.Fatalf("retired shared store routed: %+v", sp)
	}
	if slices.Contains(sp.EnvEntries(), SecureStorageEnv+"="+SharedAuthDir()) || HostStoreSharedFor("one") {
		t.Fatal("retired shared store selected")
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

// The retired host mode no longer preserves the host Keychain selector: a
// saved host setting still selects the session's private config directory
// and clears inherited credential overrides, whatever selector the host uses.
func TestRetiredHostModeNeverSelectsHostKeychain(t *testing.T) {
	for _, tc := range []struct {
		name, config, secure string
		secureSet            bool
	}{
		{name: "default"},
		{name: "custom config", config: "/host/claude-work/"},
		{name: "explicit default", config: "/host/claude-work", secureSet: true},
		{name: "separate credentials", config: "/host/settings", secure: "/host/account/", secureSet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateAuth(t)
			selectAuth(t, amuxcfg.AuthHost)
			t.Setenv(Env, tc.config)
			t.Setenv(SecureStorageEnv, tc.secure)
			if !tc.secureSet {
				os.Unsetenv(SecureStorageEnv)
			}
			sp := Template("host-auth", t.TempDir())
			if !slices.Contains(sp.EnvEntries(), SecureStorageEnv+"="+sp.Dir) || !slices.Equal(sp.AuthUnsetEnv, credentialOverrides) {
				t.Fatalf("retired host mode selected a host store: %v", sp.EnvEntries())
			}
			if HostStoreSharedFor("host-auth") {
				t.Fatal("retired host mode shares a host store")
			}
		})
	}
}
