package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"amux/internal/access"
	"amux/internal/amuxcfg"
	"amux/internal/centralauth"
	"amux/internal/core"
	"amux/internal/credentialbroker"
	"amux/internal/sessionreport"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

func (r *sessionRuntime) authorize(ctx context.Context, principal access.Principal, call sessionrpc.Call) error {
	if r == nil || r.d == nil || r.d.authority == nil {
		return access.ErrDenied
	}
	if err := r.d.authority.Valid(ctx, principal); err != nil {
		return err
	}
	if call.Kind == sessionrpc.CallReceipt {
		if call.Receipt == nil || !r.completions.authorizeReceipt(principal, *call.Receipt) {
			return access.ErrDenied
		}
		return nil
	}
	if call.Kind != sessionrpc.CallOperation {
		return access.ErrDenied
	}
	req := call.AccessRequest()
	if credentialbroker.IsVerb(req.Verb) {
		return r.authorizeCredential(ctx, principal, req)
	}
	if req.Verb == centralauth.QueryVerb {
		return r.authorizeCentralCredential(ctx, principal, req)
	}
	if isSessionReport(req) {
		return r.authorizeSessionReport(ctx, principal, req)
	}
	if _, err := canonicalSessionOperation(req); err != nil {
		return err
	}
	return r.policy.Authorize(ctx, principal, req)
}

func (r *sessionRuntime) dispatch(ctx context.Context, request sessionrpc.DispatchRequest) (sessionrpc.DispatchResult, error) {
	if request.Call.Kind != sessionrpc.CallOperation {
		return rpcInvalid("invalid_call"), nil
	}
	req := request.Call.AccessRequest()
	if r.reportsOnly.Load() && !isSessionReport(req) {
		return rpcFailed("daemon_stopping"), nil
	}
	if credentialbroker.IsVerb(req.Verb) {
		return r.dispatchCredential(ctx, request)
	}
	if req.Verb == centralauth.QueryVerb {
		return r.dispatchCentralCredential(ctx, request)
	}
	if req.Route == access.RouteAction && req.Verb == sessionreport.Capture {
		return r.dispatchSessionCapture(ctx, request.Principal, req), nil
	}
	if req.Route == access.RouteQuery && isSessionReport(req) {
		// The report context is read-only correlation: it discloses only the
		// caller's own runtime generation, and every report that carries it is
		// re-checked under the effect gate. Serving it outside the daemon-wide
		// gates halves the gated work of every hook and status-line report.
		if err := r.d.authority.Valid(ctx, request.Principal); err != nil {
			return rpcDenied("credential_invalid"), nil
		}
		return r.dispatchSessionReport(ctx, request.Principal, req), nil
	}
	var action core.Action
	if !isSessionReport(req) {
		var err error
		action, err = canonicalSessionOperation(req)
		if err != nil {
			if req.Route == access.RouteQuery && req.Verb == core.QueryRuntimeEvents {
				return runtimeEventError(err), nil
			}
			return rpcInvalid("invalid_operation"), nil
		}
		if req.Route == access.RouteQuery && req.Verb == core.QueryAgentSessions {
			return r.dispatchAgentSessions(ctx, request.Principal, req), nil
		}
		if req.Route == access.RouteQuery && req.Verb == core.QueryRuntimeEvents {
			return r.dispatchRuntimeEvents(ctx, request.Principal, req, action)
		}
	}
	if isSessionReport(req) {
		// A report reads and writes only its own session's state, so it waits
		// only for that session (and for holders of the unsplit gate).
		release := r.d.admitSession(request.Principal.SubjectID)
		defer release()
		if err := r.d.authority.Valid(ctx, request.Principal); err != nil {
			return rpcDenied("credential_invalid"), nil
		}
		return r.dispatchSessionReport(ctx, request.Principal, req), nil
	}
	if req.Route == access.RouteQuery {
		// Restricted queries only read. The shared gate keeps exclusive policy
		// mutations out, and the caller's session lock keeps out an archive or
		// delete of the caller, so none lands between this authorization and
		// the disclosure.
		release := r.d.admitSessions(request.Principal.SubjectID)
		defer release()
		if denied, ok := r.admissionCheck(ctx, request.Principal, req); !ok {
			return denied, nil
		}
		body, err := r.restrictedQuery(ctx, request.Principal, action)
		if err != nil {
			if errors.Is(err, access.ErrDenied) {
				return rpcDenied("access_denied"), nil
			}
			return rpcFailed("query_failed"), nil
		}
		return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body}, nil
	}
	guarded := withGuardSubject(withAccessGuard(ctx, func(checkCtx context.Context) error {
		return r.finalAuthorize(checkCtx, request.Principal, req)
	}), request.Principal.SubjectID)
	if selfCompletionShape(request.Principal, action) {
		return r.selfComplete(ctx, request, req, action)
	}
	if splitAction(action) {
		// An action handle admits per resource (a coordinator prompting,
		// creating or archiving a member, say) takes only those locks inside
		// handle, plus the caller's, and reruns the guard there, immediately
		// before the effect. Check once first so a denied caller gets the same
		// codes as on the exclusive path.
		release := r.d.admitShared()
		denied, ok := r.admissionCheck(ctx, request.Principal, req)
		release()
		if !ok {
			return denied, nil
		}
		return r.actionResult(r.d.handle(guarded, action))
	}
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	r.d.lockEffectExclusive()
	defer r.d.effectMu.Unlock()
	ctx = withEffectAdmission(ctx)
	guarded = withEffectAdmission(guarded)

	// Authorize again under the dispatch lock, immediately before the exact
	// canonical action executes. Membership, grants and archive state are live.
	if denied, ok := r.admissionCheck(ctx, request.Principal, req); !ok {
		return denied, nil
	}

	return r.actionResult(r.d.handle(guarded, action))
}

