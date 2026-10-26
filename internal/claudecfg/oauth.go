package claudecfg

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"time"

	"amux/internal/amuxcfg"
)

// ProfileScope is the OAuth scope Claude's subscription usage report requires.
// Interactive logins carry it; `claude setup-token` tokens are inference-only.
const ProfileScope = "user:profile"

// OAuthLogin is the access half of a Claude interactive login. The refresh
// token is never read: refreshing would fork the login's rotating chain, so an
// expired access token waits for Claude itself to renew it.
type OAuthLogin struct {
	AccessToken string
	ExpiresAt   time.Time // zero when the store states no expiry
	Scopes      []string  // nil when the store states none
}

// HasScope reports whether the login states scope; an unstated set is unknown.
func (l OAuthLogin) HasScope(scope string) (has, known bool) {
	if l.Scopes == nil {
		return false, false
	}
	return slices.Contains(l.Scopes, scope), true
}

// ErrOAuthLoginUnavailable means the source's store holds no readable login.
var ErrOAuthLoginUnavailable = errors.New("claude oauth login unavailable")

// ReadOAuthLogin reads the interactive login a host or shared source uses.
// macOS keeps these logins in the Keychain, which amux does not read.
func ReadOAuthLogin(mode string) (OAuthLogin, error) {
	var dir string
	switch {
	case runtime.GOOS == "darwin":
		return OAuthLogin{}, ErrOAuthLoginUnavailable
	case mode == amuxcfg.AuthHost:
		if dir = hostCredentialSelector(); dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return OAuthLogin{}, ErrOAuthLoginUnavailable
			}
			dir = filepath.Join(home, ".claude")
		}
	case mode == amuxcfg.AuthShared && SharedAuthEnabled():
		dir = SharedAuthDir()
	default:
		return OAuthLogin{}, ErrOAuthLoginUnavailable
	}
	return readOAuthLogin(filepath.Join(dir, CredentialsFile))
}

func readOAuthLogin(path string) (OAuthLogin, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return OAuthLogin{}, ErrOAuthLoginUnavailable
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 64<<10))
	if err != nil {
		return OAuthLogin{}, ErrOAuthLoginUnavailable
	}
	var data struct {
		OAuth *struct {
			AccessToken string   `json:"accessToken"`
			ExpiresAt   int64    `json:"expiresAt"` // Unix milliseconds
			Scopes      []string `json:"scopes"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(raw, &data) != nil || data.OAuth == nil || data.OAuth.AccessToken == "" {
		return OAuthLogin{}, ErrOAuthLoginUnavailable
	}
	l := OAuthLogin{AccessToken: data.OAuth.AccessToken, Scopes: data.OAuth.Scopes}
	if data.OAuth.ExpiresAt > 0 {
		l.ExpiresAt = time.UnixMilli(data.OAuth.ExpiresAt)
	}
	return l, nil
}
