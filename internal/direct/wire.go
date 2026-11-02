// Package direct is amux's opt-in direct client access: a listener that web and
// Android clients reach over an existing LAN or VPN route, with no cloud in the
// data path. It is the remote authorization boundary. Every request passes a
// destination-issued grant check, and only then reaches the elevated local
// HostDaemon. The local daemon's authentication and the Harness relay wire are
// not changed by this package.
//
// The wire contract lives in this file and is pinned by the fixtures under
// docs/direct/fixtures (see docs/direct-access.md).
package direct

import (
	harnessproto "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Protocol is the direct protocol identifier a client names in hello.
const Protocol = "amux-direct/1"

// Path prefixes and routes.
const (
	PathHello   = "/direct/v1/hello"
	PathToken   = "/direct/v1/token"
	PathRevoke  = "/direct/v1/revoke"
	PathClaim   = "/direct/v1/enroll/claim"
	PathSession = "/direct/v1/sessions"
	PathWatch   = "/direct/v1/sessions/watch"
)

// HelloType is the JWS "typ" header of a hello proof.
const HelloType = "amux-direct-hello+jws"

// Hello bounds.
const (
	HelloLifetimeSeconds = 60 // exp - iat
	HelloMaxSkewSeconds  = 60 // clients accept iat within ±this of their clock
	NonceBytes           = 32
)

// GrantTypeEnrollmentCode is the RFC 6749 §4.5 extension grant redeeming a
// pasted enrollment code. GrantTypeRefresh is RFC 6749 §6.
const (
	GrantTypeEnrollmentCode = "urn:amux:params:oauth:grant-type:enrollment-code"
	GrantTypeRefresh        = "refresh_token"
)

// BundlePrefix starts a paste bundle: BundlePrefix + base64url(JSON Bundle).
// BundlePrefixTicket starts a device-ticket bundle (Bundle.V 2): its code must
// be claimed with the redeeming device's own S256 challenge (PathClaim) before
// it is redeemed with that device's verifier. A v1-only client refuses it.
const (
	BundlePrefix       = "amux-direct:1:"
	BundlePrefixTicket = "amux-direct:2:"
)

// BundleClaimPKCE is Bundle.Claim on a v2 (device ticket) bundle.
const BundleClaimPKCE = "pkce"

// Token prefixes. Every token carries at least 32 random bytes after it.
const (
	AccessTokenPrefix  = "amxa_"
	RefreshTokenPrefix = "amxr_"
)

// Permissions. A grant always has read; steer is separate and explicit.
const (
	PermRead  = "read"
	PermSteer = "steer"
)

// Resource scope kinds. There is no implicit default: enrollment names one.
const (
	ResourceAllSessions = "all_sessions"
	ResourceWorkgroups  = "workgroups"
)

// Client kinds a client names when it enrolls (display only, never authority).
const (
	ClientWeb     = "web"
	ClientAndroid = "android"
)

// Action verbs admitted in S1. Everything else fails closed.
const (
	VerbPrompt = "prompt"
	VerbStop   = "stop"
)

// Resource API error codes (JSON body {"code","error"}).
const (
	CodeAuthInvalid      = "auth.invalid"
	CodeAuthExpired      = "auth.expired"
	CodeAuthRevoked      = "auth.revoked"
	CodeDenied           = "denied"
	CodeNotFound         = "not_found"
	CodeBadRequest       = "bad_request"
	CodeUnsupportedVerb  = "unsupported_verb"
	CodeArchived         = "archived"
	CodeIdemConflict     = "idem_conflict"
	CodeIdemInProgress   = "idem_in_progress"
	CodeIdemAmbiguous    = "idem_ambiguous"
	CodeEndpointNotBound = "endpoint_not_bound"
	CodeOriginNotAllowed = "origin_not_allowed"
	CodeProtocol         = "protocol_unsupported"
	CodeRateLimited      = "rate_limited"
	CodeCapacity         = "capacity_exhausted" // the grant's action records are full: enroll again
	CodeUnavailable      = "unavailable"
	CodeRejected         = "rejected"
	CodeInternal         = "internal"
)

// OAuth token endpoint errors (RFC 6749 §5.2 body {"error","error_description"}).
const (
	OAuthInvalidRequest       = "invalid_request"
	OAuthInvalidGrant         = "invalid_grant"
	OAuthUnsupportedGrantType = "unsupported_grant_type"
	OAuthTemporarilyUnavail   = "temporarily_unavailable"
)

// WebSocket close codes. The close reason is the matching code string.
const (
	CloseAuthInvalid = 4001 // "auth.invalid": unknown token (e.g. server restart); one serialized refresh, then reconnect
	CloseAuthExpired = 4002 // "auth.expired": refresh, then reconnect
	CloseAuthRevoked = 4401 // "auth.revoked": the grant is gone; enroll again
	CloseDenied      = 4403 // "denied": the grant lacks the permission
)

// CloseCodes is the fixture form of the close-code table.
var CloseCodes = map[string]int{
	CodeAuthInvalid: CloseAuthInvalid, CodeAuthExpired: CloseAuthExpired,
	CodeAuthRevoked: CloseAuthRevoked, CodeDenied: CloseDenied,
}

// HelloRequest is the body of POST /direct/v1/hello.
type HelloRequest struct {
	Protocol    string `json:"protocol"`
	ClientNonce string `json:"client_nonce"` // base64url (no padding) of 32 random bytes
	Endpoint    string `json:"endpoint"`     // the exact canonical origin the client dialed
}

// HelloResponse is the response: an ES256 compact JWS over HelloClaims.
type HelloResponse struct {
	Proof string `json:"proof"`
}

// HelloClaims is the JWS payload of a hello proof.
type HelloClaims struct {
	ServerID    string `json:"server_id"` // RFC 7638 SHA-256 thumbprint of the header jwk, base64url
	Protocol    string `json:"protocol"`
	Endpoint    string `json:"endpoint"`
	ClientNonce string `json:"client_nonce"`
	IssuedAt    int64  `json:"iat"`
	ExpiresAt   int64  `json:"exp"`
}

// Bundle is the decoded paste bundle an operator gives a client.
type Bundle struct {
	V         int      `json:"v"`
	ServerID  string   `json:"server_id"`
	Endpoints []string `json:"endpoints"`
	Code      string   `json:"code"`       // base64url of 32 random bytes; single use
	ExpiresAt int64    `json:"expires_at"` // unix seconds
	// Claim is BundleClaimPKCE on a v2 bundle and absent on v1.
	Claim string `json:"claim,omitempty"`
}

// ClaimResponse is the PathClaim success body.
type ClaimResponse struct {
	Claimed   bool  `json:"claimed"`
	ExpiresAt int64 `json:"expires_at"`
}

// ResourceScope is a grant's resource scope, returned as structured metadata.
type ResourceScope struct {
	Kind       string   `json:"kind"`                 // all_sessions | workgroups
	Workgroups []string `json:"workgroups,omitempty"` // root ids, for kind workgroups
}

// TokenResponse is the RFC 6749 §5.1 success body of POST /direct/v1/token.
type TokenResponse struct {
	AccessToken    string        `json:"access_token"`
	TokenType      string        `json:"token_type"` // "Bearer"
	ExpiresIn      int64         `json:"expires_in"` // seconds
	RefreshToken   string        `json:"refresh_token"`
	Scope          string        `json:"scope"` // space-separated permissions, e.g. "read steer"
	ClientID       string        `json:"client_id"`
	ServerID       string        `json:"server_id"`
	GrantExpiresAt int64         `json:"grant_expires_at"` // unix seconds: absolute grant expiry
	ResourceScope  ResourceScope `json:"resource_scope"`
}

// OAuthError is the RFC 6749 §5.2 error body of the token endpoint.
type OAuthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

// APIError is the error body of every resource route.
type APIError struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// SessionCaps is the per-session control surface a direct client may use: a
// verb is true only when the daemon can serve it AND the grant may steer.
type SessionCaps struct {
	Prompt bool `json:"prompt"`
	Stop   bool `json:"stop"`
}

// Session is one inventory row, an allowlisted projection of core.Session.
// Host paths, process ids, account bindings and local attach affordances are
// never sent.
type Session struct {
	ID          string                   `json:"id"`
	Title       string                   `json:"title"`
	Kind        string                   `json:"kind,omitempty"`
	Mode        string                   `json:"mode,omitempty"`
	Model       string                   `json:"model,omitempty"`
	RootID      string                   `json:"root_id,omitempty"`
	IsRoot      bool                     `json:"is_root,omitempty"`
	Repos       string                   `json:"repos,omitempty"`
	Section     string                   `json:"section,omitempty"`
	State       string                   `json:"state,omitempty"`
	Status      string                   `json:"status,omitempty"`
	Archived    bool                     `json:"archived,omitempty"`
	StartedAt   int64                    `json:"started_at,omitempty"`
	Runtime     string                   `json:"runtime,omitempty"`
	ControlMode string                   `json:"control_mode,omitempty"`
	Role        string                   `json:"role,omitempty"`
	Caps        SessionCaps              `json:"caps"`
	PRs         []harnessproto.SessionPR `json:"prs,omitempty"`
}

// SessionsResponse is GET /direct/v1/sessions.
type SessionsResponse struct {
	ServerID string    `json:"server_id"`
	Sessions []Session `json:"sessions"`
}

// RuntimeEvent is one transcript event, in amux's runtime-event vocabulary.
type RuntimeEvent = harnessproto.RuntimeEvent

// HistoryResponse is GET /direct/v1/sessions/{sid}/history. Events are
// ascending; Seq is the ordinal of the last event (0 for an empty page), HeadSeq
// the session's newest ordinal, HasMore whether older events exist.
type HistoryResponse struct {
	SessionID string         `json:"session_id"`
	Runtime   string         `json:"runtime,omitempty"`
	Seq       int64          `json:"seq"`
	HeadSeq   int64          `json:"head_seq"`
	HasMore   bool           `json:"has_more"`
	Events    []RuntimeEvent `json:"events"`
}

// StreamBatch is the data of one SSE "events" event on the stream route. Seq is
// the ordinal of the last event; reconnect with ?after=<Seq>.
type StreamBatch struct {
	Seq     int64          `json:"seq"`
	Runtime string         `json:"runtime,omitempty"`
	Events  []RuntimeEvent `json:"events"`
}

// StreamEnd is the data of a terminal SSE "auth" or "end" event.
type StreamEnd struct {
	Code string `json:"code"`
}

// ActionRequest is POST /direct/v1/sessions/{sid}/actions.
type ActionRequest struct {
	Verb    string            `json:"verb"`
	Fields  map[string]string `json:"fields"`
	IdemKey string            `json:"idem_key"`
}

// ActionResponse is a successful action result.
type ActionResponse struct {
	OK bool `json:"ok"`
}

// Watch frames (WebSocket /direct/v1/sessions/watch). The client's first frame
// is WatchAuth; nothing is sent before it is verified.
type WatchAuth struct {
	Type        string `json:"type"` // "auth"
	AccessToken string `json:"access_token"`
}

// WatchWelcome is the server's first frame after authentication.
type WatchWelcome struct {
	Type      string `json:"type"` // "welcome"
	ServerID  string `json:"server_id"`
	ExpiresAt int64  `json:"expires_at"` // unix seconds: the socket closes 4002 then
}

// WatchSnapshot carries the full filtered inventory whenever it changes.
type WatchSnapshot struct {
	Type     string    `json:"type"` // "snapshot"
	Sessions []Session `json:"sessions"`
}

// Limits.
const (
	MaxJSONBody      = 64 << 10
	MaxFormBody      = 8 << 10
	MaxPromptBytes   = 32 << 10
	MaxWatchAuthSize = 4 << 10
	MaxIdemKeyLen    = 128
	MaxLabelLen      = 64
	MaxHistoryLimit  = 500
)