// selfComplete is an agent marking itself done (`amux agent done`). It archives
// only the caller (with its members, for a coordinator), so it runs under
// those sessions' locks and creationMu, as a host archive does, not the
// exclusive gate. It begins a completion without dispatchMu: a restore that
// drained before this registers cancels the entry once it holds the gate, and
// finish rechecks ownership and the archive flag under the session lock.
func (r *sessionRuntime) selfComplete(ctx context.Context, request sessionrpc.DispatchRequest, req access.Request, action core.Action) (sessionrpc.DispatchResult, error) {
	release := r.d.admitArchive(action.ID)
	defer release()
	if denied, ok := r.admissionCheck(ctx, request.Principal, req); !ok {
		return denied, nil
	}
	if !r.isSelfCompletion(ctx, request.Principal, action) {
		// Never fall through to the ordinary wsops dispatch for a claimed
		// self-completion: that path stops the runtime before its response.
		return rpcDenied("completion_not_current"), nil
	}
	// Completion is deliberately not d.handle/wsops.Dispatch: those stop the
	// runtime before the durable response and its receipt can be observed.
	if _, err := r.applyResult(ctx, action); err != nil {
		archived, known := r.completionArchiveState(ctx, action.ID)
		if archived || !known {
			// The mutation may already be committed even though its caller saw an
			// error. Keep the narrow archived receipt window and, independently
			// of transport hooks, guarantee bounded runtime stop/revocation.
			r.d.triggerPoll()
			r.completions.begin(request.Principal, request.RequestID, action.ID)
			return rpcIndeterminate("completion_commit_uncertain"), nil
		}
		return rpcFailed("completion_failed"), nil
	}
	r.d.triggerPoll()
	hooks := r.completions.begin(request.Principal, request.RequestID, action.ID)
	body, err := r.encodeResult(core.Result{Type: "result", OK: true})
	if err != nil {
		return rpcFailed("encode_failed"), nil
	}
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body, Receipt: hooks}, nil
}

// admissionCheck is the credential and policy check every dispatch repeats
// under its admission, with the codes a denied caller sees.
func (r *sessionRuntime) admissionCheck(ctx context.Context, principal access.Principal, req access.Request) (sessionrpc.DispatchResult, bool) {
	if err := r.d.authority.Valid(ctx, principal); err != nil {
		return rpcDenied("credential_invalid"), false
	}
	if err := r.policy.Authorize(ctx, principal, req); err != nil {
		return rpcDenied("access_denied"), false
	}
	return sessionrpc.DispatchResult{}, true
}

func (r *sessionRuntime) actionResult(result core.Result) (sessionrpc.DispatchResult, error) {
	body, err := r.encodeResult(result)
	if err != nil {
		return rpcFailed("encode_failed"), nil
	}
	if !result.OK {
		// Host-side action failures can contain filesystem details. Restricted
		// callers receive a stable code only and never a raw daemon error string.
		return rpcFailed("execution_failed"), nil
	}
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body}, nil
}

// dispatchRuntimeEvents performs bounded source I/O without monopolizing the
// lifecycle effect gate. Authorization is checked under the gate immediately
// before the read and again immediately before the normalized result is
// released, so a move, archive, deletion, or credential revocation crossing the
// read prevents disclosure. The pager accepts no caller-selected path.
func (r *sessionRuntime) dispatchRuntimeEvents(ctx context.Context, principal access.Principal, req access.Request, action core.Action) (sessionrpc.DispatchResult, error) {
	if r.events == nil || r.eventsErr != nil {
		return rpcFailed("runtime_events_unavailable"), nil
	}
	if err := r.withFinalAdmission(ctx, principal, req); err != nil {
		return rpcDenied("access_denied"), nil
	}
	body, err := r.events.pageForRelease(ctx, principal, action.ID, action.Fields, func(checkCtx context.Context) error {
		return r.withFinalAdmission(checkCtx, principal, req)
	})
	if err != nil {
		return runtimeEventError(err), nil
	}
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body}, nil
}

