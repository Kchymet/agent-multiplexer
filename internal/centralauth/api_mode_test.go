package centralauth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeAPIReplacesSingleProtectedCredentialAndLaunchInput(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "claude"), Harness: Claude, Kind: KindClaudeOAuthToken}
	old, _ := NewSecret("synthetic-subscription-secret")
	first, err := store.Save(old, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := NewSecret("synthetic-api-secret")
	api, err := store.SaveKind(key, KindClaudeAPIKey, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load()
	if err != nil || loaded.Kind != KindClaudeAPIKey || loaded.Generation <= first.Generation {
		t.Fatal("mode or generation lost", err)
	}
	info, err := os.Stat(filepath.Join(store.Dir, credentialFile))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential is not private", err)
	}
	env := LaunchCredentialEnv([]string{"ANTHROPIC_API_KEY=old", "CLAUDE_CODE_OAUTH_TOKEN=old", "ANTHROPIC_AUTH_TOKEN=old", "PATH=/bin"}, api)
	if strings.Join(env, ";") != "PATH=/bin;ANTHROPIC_API_KEY="+key.Reveal() {
		t.Fatal("competing inputs retained")
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", loaded, loaded), key.Reveal()) {
		t.Fatal("credential formatting leaked key")
	}
	last, err := store.Save(old, time.Now())
	if err != nil || last.Generation <= api.Generation {
		t.Fatal("API -> subscription did not advance generation", err)
	}
}
