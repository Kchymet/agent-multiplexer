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
	// PathWorkgroups creates a new workgroup root (permission create, and only
	// for an all_sessions grant). PathCapabilities is the grant's own verb set
	// and the machine's allowlisted creation catalog (permission read).
	PathWorkgroups   = "/direct/v1/workgroups"
	PathCapabilities = "/direct/v1/capabilities"
	// PathCreations is GET PathCreations/{creation_id}: the phase of one
	// creation this grant started (permission create). See CreationStatus.
	PathCreations = "/direct/v1/creations"
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

// Permissions. A grant always has read; every other permission is separate
// and explicit. The set is additive: a permission's meaning never widens, so a
// grant, code or cloud ceiling issued before a permission existed can never
// acquire it. steer is prompt, interject and stop (#295 added interject to it).
//
//   - respond: goal control
//   - approve: answer a correlated permission request (allow / deny)
//   - manage: start, rename, archive and unarchive in-scope sessions
//   - create: add-agent to an in-scope workgroup; new-workgroup and
//     new-workgroup-empty for an all_sessions grant only
const (
	PermRead    = "read"
	PermSteer   = "steer"
	PermRespond = "respond" // goal control
	PermApprove = "approve"
	PermManage  = "manage"
	PermCreate  = "create"
)

// Permissions is the canonical order permission lists are normalized to.
var Permissions = []string{PermRead, PermSteer, PermRespond, PermApprove, PermManage, PermCreate}

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

// Action verbs. prompt, interject and stop need steer (interject joined steer
// in #295); the rest each need their own additive permission
// (VerbPermissions). Everything else fails closed.
const (
	VerbPrompt     = "prompt"
	VerbStop       = "stop"
	VerbInterject  = "interject"
	VerbGoal       = "goal"
	VerbPermission = "permission"
	VerbStart      = "start"
	VerbRename     = "rename"
	VerbArchive    = "archive"
	VerbUnarchive  = "unarchive"
	VerbAddAgent   = "add-agent"
	// Root creation verbs, sent to PathWorkgroups rather than a session.
	VerbNewWorkgroup      = "new-workgroup"
	VerbNewWorkgroupEmpty = "new-workgroup-empty"
)

// ServerVerbs lists every verb this server implements, session verbs first,
// then the root creation verbs.
func ServerVerbs() []string {
	return []string{VerbPrompt, VerbStop, VerbInterject, VerbGoal, VerbPermission, VerbStart, VerbRename,
		VerbArchive, VerbUnarchive, VerbAddAgent, VerbNewWorkgroup, VerbNewWorkgroupEmpty}
}

