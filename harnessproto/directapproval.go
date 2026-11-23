package harnessproto

// DirectApprovalV1 is tailnet direct enrollment with a cloud approval
// (docs/direct-tailnet-enroll.md, contract "TAILNET DIRECT ENROLL v1"). It
// replaces the broker relay for brokered enrollment: Harness issues a
// single-use approval to an authenticated owner; the client posts it directly
// to the host's direct server (POST /direct/v1/enroll/cloud) over the tailnet;
// and the host verifies and consumes it with one provider-initiated frame on
// its existing provider connection. No enrollment payload is relayed.
//
// A provider lists DirectApprovalV1 in its features, and sets
// DirectCapabilities.Approval and .Ceiling, while its direct server accepts
// approvals. It re-registers when either changes.
//
// Frames (the first provider-initiated request on the connection):
//
//   - HDirectApprovalVerify (provider -> orchestrator, HarnessMsg) with ReqID
//     and DirectApproval. The provider id is the authenticated connection's,
//     never part of the body.
//   - MDirectApprovalResult (orchestrator -> provider, MuxMsg) with the same
//     ReqID and DirectApprovalResult.
//
// Neither frame may be queued durably, replayed or logged with its approval.
// The orchestrator answers within 10 s; a provider keeps at most 4 verifies in
// flight per connection.
const (
	DirectApprovalV1      = "direct-approval-v1"
	HDirectApprovalVerify = "direct-approval-verify"
	MDirectApprovalResult = "direct-approval-result"

	// DirectApprovalPrefix starts every approval: "hdap_" + 43 base64url
	// characters (32 random bytes).
	DirectApprovalPrefix = "hdap_"
)

// Verify refusal codes (DirectApprovalResult.Error).
const (
	DirectApprovalErrUnknown     = "approval_unknown"  // no such approval, or voided by a retry
	DirectApprovalErrExpired     = "approval_expired"  // past its expires_at
	DirectApprovalErrUsed        = "approval_used"     // consumed with another challenge
	DirectApprovalErrMismatch    = "approval_mismatch" // provider, server_id or challenge differ
	DirectApprovalErrOwnerChange = "owner_changed"     // the provider is no longer owned by the approver (or shared/revoked)
	DirectApprovalErrUnavailable = "unavailable"       // retry
)

// DirectCeiling is the host's cloud-issuance ceiling as advertised in
// DirectCapabilities.Ceiling. Resource is DirectResourceAllSessions (never
// including the amux console) or DirectResourceWorkgroups with Workgroups.
type DirectCeiling struct {
	Permissions []string `json:"permissions"`
	Resource    string   `json:"resource"`
	Workgroups  []string `json:"workgroups,omitempty"`
}

// DirectApprovalVerify is HarnessMsg.DirectApproval: the approval a client
// presented, the serving direct server's id, and the client's S256 challenge.
type DirectApprovalVerify struct {
	Approval      string `json:"approval"`
	ServerID      string `json:"serverId"`
	CodeChallenge string `json:"codeChallenge"`
}

// DirectApprovalResult is MuxMsg.DirectApprovalResult: on success the
// approval's bound grant (Requester is the owner user id, checked by the host
// against its owner pin), otherwise Error alone.
type DirectApprovalResult struct {
	Requester   string          `json:"requester,omitempty"`
	Permissions []string        `json:"permissions,omitempty"`
	Resource    *DirectResource `json:"resource,omitempty"`
	Label       string          `json:"label,omitempty"`
	Handoff     string          `json:"handoff,omitempty"`
	ExpiresAt   int64           `json:"expiresAt,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// ValidDirectApproval reports whether s has an approval's shape.
func ValidDirectApproval(s string) bool {
	if len(s) != len(DirectApprovalPrefix)+43 || s[:len(DirectApprovalPrefix)] != DirectApprovalPrefix {
		return false
	}
	for i := len(DirectApprovalPrefix); i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
