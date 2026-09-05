package codexapp

import (
	"encoding/json"
	"errors"
	"sync"
)

// approvals.go tracks the App Server's outstanding approval requests so Resolve
// can (a) answer on the *exact* JSON-RPC id the server is waiting on, correlating
// the decision to the originating server-request / thread / turn, and (b) reject
// stale (unknown id) and duplicate (already-answered id) replies — the AGE-172 /
// §3.1 correlation property carried through to structured control mode.
//
// Correlation model: an App Server approval is a server→client JSON-RPC *request*
// carrying its own top-level id plus params.{itemId, threadId, turnId}. The reply
// is a JSON-RPC response echoing that id. So the id is the correlation key; we
// keep the raw id (to echo verbatim) alongside the thread/turn ids for validation
// and logging.

// pendingApproval is one outstanding server approval request awaiting a decision.
type pendingApproval struct {
	rawID    json.RawMessage // the server request's JSON-RPC id, echoed verbatim in the reply
	key      string          // string form of rawID; the correlation key + contract request_id
	method   string          // e.g. "item/commandExecution/requestApproval"
	threadID string
	turnID   string
	itemID   string
}

// approvalTracker is the set of outstanding approvals, keyed by request id. Safe
// for concurrent use: the read loop registers, Resolve consumes.
type approvalTracker struct {
	mu       sync.Mutex
	pending  map[string]*pendingApproval
	resolved map[string]bool // ids already answered, so a duplicate is rejected distinctly
}

func newApprovalTracker() *approvalTracker {
	return &approvalTracker{pending: map[string]*pendingApproval{}, resolved: map[string]bool{}}
}

// register records a new outstanding approval. A repeat id (server re-sent) keeps
// the first registration.
func (a *approvalTracker) register(p *pendingApproval) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.pending[p.key]; ok {
		return
	}
	a.pending[p.key] = p
}

// errStaleApproval / errDuplicateApproval classify a rejected Resolve so callers
// (and the daemon's error surface) can distinguish an id never seen from one
// already answered.
var (
	errStaleApproval     = errors.New("codexapp: unknown approval request (stale)")
	errDuplicateApproval = errors.New("codexapp: approval request already resolved (duplicate)")
)

// take removes and returns the outstanding approval for requestID, or an error
// if it is unknown (stale) or already resolved (duplicate). On success the id is
// recorded as resolved so a later duplicate is rejected distinctly.
func (a *approvalTracker) take(requestID string) (*pendingApproval, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.pending[requestID]
	if !ok {
		if a.resolved[requestID] {
			return nil, errDuplicateApproval
		}
		return nil, errStaleApproval
	}
	delete(a.pending, requestID)
	a.resolved[requestID] = true
	return p, nil
}

// open returns the request ids of every approval currently awaiting a decision,
// so the supervisor can answer a checkPermissionRequest-style correlation query
// (which ids the runtime has open right now) and can auto-clear them on close.
func (a *approvalTracker) open() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]string, 0, len(a.pending))
	for k := range a.pending {
		ids = append(ids, k)
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
