package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"amux/internal/access"
	"amux/internal/accounts"
	"amux/internal/agent"
	"amux/internal/amuxcfg"
	"amux/internal/centralauth"
	"amux/internal/claudecfg"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

// authorizeCentralCredential admits a session's request for its own harness's
// central credential. The signed principal selects the session; the request
// may name only the harness, which must match the session's recorded harness
// and the configured mode. Every tab of the session shares one principal, so
// the grant is per session rather than per agent pane.
func (r *sessionRuntime) authorizeCentralCredential(ctx context.Context, principal access.Principal, req access.Request) error {
	_, err := r.centralCredentialSession(ctx, principal, req)
	return err
}

// centralCredentialSession authorizes the request and returns the session it
// was authorized for, read through the side-effect-free policy store.
func (r *sessionRuntime) centralCredentialSession(ctx context.Context, principal access.Principal, req access.Request) (store.Session, error) {
	if principal.Kind != access.SubjectSession || req.Route != access.RouteQuery ||
		req.ID != "" || req.Target != "" || req.Tab != 0 ||
		!(len(req.Fields) == 1 || (len(req.Fields) == 2 && req.Fields[centralauth.FormatField] == centralauth.TypedFormat)) || req.Fields[centralauth.HarnessField] != string(centralauth.Claude) {
		return store.Session{}, access.ErrDenied
	}
	if err := r.d.authority.Valid(ctx, principal); err != nil {
		return store.Session{}, err
	}
	if mode := accounts.Mode(principal.SubjectID, "claude"); mode != amuxcfg.AuthCentral {
		return store.Session{}, access.ErrDenied
	}
	var session store.Session
	err := r.resolver.withStore(func(db policyStore) error {
		s, ok, err := lookupPolicySession(db, principal.SubjectID)
		if err != nil {
			return err
		}
		if !ok || s.Archived || agent.Canonical(s.Agent) != string(centralauth.Claude) {
			return access.ErrDenied
		}
		session = s
		return nil
	})
	return session, err
}

// dispatchCentralCredential returns the central Claude token. It runs inside
// the lifecycle admission gate and re-authorizes after reading, so an archive
// or revocation that crosses the read never receives the value. Failures carry
// only a fixed code: no path, value or store detail reaches the session.
func (r *sessionRuntime) dispatchCentralCredential(ctx context.Context, request sessionrpc.DispatchRequest) (sessionrpc.DispatchResult, error) {
	r.credentialMu.Lock()
	defer r.credentialMu.Unlock()
	release, _ := lockAdmission(ctx, &r.dispatchMu, &r.d.effectMu, admitShared)
	defer release()
	ctx = withAdmission(ctx, admitShared)
	req := request.Call.AccessRequest()
	s, err := r.centralCredentialSession(ctx, request.Principal, req)
	if err != nil {
		return rpcDenied("credential_access_denied"), nil
	}
	// Settings or inherited variables that would outrank or redirect the token
	// mean the user intends another method: never deliver into that launch.
	if sp, ok := agent.HarnessFor(s.Agent).Config(s); ok && len(claudecfg.CentralConflicts(sp, s.Dir, os.Environ())) > 0 {
		return rpcDenied("central_credential_conflict"), nil
	}
	binding, profile, err := accounts.Delivery(s.ID, "claude")
	if err != nil {
		return rpcDenied("account_binding_blocked"), nil
	}
	// An old launcher ignores response kind and always sets the OAuth input.
	// Refuse API material unless the caller explicitly supports typed delivery.
	if profile.Mode == "api" && req.Fields[centralauth.FormatField] != centralauth.TypedFormat {
		return rpcDenied("credential_type_unsupported"), nil
	}
	cred, err := centralauth.ClaudeStore().Load()
	if err != nil {
		if errors.Is(err, centralauth.ErrNotConfigured) {
			return rpcFailed("central_login_required"), nil
		}
		return rpcFailed("central_credential_unavailable"), nil
	}
	if (cred.Kind == centralauth.KindClaudeAPIKey) != (profile.Mode == "api") {
		return rpcDenied("account_binding_blocked"), nil
	}
	if err := r.authorizeCentralCredential(ctx, request.Principal, req); err != nil {
		return rpcDenied("credential_access_denied"), nil
	}
	if err := accounts.Accept(s.ID, binding.Revision, profile.DeliveryEpoch, "", cred.Generation); err != nil {
		return rpcDenied("account_binding_blocked"), nil
	}
	body, err := json.Marshal(centralauth.Delivery{Token: cred.Token.Reveal(), Kind: cred.Kind, Generation: cred.Generation})
	if err != nil || len(body) > sessionrpc.MaxResponseBody {
		return rpcFailed("central_credential_unavailable"), nil
	}
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body}, nil
}

// claudeKeychainAllowed reports whether the macOS Keychain broker may serve
// Claude's host store. Central and environment modes expose no host store, so
// legacy Keychain reads, writes and deletes are all refused.
func claudeKeychainAllowed() bool { return claudecfg.HostStoreShared() }
