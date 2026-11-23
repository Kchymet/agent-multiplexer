package direct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// POST /direct/v1/enroll/cloud (docs/direct-tailnet-enroll.md §4): a client
// on the tailnet presents a Harness approval and its own S256 challenge; the
// host verifies and consumes the approval through the provider (§5), checks
// its own pins and ceiling, and mints a cloud PKCE code bound to the
// challenge, which the client redeems at the token endpoint as usual.
//
// Order: cloud policy on -> local replay -> verify -> owner/provider pins ->
// ceiling -> mint. Minting is idempotent per approval hash: the same approval
// with the same challenge returns the same code until it is redeemed or
// expires. A device-ticket (handoff) approval is unbound; the first device to
// present it binds its own challenge, at Harness and here (the QR v3 path).

// PathEnrollCloud is the approval endpoint.
const PathEnrollCloud = "/direct/v1/enroll/cloud"

// EnrollStepCloud is EnrollAttempt.Step for an approval exchange.
const EnrollStepCloud = "enroll_cloud"

// OAuthAccessDenied is the error of a refused approval.
const OAuthAccessDenied = "access_denied"

// Approval refusal reasons beyond the shared Reason* values (OAuthError.Reason
// on PathEnrollCloud).
const (
	ReasonMalformed               = "malformed"
	ReasonOwnerMismatch           = "owner_mismatch"
	ReasonProviderMismatch        = "provider_mismatch"
	ReasonApprovalUnknown         = "approval_unknown"
	ReasonApprovalMismatch        = "approval_mismatch"
	ReasonCloudDisabled           = "cloud_disabled"
	ReasonRateLimited             = "rate_limited"
	ReasonOrchestratorUnreachable = "orchestrator_unreachable"
	ReasonUnavailable             = "unavailable"
)

// EnrollCloudResponse is the 200 body.
type EnrollCloudResponse struct {
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expires_at"`
	ServerID  string `json:"server_id"`
}

// approvalRefusal is a typed refusal: HTTP status, OAuth error and reason.
type approvalRefusal struct {
	status int
	err    string
	reason string
}

func (e *approvalRefusal) Error() string { return e.err + ": " + e.reason }

func refused(reason string) error {
	return &approvalRefusal{status: http.StatusForbidden, err: OAuthAccessDenied, reason: reason}
}

func approvalUnavailable(reason string) error {
	return &approvalRefusal{status: http.StatusServiceUnavailable, err: OAuthTemporarilyUnavail, reason: reason}
}

var (
	errApprovalMalformed = &approvalRefusal{status: http.StatusBadRequest, err: OAuthInvalidRequest, reason: ReasonMalformed}
	errApprovalRate      = &approvalRefusal{status: http.StatusTooManyRequests, err: OAuthTemporarilyUnavail, reason: ReasonRateLimited}
)

// approvalIssueKey is the cloud issue key for an approval: its SHA-256, so
// replay material is keyed by approval hash and the approval itself is never
// stored.
func approvalIssueKey(approval string) string {
	sum := sha256.Sum256([]byte(approval))
	return "ap_" + hex.EncodeToString(sum[:])
}

// approvalVerifier verifies one approval with Harness (production:
// ApprovalQueue through the provider).
type approvalVerifier interface {
	Verify(context.Context, hp.DirectApprovalVerify) (ApprovalVerdict, error)
}

// replayApproval answers a repeat presentation of an approval this host has
// already minted for, without asking Harness again: the same code for the
// same challenge while it is live, or why it can no longer be used. ok is
// false when the approval is new here.
func (a *Authority) replayApproval(key, challenge string) (code string, exp int64, ok bool, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	is := a.state.CloudIssues[hashToken(key)]
	if is == nil {
		return "", 0, false, nil
	}
	rec := a.state.Codes[is.CodeHash]
	now := a.now().Unix()
	c := a.state.Cloud
	switch {
	case rec == nil || rec.ConsumedAt != 0:
		return "", 0, true, refused(ReasonTicketUsed)
	case now >= rec.ExpiresAt || c == nil || rec.PolicyGen != c.Generation:
		return "", 0, true, refused(ReasonTicketExpired)
	case rec.Challenge != challenge:
		return "", 0, true, refused(ReasonTicketUsed)
	case is.Code == "":
		return "", 0, true, refused(ReasonTicketUsed)
	}
	return is.Code, rec.ExpiresAt, true, nil
}