// withFinalAdmission only checks: it gates a disclosure. The shared gate keeps
// exclusive policy mutations out, and the caller's and subject's session locks
// keep out an archive or delete of either.
func (r *sessionRuntime) withFinalAdmission(ctx context.Context, principal access.Principal, req access.Request) error {
	release := r.d.admitSessions(principal.SubjectID, req.ID)
	defer release()
	return r.finalAuthorize(ctx, principal, req)
}

func (r *sessionRuntime) finalAuthorize(ctx context.Context, principal access.Principal, req access.Request) error {
	if err := r.d.authority.Valid(ctx, principal); err != nil {
		return err
	}
	// Recheck membership, grants, and archive state at the exact effect or
	// disclosure boundary using the same canonical request that executes.
	return r.policy.Authorize(ctx, principal, req)
}

func runtimeEventError(err error) sessionrpc.DispatchResult {
	switch {
	case errors.Is(err, access.ErrDenied):
		return rpcDenied("access_denied")
	case errors.Is(err, errEventQueryInvalid):
		return rpcInvalid("runtime_events_invalid")
	case errors.Is(err, errEventCursorInvalid):
		return rpcInvalid("runtime_events_cursor_invalid")
	case errors.Is(err, errEventStateTooLarge):
		return rpcFailed("runtime_events_state_too_large")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return rpcFailed("runtime_events_cancelled")
	default:
		return rpcFailed("query_failed")
	}
}

func (r *sessionRuntime) restrictedQuery(ctx context.Context, principal access.Principal, action core.Action) ([]byte, error) {
	var value any
	switch action.Query {
	case core.QuerySessions:
		rows, err := r.resolver.scopedSessionRows(ctx, principal)
		if err != nil {
			return nil, err
		}
		value = rows
	case core.QueryRepos:
		rows, err := r.resolver.scopedRepoRows(ctx, principal)
		if err != nil {
			return nil, err
		}
		value = rows
	case core.QuerySnapshot:
		snapshot, err := r.policy.FilterSnapshot(ctx, principal, r.d.snapshot())
		if err != nil {
			return nil, err
		}
		value = normalizeRestrictedSnapshot(snapshot).Sessions
	case core.QueryVersion:
		rows, err := r.d.readModel(action)
		if err != nil {
			return nil, err
		}
		info, ok := rows.(core.VersionInfo)
		if !ok {
			return nil, fmt.Errorf("unexpected version projection %T", rows)
		}
		info.DatabaseError = ""
		value = info
	case core.QueryCodexControl:
		rows, err := r.d.readModel(action)
		if err != nil {
			return nil, err
		}
		control, ok := rows.(amuxcfg.Control)
		if !ok {
			return nil, fmt.Errorf("unexpected control projection %T", rows)
		}
		control.ConfigPath = ""
		control.Override = ""
		control.Warning = ""
		value = control
	default:
		// In particular, never call readModel's runtime-path or untracked
		// transcript fallback for a restricted principal.
		return nil, access.ErrDenied
	}
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode restricted query: %w", err)
	}
	if len(body) > sessionrpc.MaxResponseBody {
		return nil, fmt.Errorf("restricted query response exceeds transport bound")
	}
	return body, nil
}

func (r *sessionRuntime) isSelfCompletion(ctx context.Context, principal access.Principal, action core.Action) bool {
	resource, ok, err := r.resolver.Lookup(ctx, principal.SubjectID)
	return err == nil && ok && (resource.Role == store.RoleAgent || resource.Role == store.RoleCoordinator) && !resource.Archived
}

// completionArchiveState distinguishes a known pre-commit failure from an
// accepted mutation whose commit outcome cannot be proven. Unknown state stays
// on the fail-closed cleanup path and is never automatically retried.
func (r *sessionRuntime) completionArchiveState(ctx context.Context, id string) (archived, known bool) {
	resource, ok, err := r.resolver.Lookup(ctx, id)
	if err != nil || !ok {
		return false, false
	}
	return resource.Archived, true
}

func selfCompletionShape(principal access.Principal, action core.Action) bool {
	return principal.Kind == access.SubjectSession && principal.SubjectID == action.ID &&
		action.Action == core.ActionSetArchived && action.Fields["archived"] == "true"
}

func rpcDenied(code string) sessionrpc.DispatchResult {
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusDenied, Code: code}
}

func rpcInvalid(code string) sessionrpc.DispatchResult {
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusInvalid, Code: code}
}

func rpcFailed(code string) sessionrpc.DispatchResult {
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusFailed, Code: code}
}

func rpcIndeterminate(code string) sessionrpc.DispatchResult {
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusIndeterminate, Code: code}
}
