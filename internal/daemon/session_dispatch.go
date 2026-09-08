package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"amux/internal/access"
	"amux/internal/amuxcfg"
	"amux/internal/core"
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
	action, err := canonicalSessionOperation(req)
	if err != nil {
		if req.Route == access.RouteQuery && req.Verb == core.QueryRuntimeEvents {
			return runtimeEventError(err), nil
		}
		return rpcInvalid("invalid_operation"), nil
	}
	if req.Route == access.RouteQuery && req.Verb == core.QueryRuntimeEvents {
		return r.dispatchRuntimeEvents(ctx, request.Principal, req, action)
	}

	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	r.d.effectMu.Lock()
	defer r.d.effectMu.Unlock()
	ctx = withEffectAdmission(ctx)

	if err := r.d.authority.Valid(ctx, request.Principal); err != nil {
		return rpcDenied("credential_invalid"), nil
	}
	// Authorize again under the dispatch lock, immediately before the exact
	// canonical action executes. Membership, grants and archive state are live.
	if err := r.policy.Authorize(ctx, request.Principal, req); err != nil {
		return rpcDenied("access_denied"), nil
	}
	if req.Route == access.RouteQuery {
		body, err := r.restrictedQuery(ctx, request.Principal, action)
		if err != nil {
			if errors.Is(err, access.ErrDenied) {
				return rpcDenied("access_denied"), nil
			}
			return rpcFailed("query_failed"), nil
		}
		return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body}, nil
	}

	if selfCompletionShape(request.Principal, action) {
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

	guarded := withAccessGuard(ctx, func() error {
		if err := r.d.authority.Valid(ctx, request.Principal); err != nil {
			return err
		}
		return r.policy.Authorize(ctx, request.Principal, req)
	})
	result := r.d.handle(guarded, action)
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
	body, err := r.events.pageForRelease(ctx, principal, action.ID, action.Fields, func(ctx context.Context) error {
		return r.withFinalAdmission(ctx, principal, req)
	})
	if err != nil {
		return runtimeEventError(err), nil
	}
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body}, nil
}

func (r *sessionRuntime) withFinalAdmission(ctx context.Context, principal access.Principal, req access.Request) error {
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	r.d.effectMu.Lock()
	defer r.d.effectMu.Unlock()
	return r.finalAuthorize(withEffectAdmission(ctx), principal, req)
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
	return err == nil && ok && resource.Role == store.RoleAgent && !resource.Archived
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
