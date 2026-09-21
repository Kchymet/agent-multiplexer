package daemon

import (
	"context"
	"encoding/json"
	"os"
	"slices"

	"amux/internal/access"
	"amux/internal/agent"
	"amux/internal/claudecfg"
	"amux/internal/credentialbroker"
	"amux/internal/sessionrpc"
)

// The verified session identity selects the harness. Host configuration selects
// the account. A request cannot name another session, keychain, or auth store.
func (r *sessionRuntime) authorizeCredential(ctx context.Context, principal access.Principal, req access.Request) error {
	if principal.Kind != access.SubjectSession || req.ID != "" || req.Target != "" || req.Tab != 0 {
		return access.ErrDenied
	}
	if err := r.d.authority.Valid(ctx, principal); err != nil {
		return err
	}
	o, err := credentialbroker.FromFields(req.Verb, req.Fields)
	if err != nil {
		return access.ErrDenied
	}
	if (o.Verb == credentialbroker.Read && req.Route != access.RouteQuery) ||
		(o.Verb != credentialbroker.Read && req.Route != access.RouteAction) {
		return access.ErrDenied
	}
	github := credentialbroker.GitHubAllowed(o)
	if !github && (o.Account != credentialbroker.HostAccount() || !slices.Contains(credentialbroker.ClaudeServices(claudecfg.CredentialSelector()), o.Service)) {
		return access.ErrDenied
	}
	return r.resolver.withStore(func(db policyStore) error {
		s, ok, err := lookupPolicySession(db, principal.SubjectID)
		if err != nil {
			return err
		}
		if !ok || s.Archived || (!github && agent.Canonical(s.Agent) != "claude") {
			return access.ErrDenied
		}
		return nil
	})
}

func (r *sessionRuntime) dispatchCredential(ctx context.Context, request sessionrpc.DispatchRequest) (sessionrpc.DispatchResult, error) {
	// Keep the short, bounded native credential operation within the lifecycle
	// admission gate so an archive, role change, or revoke cannot cross a write.
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	r.d.effectMu.Lock()
	defer r.d.effectMu.Unlock()
	ctx = withEffectAdmission(ctx)
	req := request.Call.AccessRequest()
	if err := r.authorizeCredential(ctx, request.Principal, req); err != nil {
		return rpcDenied("credential_access_denied"), nil
	}
	o, _ := credentialbroker.FromFields(req.Verb, req.Fields)
	execute := r.credentialOperation
	if execute == nil {
		execute = (credentialbroker.Keychain{Env: os.Environ()}).Execute
		if _, github := credentialbroker.GitHubHost(o); github {
			execute = (credentialbroker.GitHub{Env: os.Environ()}).Execute
		}
	}
	result, err := execute(ctx, o)
	if err != nil {
		return rpcFailed("credential_service_failed"), nil
	}
	if err := r.authorizeCredential(ctx, request.Principal, req); err != nil {
		return rpcDenied("credential_access_denied"), nil
	}
	body, err := json.Marshal(result)
	if err != nil || len(body) > sessionrpc.MaxResponseBody {
		return rpcFailed("credential_service_failed"), nil
	}
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body}, nil
}
