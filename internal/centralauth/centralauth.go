// Package centralauth is amux's central harness credential service. The host
// acquires one credential per harness; the daemon stores it outside every
// sandbox and delivers only that credential, over the authenticated session
// mailbox, to panes it has authorized. No session receives a refresh token,
// another session's credential, or the host harness's own login store.
//
// Claude uses the documented long-lived OAuth token from `claude setup-token`,
// consumed through CLAUDE_CODE_OAUTH_TOKEN. That token has no refresh token:
// renewal is a new host login followed by restarting panes (conversations are
// resumed), never an automatic refresh.
package centralauth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode"
)

// Harness names a credential store.
type Harness string

const (
	Claude Harness = "claude"
	Codex  Harness = "codex"
)

// Kind identifies the shape of a delivered credential.
type Kind string

// KindClaudeOAuthToken is a `claude setup-token` value, delivered to Claude
// Code as CLAUDE_CODE_OAUTH_TOKEN.
const KindClaudeOAuthToken Kind = "claude-oauth-token"
const KindClaudeAPIKey Kind = "claude-api-key"

// ClaudeTokenEnv is Claude Code's documented long-lived OAuth token input.
const ClaudeTokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"

// maxSecretBytes bounds a stored or delivered value well below the session
// mailbox response limit. Claude OAuth tokens are about 100 bytes.
const maxSecretBytes = 4096

var (
	// ErrNotConfigured means no central login exists for the harness yet.
	ErrNotConfigured = errors.New("central login not configured")
	// ErrDenied is the only detail a refused session request receives.
	ErrDenied = errors.New("central credential access denied")
	// ErrInvalidSecret rejects empty, oversized or non-printable values.
	ErrInvalidSecret = errors.New("invalid credential value")
	// ErrInsecureStore rejects a store whose permissions or file type expose it.
	ErrInsecureStore = errors.New("central credential store is not private")
)

// Secret holds a credential value. Every formatting and encoding path yields a
// redacted placeholder; Reveal is reserved for the single delivery sink.
type Secret struct{ value string }

// NewSecret validates a value: nonempty, bounded, printable, no whitespace.
func NewSecret(value string) (Secret, error) {
	if value == "" || len(value) > maxSecretBytes {
		return Secret{}, ErrInvalidSecret
	}
	for _, r := range value {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return Secret{}, ErrInvalidSecret
		}
	}
	return Secret{value: value}, nil
}

// Reveal returns the raw value. Call it only where the value leaves amux for
// its consumer (the harness environment, a store write, a mailbox response).
func (s Secret) Reveal() string { return s.value }

// Empty reports whether no value is held.
func (s Secret) Empty() bool { return s.value == "" }

// Fingerprint is a short non-reversible identifier for status and logs.
func (s Secret) Fingerprint() string {
	if s.value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(s.value))
	return hex.EncodeToString(sum[:4])
}

func (s Secret) String() string {
	if s.value == "" {
		return "[empty]"
	}
	return "[redacted " + s.Fingerprint() + "]"
}

func (s Secret) GoString() string { return s.String() }

// Format covers %v, %+v, %#v, %s, %q and %x alike.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.String())) }

func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + s.String() + `"`), nil }

// Credential is one harness credential as delivered to a session.
type Credential struct {
	Harness Harness
	Kind    Kind
	Token   Secret
	// StoredAt is when amux saved the credential. It is not an expiry: amux
	// records an expiry only when the issuer states one explicitly.
	StoredAt time.Time
	// Generation increases on every successful central login, so a consumer
	// can tell a reissued credential from the one it already holds.
	Generation uint64
}

// Issuer is what the daemon exposes to delivery paths. It authorizes the
// requesting session before reading any credential.
type Issuer interface {
	Issue(ctx context.Context, sessionID string, h Harness) (Credential, error)
}

// QueryVerb is the restricted session RPC query that delivers the central
// credential for the requesting session's own harness. HarnessField selects
// the harness; optional FormatField negotiates typed API-key delivery. The
// session identity comes from the signed request.
const (
	QueryVerb    = "central-credential"
	HarnessField = "harness"
	FormatField  = "credential-format"
	TypedFormat  = "typed-v1"
)

// Delivery is the mailbox response body. Token is the raw value: it exists
// only between the daemon's response and the launcher's exec.
type Delivery struct {
	Token      string `json:"token"`
	Kind       Kind   `json:"kind,omitempty"`
	Generation uint64 `json:"generation"`
}

// LauncherArg, as the first payload word, makes the protected payload
// trampoline run Launch: after inherited descriptors are marked close-on-exec,
// it fetches the session's credential over the mailbox and execs Claude.
const LauncherArg = "--amux-claude-central-exec"
