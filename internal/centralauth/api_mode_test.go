package centralauth

import (
	"amux/internal/sessionrpc"
	"context"
	"encoding/json"
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

type legacyDeliveryQuerier struct {
	calls int
	api   bool
}

func (q *legacyDeliveryQuerier) Query(_ context.Context, r sessionrpc.Query) (sessionrpc.Result, error) {
	q.calls++
	if len(r.Fields) > 1 {
		return sessionrpc.Result{}, &sessionrpc.RemoteError{Code: "access_denied"}
	}
	kind := KindClaudeOAuthToken
	if q.api {
		kind = KindClaudeAPIKey
	}
	b, _ := json.Marshal(Delivery{Token: "synthetic-secret", Kind: kind, Generation: 1})
	return sessionrpc.Result{Status: sessionrpc.StatusOK, Body: b}, nil
}
func (q *legacyDeliveryQuerier) Close() error { return nil }

func TestTypedDeliveryFallbackNeverAcceptsAPIKey(t *testing.T) {
	prior := openSession
	defer func() { openSession = prior }()
	for _, api := range []bool{false, true} {
		q := &legacyDeliveryQuerier{api: api}
		openSession = func() (sessionQuerier, error) { return q, nil }
		cred, err := FetchCredentialFromSession()
		if q.calls != 2 {
			t.Fatal("legacy compatibility path not exercised")
		}
		if api && (err == nil || !cred.Token.Empty()) {
			t.Fatal("untyped fallback accepted API material")
		}
		if !api && (err != nil || cred.Kind != KindClaudeOAuthToken) {
			t.Fatal("legacy subscription compatibility failed", err)
		}
	}
}
