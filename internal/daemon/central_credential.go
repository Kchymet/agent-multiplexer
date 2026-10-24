package daemon

import (
	"context"
	"encoding/json"

	"amux/internal/access"
	"amux/internal/agent"
	"amux/internal/amuxcfg"
	"amux/internal/centralauth"
	"amux/internal/claudecfg"
	"amux/internal/sessionrpc"
)

// authorizeCentralCredential admits a session's request for its own harness's
// central credential. The signed principal selects the session; the request
// may name only the harness, which must match the session's recorded harness
// and the configured mode. Every tab of the session shares one principal, so
// the grant is per session rather than per agent pane.
func (r *sessionRuntime) authorizeCentralCredential(ctx context.Context, principal access.Principal, req access.Request) error {
	if principal.Kind != access.SubjectSession || req.Route != access.RouteQuery ||
		req.ID != "" || req.Target != "" || req.Tab != 0 ||
		len(req.Fields) != 1 || req.Fields[centralauth.HarnessField] != string(centralauth.Claude) {
		return access.ErrDenied
	}
	if err := r.d.authority.Valid(ctx, principal); err != nil {
		return err
	}
	if mode, err := amuxcfg.AuthMode("claude"); err != nil || mode != amuxcfg.AuthCentral {
		return access.ErrDenied
	}
	return r.resolver.withStore(func(db policyStore) error {
		s, ok, err := lookupPolicySession(db, principal.SubjectID)
		if err != nil {
			return err
		}
		if !ok || s.Archived || agent.Canonical(s.Agent) != string(centralauth.Claude) {
			return access.ErrDenied
		}
		return nil
	})
}

// dispatchCentralCredential returns the central Claude token. It runs inside
// the lifecycle admission gate and re-authorizes after reading, so an archive
// or revocation that crosses the read never receives the value. Failures carry
// only a fixed code: no path, value or store detail reaches the session.
func (r *sessionRuntime) dispatchCentralCredential(ctx context.Context, request sessionrpc.DispatchRequest) (sessionrpc.DispatchResult, error) {
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	r.d.effectMu.Lock()
	defer r.d.effectMu.Unlock()
	ctx = withEffectAdmission(ctx)
	req := request.Call.AccessRequest()
	if err := r.authorizeCentralCredential(ctx, request.Principal, req); err != nil {
		return rpcDenied("credential_access_denied"), nil
	}
	cred, err := centralauth.ClaudeStore().Load()
	if err != nil {
		return rpcFailed("central_credential_unavailable"), nil
	}
	if err := r.authorizeCentralCredential(ctx, request.Principal, req); err != nil {
		return rpcDenied("credential_access_denied"), nil
	}
	body, err := json.Marshal(centralauth.Delivery{Token: cred.Token.Reveal(), Generation: cred.Generation})
	if err != nil || len(body) > sessionrpc.MaxResponseBody {
		return rpcFailed("central_credential_unavailable"), nil
	}
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body}, nil
}

// claudeKeychainAllowed reports whether the macOS Keychain broker may serve
// Claude's host store. Central and environment modes expose no host store, so
// legacy Keychain reads, writes and deletes are all refused.
func claudeKeychainAllowed() bool { return claudecfg.HostStoreShared() }