// VerbPermissions maps every admitted verb to the one permission it needs.
var VerbPermissions = map[string]string{
	VerbPrompt: PermSteer, VerbStop: PermSteer, VerbInterject: PermSteer,
	VerbGoal:       PermRespond,
	VerbPermission: PermApprove,
	VerbStart:      PermManage, VerbRename: PermManage, VerbArchive: PermManage, VerbUnarchive: PermManage,
	VerbAddAgent: PermCreate, VerbNewWorkgroup: PermCreate, VerbNewWorkgroupEmpty: PermCreate,
}

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
	CodeNotInCatalog     = "not_in_catalog" // a creation names a harness or repo this machine/target does not offer
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
// verb is true only when the daemon can serve it for this row, the row's
// archive state allows it, AND the grant holds the verb's permission. The
// fields after Stop are additive and omitted when false, so a grant without
// the newer permissions sees exactly the v1 fixture bytes.
type SessionCaps struct {
	Prompt     bool `json:"prompt"`
	Interject  bool `json:"interject"` // S1 field order and tag, as pinned by fixtures/v1
	Stop       bool `json:"stop"`
	Goal       bool `json:"goal,omitempty"`
	Permission bool `json:"permission,omitempty"`
	Start      bool `json:"start,omitempty"`
	Rename     bool `json:"rename,omitempty"`
	Archive    bool `json:"archive,omitempty"`
	Unarchive  bool `json:"unarchive,omitempty"`
	AddAgent   bool `json:"add_agent,omitempty"`
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
//
// Async (optional, creation verbs only: add-agent, new-workgroup,
// new-workgroup-empty) asks for an asynchronous creation: the answer is 202
// with a CreationStatus, never an ActionResponse. Async is not part of the
// request's identity: the same idem_key names the same creation whether it is
// sent synchronously or not. A server without async creation refuses the field
// with 400 bad_request (CapabilitiesResponse.AsyncCreate tells in advance).
type ActionRequest struct {
	Verb    string            `json:"verb"`
	Fields  map[string]string `json:"fields"`
	IdemKey string            `json:"idem_key"`
	Async   bool              `json:"async,omitempty"`
}

// Creation phases, in order; ready and failed are terminal.
//   - queued: recorded, waiting for an execution slot (nothing sent yet);
//   - preparing: handed to the daemon, which is fetching the repo, creating
//     the worktree, writing the harness config and starting the agent;
//   - ready: created; NewID and RootID are set;
//   - failed: Code and Error say why; Uncertain marks an unknown outcome.
const (
	CreatePhaseQueued    = "queued"
	CreatePhasePreparing = "preparing"
	CreatePhaseReady     = "ready"
	CreatePhaseFailed    = "failed"
)

// CreationStatus is the body of an async creation's 202 answer and of
// GET PathCreations/{creation_id}. It never carries "ok": a client treats a
// creation as made only when Phase is ready, and the server never reports
// ready without both NewID and RootID.
//
// Version increases with each phase change (queued 1, preparing 2, terminal
// 3); pass it back as ?after= to long-poll for the next change.
//
// Uncertain (on failed, code idem_ambiguous) means the outcome is unknown: the
// daemon may have created it (the transport was lost after dispatch, the
// bound ran out, the server restarted after dispatch, or access was revoked
// mid-creation). It will never run again with this idem_key; check the
// inventory before retrying with a new one. A failed creation without
// Uncertain did not run (unavailable, auth.revoked, auth.expired, a
// precheck refusal) or was refused by the daemon (rejected).
type CreationStatus struct {
	CreationID string `json:"creation_id"`
	Verb       string `json:"verb"`
	Phase      string `json:"phase"`
	Version    int    `json:"version"`
	NewID      string `json:"new_id,omitempty"`
	RootID     string `json:"root_id,omitempty"`
	Code       string `json:"code,omitempty"`
	Error      string `json:"error,omitempty"`
	Uncertain  bool   `json:"uncertain,omitempty"`
	ElapsedMS  int64  `json:"elapsed_ms"`
}

// ActionResponse is a successful action result. NewID is the session a
// creation verb made (add-agent: the agent; new-workgroup*: the root); it is
// recorded with the idempotency key, so a replay returns the same id.
//
// Affected, on archive and unarchive, lists every session whose effective
// archive state the action changes, judged from the fresh inventory it was
// admitted against: the target, plus for a workgroup root each member that is
// not archived in its own right. Members archived themselves stay archived
// after a root unarchive and are not listed.
//
// RootID accompanies NewID: the workgroup root the new session belongs to (the
// add-agent target, or NewID itself for a new workgroup), so a client can
// navigate before the next inventory publishes the row.
type ActionResponse struct {
	OK       bool     `json:"ok"`
	NewID    string   `json:"new_id,omitempty"`
	RootID   string   `json:"root_id,omitempty"`
	Affected []string `json:"affected,omitempty"`
}

// CreateRequest is POST /direct/v1/workgroups: VerbNewWorkgroup or
// VerbNewWorkgroupEmpty with an exact field allowlist.
type CreateRequest = ActionRequest

// CapabilitiesResponse is GET /direct/v1/capabilities: what this grant may do
// on this machine. Verbs are the session verbs the grant's permissions admit
// (each row's caps still decide per session). CreateWorkgroup reports whether
// PathWorkgroups is open to this grant. Catalog is present only with create.
type CapabilitiesResponse struct {
	ServerID        string           `json:"server_id"`
	Permissions     []string         `json:"permissions"`
	ResourceScope   ResourceScope    `json:"resource_scope"`
	ServerVerbs     []string         `json:"server_verbs"`
	Verbs           []string         `json:"verbs"`
	CreateWorkgroup bool             `json:"create_workgroup"`
	Catalog         *CreationCatalog `json:"catalog,omitempty"`
	// AsyncCreate reports that this server accepts ActionRequest.Async and
	// PathCreations for this grant (it holds create).
	AsyncCreate bool `json:"async_create,omitempty"`
}

// CreationCatalog is the allowlisted, machine-appropriate input space for the
// creation verbs. It never carries repository sources, host paths, account
// bindings, credentials or configuration: harness names and versions verified
// on this machine, the machine identity mode, the session modes, and repo
// names. Repos (tracked repo names) is present only for a grant that may create
// workgroups; Workgroups lists each in-scope workgroup's repo ceiling for
// add-agent.
type CreationCatalog struct {
	Harnesses     []CatalogHarness   `json:"harnesses"`
	IdentityModes []string           `json:"identity_modes"`
	Modes         []string           `json:"modes"`
	Repos         []string           `json:"repos,omitempty"`
	Workgroups    []CatalogWorkgroup `json:"workgroups,omitempty"`
}

// CatalogHarness is one harness this machine verified it can launch, with the
// model names its local picker offers (sanitized names only; a creation's
// model field is not limited to this list, which may lag the runtime).
type CatalogHarness struct {
	Name    string   `json:"name"`
	Version string   `json:"version,omitempty"`
	Models  []string `json:"models,omitempty"`
}

// CatalogWorkgroup is one in-scope workgroup root and the repo names its
// members may be given (its coordinator's grant ceiling).
type CatalogWorkgroup struct {
	ID    string   `json:"id"`
	Repos []string `json:"repos"`
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
	MaxNameBytes     = 128  // rename / workgroup name
	MaxFieldBytes    = 256  // ids, models, request ids, runtime generations
	MaxReasonBytes   = 4096 // permission reason
)
