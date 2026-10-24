package codexauth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	authFile    = "auth.json"
	maxAuthFile = 64 << 10
	maxToken    = 16 << 10
)

// errMalformed is internal: auth.json exists but does not hold usable ChatGPT
// tokens (possibly mid-write). It never carries file contents.
var errMalformed = errors.New("codexauth: auth.json is malformed")

// ensureHome creates the private home 0700 and refuses one that is a symlink,
// foreign-owned, or group/other accessible. It never loosens or repairs modes.
func ensureHome(home string) error {
	if err := os.MkdirAll(filepath.Dir(home), 0o700); err != nil {
		return fmt.Errorf("codexauth: create auth root: %w", err)
	}
	if err := os.Mkdir(home, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("codexauth: create home: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Lstat(home, &st); err != nil {
		return fmt.Errorf("codexauth: stat home: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Uid != uint32(os.Geteuid()) || st.Mode&0o077 != 0 {
		return ErrInsecureHome
	}
	return nil
}

// ownerLock takes the cross-process exclusive lock that makes this Provider
// the only native server, and so the only refresh-token writer, for home. The
// descriptor is close-on-exec, so the native server never inherits it.
func ownerLock(home string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(home, ".amux-owner.lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("codexauth: open owner lock: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("codexauth: owner lock: %w", err)
	}
	return f, nil
}

// readTokens is the narrow auth.json parser. It decodes only access/id token,
// account id, and the API-key/auth-mode markers; refresh_token is never
// decoded into any value.
func readTokens(home string) (Tokens, error) {
	fd, err := unix.Open(filepath.Join(home, authFile), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return Tokens{}, ErrNotLoggedIn
	}
	if errors.Is(err, unix.ELOOP) {
		return Tokens{}, ErrInsecureHome
	}
	if err != nil {
		return Tokens{}, fmt.Errorf("codexauth: open auth.json: %w", err)
	}
	f := os.NewFile(uintptr(fd), authFile)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return Tokens{}, fmt.Errorf("codexauth: stat auth.json: %w", err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) || st.Mode&0o077 != 0 {
		return Tokens{}, ErrInsecureHome
	}
	if st.Size > maxAuthFile {
		return Tokens{}, errMalformed
	}
	b, err := io.ReadAll(io.LimitReader(f, maxAuthFile+1))
	defer clear(b)
	if err != nil {
		return Tokens{}, fmt.Errorf("codexauth: read auth.json: %w", err)
	}
	if len(b) > maxAuthFile {
		return Tokens{}, errMalformed
	}
	return parseAuth(b)
}

func parseAuth(b []byte) (Tokens, error) {
	var doc struct {
		AuthMode *string `json:"auth_mode"`
		APIKey   *string `json:"OPENAI_API_KEY"`
		Tokens   *struct {
			IDToken     json.RawMessage `json:"id_token"`
			AccessToken string          `json:"access_token"`
			AccountID   *string         `json:"account_id"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return Tokens{}, errMalformed
	}
	if doc.AuthMode != nil && !strings.EqualFold(*doc.AuthMode, "chatgpt") {
		return Tokens{}, ErrUnsupportedAuth
	}
	if doc.Tokens == nil {
		if doc.APIKey != nil && *doc.APIKey != "" {
			return Tokens{}, ErrUnsupportedAuth
		}
		return Tokens{}, ErrNotLoggedIn
	}
	t := Tokens{AccessToken: doc.Tokens.AccessToken}
	if !validToken(t.AccessToken) {
		return Tokens{}, errMalformed
	}
	access := jwtClaims(t.AccessToken)
	if exp, ok := access["exp"].(float64); ok && exp > 0 {
		t.ExpiresAt = time.Unix(int64(exp), 0).UTC()
	}
	// The id token is a JWT string in current Codex; only its public claims are
	// read and the token itself is discarded.
	var idToken string
	_ = json.Unmarshal(doc.Tokens.IDToken, &idToken)
	id := jwtClaims(idToken)
	if doc.Tokens.AccountID != nil {
		t.AccountID = *doc.Tokens.AccountID
	}
	for _, c := range []map[string]any{id, access} {
		auth, _ := c["https://api.openai.com/auth"].(map[string]any)
		if t.AccountID == "" {
			t.AccountID, _ = auth["chatgpt_account_id"].(string)
		}
		if t.PlanType == "" {
			t.PlanType, _ = auth["chatgpt_plan_type"].(string)
		}
	}
	if t.AccountID == "" || len(t.AccountID) > 256 || strings.ContainsFunc(t.AccountID, isControl) {
		return Tokens{}, errMalformed
	}
	if len(t.PlanType) > 64 || strings.ContainsFunc(t.PlanType, isControl) {
		t.PlanType = ""
	}
	return t, nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

func validToken(s string) bool {
	if s == "" || len(s) > maxToken {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

// jwtClaims decodes an unverified JWT payload for its public claims (exp, plan,
// account). Nothing here is trusted for authorization: the native server and
// OpenAI validate the token itself.
func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return nil
	}
	return claims
}
