package harnessproto

// DirectEnrollV1 is cloud-assisted direct enrollment (docs/direct-cloud-
// enrollment.md). A provider advertises it only while the host operator has
// enabled cloud issuance on this host's `amux direct serve`, and then answers
// exactly one request type: mint a PKCE-bound enrollment code for an
// authenticated owner. It carries no token, verifier or session data, and it
// is not a generic channel to the host's direct administration.
//
// The orchestrator sends direct-enroll with a ReqID; the provider answers one
// direct-enroll-result with the same ReqID. Neither frame may be queued
// durably, logged with its bundle, replayed or broadcast: the bundle carries a
// short-lived single-use code (useless without the client's verifier, but
// still not for logs). The orchestrator never dials the advertised endpoints.
const (
	DirectEnrollV1      = "direct-enroll-v1"
	MDirectEnroll       = "direct-enroll"
	HDirectEnrollResult = "direct-enroll-result"

	DirectEnrollMethodS256 = "S256"

	DirectResourceAllSessions = "all_sessions"
	DirectResourceWorkgroups  = "workgroups"
)

// Direct enrollment refusal codes (DirectEnrollResult.Error).
const (
	DirectEnrollErrDisabled         = "cloud_disabled"        // issuance is off (or the direct server is not running)
	DirectEnrollErrOwnerMismatch    = "owner_mismatch"        // requester is not the owner the host pinned
	DirectEnrollErrProviderMismatch = "provider_mismatch"     // this provider is not the one the host pinned
	DirectEnrollErrInvalidRequest   = "invalid_request"       // malformed request
	DirectEnrollErrInvalidChallenge = "invalid_challenge"     // not S256 over 32 bytes
	DirectEnrollErrScope            = "scope_exceeds_ceiling" // permissions or resource above the host's ceiling
	DirectEnrollErrIdemConflict     = "idem_conflict"         // issueKey reused for a different request
	DirectEnrollErrIssueExpired     = "issue_expired"         // issueKey's code was used or expired: start over
	DirectEnrollErrCapacity         = "capacity_exhausted"    // too many outstanding codes
	DirectEnrollErrRateLimited      = "rate_limited"          // hourly limit reached
	DirectEnrollErrBusy             = "busy"                  // provider has too many requests in flight
	DirectEnrollErrUnavailable      = "unavailable"           // outcome unknown or host unavailable: retry with the SAME issueKey
)

// DirectCapabilities advertises this host's direct server for discovery. It is
// present only while direct enrollment is offered. Endpoints are the canonical
// https origins clients dial; the client verifies them itself (trusted TLS and
// the signed hello naming ServerID). Ceiling and pins stay on the host.
type DirectCapabilities struct {
	ServerID  string   `json:"serverId"`
	Endpoints []string `json:"endpoints"`
	Issuance  bool     `json:"issuance"`
}

// DirectResource is a requested resource scope (all_sessions, or workgroups by
// root id). The host refuses anything above its configured ceiling.
type DirectResource struct {
	Kind       string   `json:"kind"`
	Workgroups []string `json:"workgroups,omitempty"`
}

// DirectEnrollRequest asks the host for one enrollment code. Requester is the
// authenticated Harness user the orchestrator vouches for, taken from its own
// session state, never from client input. IssueKey makes retries idempotent:
// a client that saw no answer retries with the same key and gets the same
// bundle back. The provider adds its own registered provider id.
type DirectEnrollRequest struct {
	IssueKey            string         `json:"issueKey"`
	Requester           string         `json:"requester"`
	CodeChallenge       string         `json:"codeChallenge"`
	CodeChallengeMethod string         `json:"codeChallengeMethod"`
	Permissions         []string       `json:"permissions"`
	Resource            DirectResource `json:"resource"`
	Label               string         `json:"label,omitempty"`
}

// DirectEnrollResult answers it: a paste bundle (amux-direct:1:…) on success,
// or a refusal code.
type DirectEnrollResult struct {
	Bundle    string `json:"bundle,omitempty"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
	ServerID  string `json:"serverId,omitempty"`
	Error     string `json:"error,omitempty"`
}
