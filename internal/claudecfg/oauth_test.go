package claudecfg

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadOAuthLogin(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, CredentialsFile)
	write := func(s string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-a","refreshToken":"r","expiresAt":1762232399000,"scopes":["user:inference","user:profile"]}}`)
	l, err := readOAuthLogin(path)
	if err != nil || l.AccessToken != "sk-ant-oat01-a" || !l.ExpiresAt.Equal(time.UnixMilli(1762232399000)) {
		t.Fatalf("%+v %v", l, err)
	}
	if has, known := l.HasScope(ProfileScope); !has || !known {
		t.Fatal("profile scope not found")
	}
	write(`{"claudeAiOauth":{"accessToken":"x"}}`)
	if l, err = readOAuthLogin(path); err != nil || !l.ExpiresAt.IsZero() {
		t.Fatalf("%+v %v", l, err)
	}
	if _, known := l.HasScope(ProfileScope); known {
		t.Fatal("unstated scopes reported as known")
	}
	for _, bad := range []string{`{}`, `{"claudeAiOauth":{}}`, `not json`, `{"mcpOAuth":{}}`} {
		write(bad)
		if _, err := readOAuthLogin(path); err != ErrOAuthLoginUnavailable {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readOAuthLogin(link); err != ErrOAuthLoginUnavailable {
		t.Fatal("followed a symlinked credential store")
	}
}
