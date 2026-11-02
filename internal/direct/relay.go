package direct

import (
	"context"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// AdminCaller sends one request over the host-proven admin channel
// (production: AdminCall with the protected host credential).
type AdminCaller func(context.Context, AdminRequest) (AdminResponse, error)

// RelayIssuer is the provider's only bridge from relay frames to direct
// administration. It can send exactly two operations: OpCloudStatus, when the
// provider registers, to decide whether to advertise direct-enroll-v1; and
// OpIssueCloudCode, built field by field from a typed relay request. There is
// no way to reach any other admin operation through it, and refusal messages
// from the server are reduced to their stable code.
type RelayIssuer struct{ call AdminCaller }

// NewRelayIssuer wraps an admin caller.
func NewRelayIssuer(call AdminCaller) *RelayIssuer { return &RelayIssuer{call: call} }

// Capabilities reports the direct server for advertisement, or nil when cloud
// issuance is not enabled or the server is not running.
func (r *RelayIssuer) Capabilities(ctx context.Context) (*hp.DirectCapabilities, error) {
	resp, err := r.call(ctx, AdminRequest{Op: OpCloudStatus})
	if err != nil {
		return nil, err
	}
	if resp.Cloud == nil || !resp.Cloud.Enabled || resp.ServerID == "" || len(resp.Endpoints) == 0 {
		return nil, nil
	}
	return &hp.DirectCapabilities{ServerID: resp.ServerID, Endpoints: append([]string(nil), resp.Endpoints...), Issuance: true, Handoff: true}, nil
}

// relayCodes are the refusal codes passed back over the relay. Anything else
// (including transport failures) is "unavailable": the outcome is unknown and
// the client retries with the same issue key, which never mints twice.
var relayCodes = map[string]string{
	CloudErrDisabled:         hp.DirectEnrollErrDisabled,
	CloudErrOwnerMismatch:    hp.DirectEnrollErrOwnerMismatch,
	CloudErrProviderMismatch: hp.DirectEnrollErrProviderMismatch,
	CloudErrInvalidRequest:   hp.DirectEnrollErrInvalidRequest,
	CloudErrInvalidChallenge: hp.DirectEnrollErrInvalidChallenge,
	CloudErrScope:            hp.DirectEnrollErrScope,
	CloudErrIdemConflict:     hp.DirectEnrollErrIdemConflict,
	CloudErrIssueExpired:     hp.DirectEnrollErrIssueExpired,
	CloudErrCapacity:         hp.DirectEnrollErrCapacity,
	CloudErrRateLimited:      hp.DirectEnrollErrRateLimited,
	CloudErrUnavailable:      hp.DirectEnrollErrUnavailable,
}

// Enroll asks the direct server for one cloud code. providerID is the
// provider's own registered id, never a value from the relayed frame.
func (r *RelayIssuer) Enroll(ctx context.Context, providerID string, req hp.DirectEnrollRequest) hp.DirectEnrollResult {
	issue := CloudIssueRequest{
		IssueKey: req.IssueKey, Requester: req.Requester, ProviderID: providerID,
		CodeChallenge: req.CodeChallenge, CodeChallengeMethod: req.CodeChallengeMethod,
		Permissions: append([]string(nil), req.Permissions...),
		Resource:    ResourceScope{Kind: req.Resource.Kind, Workgroups: append([]string(nil), req.Resource.Workgroups...)},
		Label:       req.Label,
		Handoff:     req.Handoff,
	}
	resp, err := r.call(ctx, AdminRequest{Op: OpIssueCloudCode, Issue: &issue})
	if err == nil && resp.OK && resp.Bundle != "" {
		return hp.DirectEnrollResult{Bundle: resp.Bundle, ExpiresAt: resp.ExpiresAt, ServerID: resp.ServerID}
	}
	if code, ok := relayCodes[resp.Code]; ok {
		return hp.DirectEnrollResult{Error: code}
	}
	return hp.DirectEnrollResult{Error: hp.DirectEnrollErrUnavailable}
}