func (a *Authority) cloudEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state.Cloud != nil && a.state.Cloud.Enabled
}

// verifyRefusals maps Harness's verify errors to the host's answer.
var verifyRefusals = map[string]error{
	hp.DirectApprovalErrUnknown:     refused(ReasonApprovalUnknown),
	hp.DirectApprovalErrExpired:     refused(ReasonTicketExpired),
	hp.DirectApprovalErrUsed:        refused(ReasonTicketUsed),
	hp.DirectApprovalErrMismatch:    refused(ReasonApprovalMismatch),
	hp.DirectApprovalErrOwnerChange: refused(ReasonOwnerMismatch),
	hp.DirectApprovalErrUnavailable: approvalUnavailable(ReasonUnavailable),
}

// issueRefusals maps a cloud issuance refusal to the host's answer.
var issueRefusals = map[string]error{
	CloudErrDisabled:         refused(ReasonCloudDisabled),
	CloudErrOwnerMismatch:    refused(ReasonOwnerMismatch),
	CloudErrProviderMismatch: refused(ReasonProviderMismatch),
	CloudErrScope:            refused(ReasonCeiling),
	CloudErrIdemConflict:     refused(ReasonTicketUsed),
	CloudErrIssueExpired:     refused(ReasonTicketUsed),
	CloudErrInvalidRequest:   refused(ReasonApprovalMismatch),
	CloudErrInvalidChallenge: errApprovalMalformed,
	CloudErrCapacity:         errApprovalRate,
	CloudErrRateLimited:      errApprovalRate,
	CloudErrUnavailable:      approvalUnavailable(ReasonUnavailable),
}

// EnrollWithApproval runs the approval exchange and returns the minted code.
func (a *Authority) EnrollWithApproval(ctx context.Context, v approvalVerifier, approval, challenge string) (string, int64, error) {
	if !hp.ValidDirectApproval(approval) || !validNonce(challenge) {
		return "", 0, errApprovalMalformed
	}
	if !a.cloudEnabled() {
		return "", 0, refused(ReasonCloudDisabled)
	}
	key := approvalIssueKey(approval)
	if code, exp, ok, err := a.replayApproval(key, challenge); ok {
		return code, exp, err
	}
	if v == nil {
		return "", 0, approvalUnavailable(ReasonOrchestratorUnreachable)
	}
	verdict, err := v.Verify(ctx, hp.DirectApprovalVerify{Approval: approval, ServerID: a.ServerID(), CodeChallenge: challenge})
	switch {
	case errors.Is(err, errApprovalBusy):
		return "", 0, approvalUnavailable(ReasonUnavailable)
	case err != nil:
		return "", 0, approvalUnavailable(ReasonOrchestratorUnreachable)
	}
	res := verdict.Result
	if res.Error != "" {
		if e, ok := verifyRefusals[res.Error]; ok {
			return "", 0, e
		}
		return "", 0, approvalUnavailable(ReasonUnavailable)
	}
	if res.Handoff != "" && res.Handoff != hp.DirectHandoffDeviceTicket {
		return "", 0, refused(ReasonApprovalMismatch)
	}
	if res.ExpiresAt != 0 && a.now().Unix() >= res.ExpiresAt {
		return "", 0, refused(ReasonTicketExpired)
	}
	req := CloudIssueRequest{
		IssueKey: key, Requester: res.Requester, ProviderID: verdict.ProviderID,
		CodeChallenge: challenge, CodeChallengeMethod: CodeChallengeMethodS256,
		Permissions: slices.Clone(res.Permissions), Label: res.Label,
	}
	if res.Resource != nil {
		req.Resource = ResourceScope{Kind: res.Resource.Kind, Workgroups: slices.Clone(res.Resource.Workgroups)}
	}
	if validLabel(req.Label) != nil {
		req.Label = "" // IssueCloudCode names it; a bad label never refuses an approval
	}
	// IssueCloudCode checks, in order: policy, owner pin, provider pin,
	// challenge, permissions and resource within the ceiling, then mints
	// idempotently by issue key (the approval hash).
	code, exp, err := a.IssueCloudCode(req)
	if err != nil {
		var ce *CloudError
		if errors.As(err, &ce) {
			if e, ok := issueRefusals[ce.Code]; ok {
				return "", 0, e
			}
		}
		return "", 0, approvalUnavailable(ReasonUnavailable)
	}
	return code, exp, nil
}

