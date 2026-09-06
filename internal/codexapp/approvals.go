package codexapp

import (
	"encoding/json"
	"errors"
	"sync"

	"github.com/kchymet/agent-multiplexer/harnessproto"
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
// `serverRequest/resolved{requestId, threadId}` notification to every client. So a
// request has three fates, and only the SERVER notification is authoritative:
//
//   - answered locally: this supervisor sent a decision; it stays correlatable
//     (awaiting confirmation) — we do NOT emit permission_resolved on the write
//     alone (no speculative resolution). The server's notification confirms it.
//   - answered by another client (native TUI / web peer): we never wrote a reply;
//     the server's notification is the only signal, and clears it.
//   - abandoned: the turn ends with the request still open; it is cleared then.

// approvalState is where a live approval is in its lifecycle.
type approvalState int

const (
	apPending  approvalState = iota // registered; no client has answered yet
	apAnswered                      // this supervisor answered; awaiting server confirmation
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
	decision string // the decision this supervisor sent (apAnswered only)
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

// answerLocally records this supervisor's decision for requestID and returns the
// request so the caller can send the JSON-RPC reply. It does NOT mark the request
// resolved or emit anything — resolution waits for the server's authoritative
// `serverRequest/resolved` notification (no speculative resolution on write). A
// second local answer, or an answer to an already-resolved id, is a duplicate; an
// unknown id is stale.
func (a *approvalTracker) answerLocally(requestID, decision string) (*pendingApproval, error) {
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
	p.decision = decision
	return p, nil
}

// serverResolved records the server's authoritative resolution of requestID (by
// any client) and returns the decision to surface plus whether this is the first
// time it was resolved (so the caller emits permission_resolved exactly once). The
// decision is the one this supervisor sent if it answered locally, otherwise
// DecisionCleared (another client answered; we don't know which way). An unknown or
// already-resolved id returns ok=false (ignored — no duplicate emit).
func (a *approvalTracker) serverResolved(requestID string) (decision string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.resolved[requestID] {
		return "", false // already resolved — duplicate notification
	}
	p, live := a.live[requestID]
	if !live {
		return "", false // unknown request — not ours to resolve
	}
	decision = harnessproto.DecisionCleared
	if p.state == apAnswered {
		decision = p.decision
	}
	delete(a.live, requestID)
	a.resolved[requestID] = true
	return decision, true
}

// threadOf returns the pinned thread id a live request belongs to, so a resolution
// notification can be matched to the right thread. ok=false for an unknown id.
func (a *approvalTracker) threadOf(requestID string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.live[requestID]
	if !ok {
		return "", false
	}
	return p.threadID, true
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

// resolution is one cleared approval: its request id and the decision to surface.
type resolution struct {
	key      string
	decision string
}

// drainOutstanding resolves every still-live approval (turn ended before the
// server confirmed them) and returns what to surface: an answered request carries
// the decision this supervisor sent, a pending one DecisionCleared. Each id is
// marked resolved so a late notification does not double-emit.
func (a *approvalTracker) drainOutstanding() []resolution {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]resolution, 0, len(a.live))
	for k, p := range a.live {
		d := harnessproto.DecisionCleared
		if p.state == apAnswered {
			d = p.decision
		}
		out = append(out, resolution{key: k, decision: d})
		a.resolved[k] = true
	}
	a.live = map[string]*pendingApproval{}
	return out
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
