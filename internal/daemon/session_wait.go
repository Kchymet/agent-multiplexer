package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/sessionrpc"
)

// This file serves core.QueryWait: a long-poll that parks on the daemon's
// snapshot broadcast (the same push the rail's clients get) and answers when
// the target session's state, archived flag or PRs change. It runs on the
// session mailbox's parallel lane (sessionCallParallel), so a wait never holds
// the caller's other calls — a coordinator blocked on a member can still steer
// it — and the mailbox's Close cancels it.

// maxWaitTokenBytes bounds WaitSinceField.
const maxWaitTokenBytes = 64

// validateWaitQuery checks a QueryWait request's shape.
func validateWaitQuery(req access.Request) error {
	if strings.TrimSpace(req.ID) == "" {
		return fmt.Errorf("query %q requires an id", req.Verb)
	}
	if err := validateFields(req.Fields, core.WaitSinceField, core.WaitTimeoutField); err != nil {
		return err
	}
	if len(req.Fields[core.WaitSinceField]) > maxWaitTokenBytes {
		return fmt.Errorf("wait token too long")
	}
	_, err := waitTimeout(req.Fields)
	return err
}

func waitTimeout(fields map[string]string) (time.Duration, error) {
	v, ok := fields[core.WaitTimeoutField]
	if !ok {
		return core.MaxWaitSeconds * time.Second, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > core.MaxWaitSeconds || strconv.Itoa(n) != v {
		return 0, fmt.Errorf("wait timeout must be whole seconds 0..%d", core.MaxWaitSeconds)
	}
	return time.Duration(n) * time.Second, nil
}

// sessionCallParallel puts a wait on the mailbox's parallel lane.
func sessionCallParallel(call sessionrpc.Call) bool {
	req := call.AccessRequest()
	return req.Route == access.RouteQuery && (req.Verb == core.QueryWait || req.Verb == core.QueryQuestionHold)
}

// dispatchQuestionHold serves QueryQuestionHold on the parallel lane: the
// caller waits on its own held call, outside every admission lock.
func (r *sessionRuntime) dispatchQuestionHold(ctx context.Context, principal access.Principal, req access.Request, action core.Action) (sessionrpc.DispatchResult, error) {
	if err := r.withFinalAdmission(ctx, principal, req); err != nil || action.ID != principal.SubjectID {
		return rpcDenied("access_denied"), nil
	}
	body, err := json.Marshal(r.d.holdQuestion(ctx, action.ID, action.Fields))
	if err != nil {
		return rpcFailed("query_failed"), nil
	}
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body}, nil
}

// dispatchWait authorizes, waits without holding any admission lock, then
// re-authorizes at the disclosure boundary: the caller's grant may have changed
// (a member moved out, the caller archived) during the wait.
func (r *sessionRuntime) dispatchWait(ctx context.Context, principal access.Principal, req access.Request, action core.Action) (sessionrpc.DispatchResult, error) {
	if err := r.withFinalAdmission(ctx, principal, req); err != nil {
		return rpcDenied("access_denied"), nil
	}
	timeout, err := waitTimeout(action.Fields)
	if err != nil {
		return rpcInvalid("wait_invalid"), nil
	}
	res, err := r.d.waitSession(ctx, action.ID, action.Fields[core.WaitSinceField], timeout)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return rpcFailed("wait_cancelled"), nil
		}
		return rpcFailed("query_failed"), nil
	}
	if err := r.withFinalAdmission(ctx, principal, req); err != nil {
		return rpcDenied("access_denied"), nil
	}
	body, err := json.Marshal(res)
	if err != nil {
		return rpcFailed("query_failed"), nil
	}
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body}, nil
}

// waitSession blocks until id's view differs from since (the current view when
// since is empty) or timeout passes. It is woken by every rail broadcast — each
// rail poll, including the immediate one the PR watcher triggers on a PR
// change — and re-reads the latest rail rather than
// trusting a frame a slow subscriber might have missed.
func (d *Daemon) waitSession(ctx context.Context, id, since string, timeout time.Duration) (core.WaitResult, error) {
	cur := d.waitView(id)
	if since == "" {
		if timeout == 0 {
			return cur, nil
		}
		since = cur.Token
	}
	if cur.Token != since {
		cur.Changed = true
		return cur, nil
	}
	if timeout == 0 {
		return cur, nil
	}
	ch := make(chan core.Snapshot, 1)
	d.subsMu.Lock()
	if d.subs == nil {
		d.subs = map[chan core.Snapshot]struct{}{}
	}
	d.subs[ch] = struct{}{}
	d.subsMu.Unlock()
	defer func() {
		d.subsMu.Lock()
		delete(d.subs, ch)
		d.subsMu.Unlock()
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return cur, ctx.Err()
		case <-timer.C:
			return cur, nil
		case <-ch:
			cur = d.waitView(id)
			if cur.Token != since {
				cur.Changed = true
				return cur, nil
			}
		}
	}
}

// waitView is id's current view on the rail.
func (d *Daemon) waitView(id string) core.WaitResult {
	res := core.WaitResult{ID: id}
	d.mu.RLock()
	for _, s := range d.sessions {
		if s.ID != id || s.IsRoot && s.Role == "" {
			continue
		}
		res.Present = true
		res.State, res.Status, res.Archived = s.State, s.Status, s.Archived
		if s.PRs != nil {
			res.PRs = append(res.PRs, (*s.PRs)...)
		}
		break
	}
	d.mu.RUnlock()
	res.Token = waitToken(res)
	return res
}

// waitToken fingerprints what a waiter is waiting on: presence, state,
// archived flag and PRs. The status line is deliberately left out —
// it carries cosmetic suffixes that would wake a waiter for nothing.
func waitToken(res core.WaitResult) string {
	b, _ := json.Marshal(struct {
		Present  bool
		State    string
		Archived bool
		PRs      any
	}{res.Present, res.State, res.Archived, res.PRs})
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:18])
}
