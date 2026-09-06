package claudecfg

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in compatibility check of Claude's undocumented store-location override.
// Uses only synthetic credentials in temporary homes, never a real login or a
// model request. This detects a CLI that ignores the secure-storage override.
func TestClaudeSharedAuthStoreCompatibility(t *testing.T) {
	if os.Getenv("AMUX_CLAUDE_AUTH_SMOKE") != "1" {
		t.Skip("set AMUX_CLAUDE_AUTH_SMOKE=1 with Claude installed to check store routing")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	isolateAuth(t)
	if err := Login(func() error {
		b, err := json.Marshal(map[string]any{"claudeAiOauth": map[string]any{
			"accessToken": "mock-access-token", "refreshToken": "mock-refresh-token",
			"expiresAt": time.Now().Add(time.Hour).UnixMilli(), "scopes": []string{"user:inference"},
		}})
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(SharedAuthDir(), CredentialsFile), b, 0600)
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		private := filepath.Join(t.TempDir(), id)
		if err := os.MkdirAll(private, 0700); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cmd := exec.CommandContext(ctx, bin, "auth", "status")
		cmd.Dir = private
		cmd.Env = append(AuthCommandEnv(os.Environ()), Env+"="+private)
		b, err := cmd.Output()
		cancel()
		if err != nil {
			t.Fatalf("Claude did not find synthetic shared credentials: %v", err)
		}
		var status struct {
			LoggedIn bool `json:"loggedIn"`
		}
		if err := json.Unmarshal(b, &status); err != nil || !status.LoggedIn {
			t.Fatalf("Claude did not honor %s (status parse error: %v)", SecureStorageEnv, err)
		}
	}
}
