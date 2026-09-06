package codexapp

import (
	"encoding/json"
	"errors"
	"sync"
)

// approvals.go tracks the App Server's outstanding approval requests so the
// supervisor can (a) answer on the *exact* JSON-RPC id the server is waiting on,
// (b) reject stale (unknown) and duplicate (already-answered) replies, and (c)
// clear a request when the SERVER reports it resolved — by any client — via the
// `serverRequest/resolved` notification, which arrives while the turn is still
// active (§3.1 / AGE-198).
//
// Correlation model: an App Server approval is a server→client JSON-RPC *request*
// carrying its own top-level id plus params.{itemId, threadId, turnId}. A client's
// reply is a JSON-RPC response echoing that id; the server then broadcasts a
// `serverRequest/resolved{requestId, threadId}` notification to every client.
//
// CRUCIAL: our local answer is only an *attempt*. A concurrent peer may have
// answered the other way before ours arrived, and the resolution notification names
// neither the winning client nor its decision. So the tracker never treats our
// attempt as the outcome — resolution and turn-end abandonment clear a request
// NEUTRALLY (the supervisor emits DecisionCleared) unless a real winning decision
// is supplied by the server itself.

// approvalState is where a live approval is in its lifecycle.
type approvalState int

const (
	apPending  approvalState = iota // registered; no client has answered yet
	apAnswered                      // this supervisor sent a reply; awaiting server confirmation
)

// pendingApproval is one live server approval request (pending or answered).
type pendingApproval struct {
	rawID    json.RawMessage // the server request's JSON-RPC id, echoed verbatim in the reply
	key      string          // string form of rawID; the correlation key + contract request_id
	method   string          // e.g. "item/commandExecution/requestApproval"
	threadID string
	turnID   string
	itemID   string
	state    approvalState
}

// approvalTracker holds the live approvals plus the ids already resolved (for
// duplicate/stale classification). Safe for concurrent use: the read loop
// registers and resolves-by-server; Resolve answers locally.
type approvalTracker struct {
	mu       sync.Mutex
	live     map[string]*pendingApproval // pending OR answered-awaiting-confirmation
	resolved map[string]bool             // fully resolved (server-confirmed / external / cleared)
}

func newApprovalTracker() *approvalTracker {
	return &approvalTracker{live: map[string]*pendingApproval{}, resolved: map[string]bool{}}
}

// register records a new outstanding approval. A repeat id (server re-sent) keeps
// the first registration; an id already resolved is not resurrected.
func (a *approvalTracker) register(p *pendingApproval) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.live[p.key]; ok || a.resolved[p.key] {
		return
	}
	p.state = apPending
	a.live[p.key] = p
}

// errStaleApproval / errDuplicateApproval classify a rejected Resolve so callers
// (and the daemon's error surface) can distinguish an id never seen from one
// already answered/resolved.
var (
	errStaleApproval     = errors.New("codexapp: unknown approval request (stale)")
	errDuplicateApproval = errors.New("codexapp: approval request already resolved (duplicate)")
)

// answerLocally marks that this supervisor has sent a reply for requestID and
// returns the request so the caller can write the JSON-RPC response. It does NOT
// mark the request resolved or emit anything — resolution waits for the server's
// authoritative notification, and our reply is only an attempt (a peer may win the
// other way). A second local answer, or an answer to an already-resolved id, is a
// duplicate; an unknown id is stale.
func (a *approvalTracker) answerLocally(requestID string) (*pendingApproval, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.resolved[requestID] {
		return nil, errDuplicateApproval
	}
	p, ok := a.live[requestID]
	if !ok {
		return nil, errStaleApproval
	}
	if p.state == apAnswered {
		return nil, errDuplicateApproval
	}
	p.state = apAnswered
	return p, nil
}

// serverResolved records the server's authoritative resolution of requestID (by
// any client) and reports whether this is the first time it was resolved (so the
// caller emits permission_resolved exactly once). It intentionally returns NO
// decision — the notification names no winner, so the caller clears neutrally. An
// unknown or already-resolved id returns false (ignored — no duplicate emit).
func (a *approvalTracker) serverResolved(requestID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.resolved[requestID] {
		return false
	}
	if _, live := a.live[requestID]; !live {
		return false
	}
	delete(a.live, requestID)
	a.resolved[requestID] = true
	return true
}

// drainOutstanding resolves every still-live approval (the turn ended before the
// server confirmed them) and returns their ids so the caller can clear each
// NEUTRALLY — an unconfirmed local answer is still only an attempt, so turn-end
// abandonment must not surface it as the outcome. Each id is marked resolved so a
// late notification does not double-emit.
func (a *approvalTracker) drainOutstanding() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.live))
	for k := range a.live {
		out = append(out, k)
		a.resolved[k] = true
	}
	a.live = map[string]*pendingApproval{}
	return out
}

// open returns the ids of approvals still awaiting a first answer (apPending), the
// set a new decision may target — an answered-awaiting-confirmation request is not
// re-answerable.
func (a *approvalTracker) open() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]string, 0, len(a.live))
	for k, p := range a.live {
		if p.state == apPending {
			ids = append(ids, k)
		}
	}
	return ids
}

// idKey renders a JSON-RPC id (number or string) to its canonical string key. A
// quoted string id ("req_1") is unquoted; a numeric id (42) is used as-is.
func idKey(rawID json.RawMessage) string {
	var s string
	if err := json.Unmarshal(rawID, &s); err == nil {
		return s
	}
	return string(rawID)
}
