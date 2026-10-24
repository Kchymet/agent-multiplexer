package claudecfg

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"amux/internal/cfghome"
)

// The host's real managed policy must never decide a unit test.
func init() {
	absent := filepath.Join(os.TempDir(), "amux-claudecfg-no-managed-settings", "managed-settings.json")
	managedSettingsPaths = map[string]string{"linux": absent, "darwin": absent}
}

func seededCentral(t *testing.T) (cfghome.Spec, string, string) {
	t.Helper()
	isolateAuth(t)
	saveCentral(t)
	sp := Template("one", t.TempDir())
	if _, err := cfghome.Seed(sp); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(t.TempDir(), "managed-settings.json")
	previous := managedSettingsPaths
	managedSettingsPaths = map[string]string{"linux": managed, "darwin": managed}
	t.Cleanup(func() { managedSettingsPaths = previous })
	return sp, t.TempDir(), managed
}

func writeSettingsFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Claude merges managed-settings.d/*.json after managed-settings.json; it
// skips hidden names and other extensions.
func TestCentralConflictsReadManagedDropIns(t *testing.T) {
	sp, project, managed := seededCentral(t)
	dropIns := filepath.Join(filepath.Dir(managed), "managed-settings.d")
	writeSettingsFile(t, filepath.Join(dropIns, ".disabled.json"), `{"env":{"ANTHROPIC_BASE_URL":"https://x.invalid"}}`)
	writeSettingsFile(t, filepath.Join(dropIns, "README.txt"), `{"env":{"ANTHROPIC_BASE_URL":"https://x.invalid"}}`)
	if err := os.MkdirAll(filepath.Join(dropIns, "dir.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := CentralConflicts(sp, project, nil); len(got) != 0 {
		t.Fatalf("ignored drop-ins reported: %v", got)
	}
	writeSettingsFile(t, filepath.Join(dropIns, "20-gateway.json"), `{"env":{"ANTHROPIC_BASE_URL":"https://gateway.invalid"}}`)
	writeSettingsFile(t, filepath.Join(dropIns, "10-key.json"), `{"apiKeyHelper":"/usr/local/bin/key"}`)
	got := CentralConflicts(sp, project, nil)
	want := []string{
		"managed settings 10-key.json apiKeyHelper",
		"managed settings 20-gateway.json env.ANTHROPIC_BASE_URL",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("conflicts = %v\nwant %v", got, want)
	}
	if err := LaunchAuthError(sp, project, nil); err == nil || !strings.Contains(err.Error(), "20-gateway.json") {
		t.Fatalf("drop-in conflict did not refuse the launch: %v", err)
	}
}

// Every credential, endpoint and provider input Claude 2.1.x ranks above or
// uses instead of CLAUDE_CODE_OAUTH_TOKEN is a conflict in a settings layer.
func TestCentralConflictsCoverEveryCredentialEndpointAndProviderInput(t *testing.T) {
	sp, project, _ := seededCentral(t)
	path := filepath.Join(project, ".claude", "settings.json")
	for _, name := range []string{
		"ANTHROPIC_UNIX_SOCKET", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR",
		"CLAUDE_CODE_USE_ANTHROPIC_AWS", "CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD",
		"CLAUDE_CODE_USE_MANTLE", "CLAUDE_CODE_USE_GATEWAY",
	} {
		writeSettingsFile(t, path, `{"env":{"`+name+`":"1"}}`)
		want := "project .claude/settings.json env." + name
		if got := CentralConflicts(sp, project, nil); !slices.Equal(got, []string{want}) {
			t.Fatalf("%s: conflicts = %v", name, got)
		}
		writeSettingsFile(t, path, `{"env":{"`+name+`":"0"}}`)
		if got := CentralConflicts(sp, project, nil); len(got) != 0 {
			t.Fatalf("%s=0 reported: %v", name, got)
		}
	}
	// Unrelated model and telemetry settings never block central mode.
	writeSettingsFile(t, path, `{"model":"opus","env":{"ANTHROPIC_MODEL":"opus","ANTHROPIC_CUSTOM_HEADERS":"X-Team: a","CLAUDE_CODE_ENABLE_TELEMETRY":"1"}}`)
	if got := CentralConflicts(sp, project, nil); len(got) != 0 {
		t.Fatalf("benign settings reported: %v", got)
	}
}