// enrollCloud serves PathEnrollCloud. Like the token and claim endpoints it is
// unauthenticated (the approval is the authority), form-encoded, takes no URL
// parameters and counts refusals toward the per-source limit. It never logs
// or echoes the approval.
func (s *Server) enrollCloud(w http.ResponseWriter, r *http.Request) {
	src := source(r)
	n := &exchangeNote{step: EnrollStepCloud}
	defer s.noteExchange(n)
	fail := func(e *approvalRefusal) {
		if e.status == http.StatusForbidden {
			s.recordFailure(src)
		}
		if e.status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "60")
		}
		n.refuse(w, e.status, e.err, "", e.reason)
	}
	if s.limited(src) {
		fail(errApprovalRate)
		return
	}
	vals, err := parseForm(w, r)
	if err != nil || vals.Get("code_challenge_method") != CodeChallengeMethodS256 {
		fail(errApprovalMalformed)
		return
	}
	var q approvalVerifier
	if s.cfg.Approvals != nil {
		q = s.cfg.Approvals
	}
	code, exp, err := s.cfg.Authority.EnrollWithApproval(r.Context(), q, vals.Get("approval"), vals.Get("code_challenge"))
	var ar *approvalRefusal
	switch {
	case errors.As(err, &ar):
		fail(ar)
	case err != nil:
		s.cfg.Logf("direct: enroll/cloud: %v", err)
		fail(approvalUnavailable(ReasonUnavailable).(*approvalRefusal))
	default:
		n.ok = true
		w.Header().Set("Pragma", "no-cache")
		writeJSON(w, http.StatusOK, EnrollCloudResponse{Code: code, ExpiresAt: exp, ServerID: s.cfg.Authority.ServerID()})
	}
}

// BundlePrefixApproval starts a QR v3 payload: a device-ticket approval
// Harness issued for another device (docs/direct-tailnet-enroll.md §3). The
// scanning device verifies the server itself, then posts the approval with its
// own challenge to PathEnrollCloud; the first device to do so binds it.
const BundlePrefixApproval = "amux-direct:3:"

// ApprovalBundle is the QR v3 payload.
type ApprovalBundle struct {
	V         int      `json:"v"`
	ServerID  string   `json:"server_id"`
	Endpoints []string `json:"endpoints"`
	Approval  string   `json:"approval"`
	ExpiresAt int64    `json:"expires_at"`
}

// EncodeApprovalBundle renders a QR v3 payload.
func EncodeApprovalBundle(b ApprovalBundle) (string, error) {
	b.V = 3
	data, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	return BundlePrefixApproval + base64.RawURLEncoding.EncodeToString(data), nil
}

// DecodeApprovalBundle parses and checks a QR v3 payload.
func DecodeApprovalBundle(s string, now time.Time) (ApprovalBundle, error) {
	if !strings.HasPrefix(s, BundlePrefixApproval) {
		return ApprovalBundle{}, errors.New("not an amux-direct v3 approval")
	}
	data, err := base64.RawURLEncoding.DecodeString(s[len(BundlePrefixApproval):])
	if err != nil {
		return ApprovalBundle{}, errors.New("approval bundle is not base64url")
	}
	var b ApprovalBundle
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return ApprovalBundle{}, fmt.Errorf("approval bundle: %w", err)
	}
	switch {
	case b.V != 3:
		return ApprovalBundle{}, errors.New("approval bundle: unsupported version")
	case b.ServerID == "" || len(b.Endpoints) == 0:
		return ApprovalBundle{}, errors.New("approval bundle: missing server_id or endpoints")
	case !hp.ValidDirectApproval(b.Approval):
		return ApprovalBundle{}, errors.New("approval bundle: malformed approval")
	case now.Unix() >= b.ExpiresAt:
		return ApprovalBundle{}, errors.New("approval bundle: expired")
	}
	for _, e := range b.Endpoints {
		if c, err := CanonicalOrigin(e, false); err != nil || c != e {
			return ApprovalBundle{}, fmt.Errorf("approval bundle: endpoint %q is not a canonical https origin", e)
		}
	}
	return b, nil
}
