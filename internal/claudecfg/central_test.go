package claudecfg

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"amux/internal/amuxcfg"
	"amux/internal/centralauth"
	"amux/internal/cfghome"
)

const mockCentralToken = "mock-central-token-0123456789"

func saveCentral(t *testing.T) {
	t.Helper()
	tok, err := centralauth.NewSecret(mockCentralToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := centralauth.ClaudeStore().Save(tok, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestCentralIsTheDefaultAndSharesNoHostStore(t *testing.T) {
	isolateAuth(t)
	user := User()
	if err := os.MkdirAll(user.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(user.CredentialsPath(), []byte("mock host credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	// An older amux shared login is not selected unless claude.auth says so.
	if err := Login(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if AuthMode() != amuxcfg.AuthCentral || HostStoreShared() {
		t.Fatalf("default mode = %q, host store shared = %v", AuthMode(), HostStoreShared())
	}
	if CredentialSelector() == SharedAuthDir() {
		t.Fatal("unselected legacy shared store became the credential selector")
	}
	sp := Template("one", t.TempDir())
	if len(sp.Shared) != 0 || sp.AuthDir != "" || !slices.Equal(sp.Retire, []string{CredentialsFile}) {
		t.Fatalf("central routing = shared %v, dir %q, retire %v", sp.Shared, sp.AuthDir, sp.Retire)
	}
	if binds := cfghome.Binds(sp); len(binds) != 0 {
		t.Fatalf("central mode binds host credentials: %v", binds)
	}
	env := sp.EnvEntries()
	if !slices.Contains(env, SecureStorageEnv+"="+sp.Dir) {
		t.Fatalf("secure storage must be the private config dir: %v", env)
	}
	for _, key := range credentialOverrides {
		if !slices.Contains(env, key+"=") {
			t.Fatalf("inherited %s not cleared: %v", key, env)
		}
	}
	if _, err := cfghome.Seed(sp); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(sp.Dir, CredentialsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh central copy has a credential entry: %v", err)
	}
}

func TestCentralRetiresLegacyLinksWithoutTouchingTheirTargets(t *testing.T) {
	isolateAuth(t)
	user := User()
	if err := os.MkdirAll(user.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(user.CredentialsPath(), []byte("mock host credential"), 0o600); err != nil {
		t.Fatal(err)
	}
	selectAuth(t, amuxcfg.AuthHost)
	linked := Template("linked", t.TempDir())
	detached := Template("detached", t.TempDir())
	for _, sp := range []cfghome.Spec{linked, detached} {
		if _, err := cfghome.Seed(sp); err != nil {
			t.Fatal(err)
		}
	}
	if target, err := os.Readlink(filepath.Join(linked.Dir, CredentialsFile)); err != nil || target != user.CredentialsPath() {
		t.Fatalf("host mode link = %q, %v", target, err)
	}
	// A private credential that is a regular file is not amux's to delete.
	private := filepath.Join(detached.Dir, CredentialsFile)
	if err := os.Remove(private); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(private, []byte("mock private credential"), 0o600); err != nil {
		t.Fatal(err)
	}

	selectAuth(t, amuxcfg.AuthCentral)
	for _, sp := range []cfghome.Spec{Template("linked", linked.Root), Template("detached", detached.Root)} {
		if _, err := cfghome.Seed(sp); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(filepath.Join(linked.Dir, CredentialsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy link survived central migration: %v", err)
	}
	if b, err := os.ReadFile(user.CredentialsPath()); err != nil || string(b) != "mock host credential" {
		t.Fatalf("host credential changed by link retirement: %q, %v", b, err)
	}
	if b, err := os.ReadFile(private); err != nil || string(b) != "mock private credential" {
		t.Fatalf("detached private credential removed: %q, %v", b, err)
	}
}

func TestEnvironmentModePreservesInheritedProviderSelection(t *testing.T) {
	isolateAuth(t)
	selectAuth(t, amuxcfg.AuthEnvironment)
	sp := Template("env", t.TempDir())
	if len(sp.Shared) != 0 || sp.AuthDir != "" || len(sp.AuthUnsetEnv) != 0 {
		t.Fatalf("environment routing = %+v", sp)
	}
	for _, kv := range sp.EnvEntries() {
		if strings.HasPrefix(kv, "ANTHROPIC_") || strings.HasPrefix(kv, "CLAUDE_CODE_OAUTH") {
			t.Fatalf("environment mode overrode %s", kv)
		}
	}
	if err := LaunchAuthError(sp, []string{"ANTHROPIC_API_KEY=mock"}); err != nil {
		t.Fatalf("environment mode launch error = %v", err)
	}
}

func TestLaunchAuthErrorGuidesMigrationAndNamesConflictsWithoutValues(t *testing.T) {
	isolateAuth(t)
	sp := Template("one", t.TempDir())
	if _, err := cfghome.Seed(sp); err != nil {
		t.Fatal(err)
	}
	err := LaunchAuthError(sp, nil)
	if !errors.Is(err, ErrCentralLoginRequired) || !strings.Contains(err.Error(), "amux auth login") ||
		!strings.Contains(err.Error(), "claude.auth environment") {
		t.Fatalf("missing login error = %v", err)
	}
	saveCentral(t)
	if err := LaunchAuthError(sp, []string{"PATH=/usr/bin", "ANTHROPIC_API_KEY="}); err != nil {
		t.Fatalf("configured central launch error = %v", err)
	}

	const secretValue = "mock-gateway-secret-value"
	err = LaunchAuthError(sp, []string{"ANTHROPIC_BASE_URL=https://gateway.invalid", "ANTHROPIC_AUTH_TOKEN=" + secretValue})
	if err == nil || !strings.Contains(err.Error(), "environment ANTHROPIC_BASE_URL") ||
		!strings.Contains(err.Error(), "environment ANTHROPIC_AUTH_TOKEN") || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("inherited conflict error = %v", err)
	}
	settings := `{"apiKeyHelper":"/bin/echo ` + secretValue + `","env":{"CLAUDE_CODE_USE_BEDROCK":"1","ANTHROPIC_MODEL":"x"}}`
	if err := os.WriteFile(filepath.Join(sp.Dir, "settings.local.json"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	err = LaunchAuthError(sp, nil)
	if err == nil || !strings.Contains(err.Error(), "settings.local.json apiKeyHelper") ||
		!strings.Contains(err.Error(), "settings.local.json env.CLAUDE_CODE_USE_BEDROCK") ||
		strings.Contains(err.Error(), "ANTHROPIC_MODEL") || strings.Contains(err.Error(), secretValue) {
		t.Fatalf("settings conflict error = %v", err)
	}

	selectAuth(t, amuxcfg.AuthShared)
	if err := LaunchAuthError(sp, nil); err == nil || !strings.Contains(err.Error(), "no older amux shared Claude login") {
		t.Fatalf("shared without login = %v", err)
	}
	selectAuth(t, amuxcfg.AuthHost)
	if err := LaunchAuthError(sp, []string{"ANTHROPIC_API_KEY=mock"}); err != nil {
		t.Fatalf("host mode launch error = %v", err)
	}
}

func TestInvalidCentralStoreFailsClosed(t *testing.T) {
	isolateAuth(t)
	saveCentral(t)
	store := centralauth.ClaudeStore()
	if err := os.Chmod(store.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sp := Template("one", t.TempDir())
	if err := LaunchAuthError(sp, nil); err == nil || !strings.Contains(err.Error(), "not private") {
		t.Fatalf("exposed store launch error = %v", err)
	}
}
