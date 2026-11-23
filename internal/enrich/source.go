package enrich

import (
	"context"
	"encoding/json"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// A Source observes one kind of thing related to sessions — pull requests
// first; later issues (Linear/GitHub), CI runs, pipeline-engine builds and
// deploys, preview URLs, Firebase releases — and attributes what it finds to
// session ids. The Registry runs it: one Poll per source per interval for the
// whole scope (never per client), with backoff, setup conditions, staleness,
// persistence, retention and transition events handled generically.
type Source interface {
	// Kind is the enrichment kind it produces (hp.Enrichment.Kind).
	Kind() string
	// Interval is the normal poll interval.
	Interval() time.Duration
	// Retention says how long finished items stay listed.
	Retention() Retention
	// Poll observes the scope. prev is the last observation per session id
	// for this kind (the source's own persisted records in Entry.Data). It
	// returns the complete item set for every session it observed (sessions
	// it leaves out keep their items) and the transitions it saw. On a partial
	// failure it returns what it observed together with the error; on a setup
	// failure (Classify) nothing.
	Poll(ctx context.Context, scope Scope, prev map[string][]Entry) (Observation, error)
	// Classify sorts a Poll error for backoff and staleness.
	Classify(err error) Failure
	// Render turns an item into its published envelope: everything except
	// observed_at and stale, which the Registry fills in.
	Render(it Entry) hp.Enrichment
}

// Retention bounds finished items: each stays Window after it ended, at most
// Max of them per session (the most recently ended). Open items always stay.
type Retention struct {
	Window time.Duration
	Max    int
}

// Scope is what the sources observe: the tracked sessions with what they need
// (branches, repos).
type Scope struct {
	Sessions []ScopeSession
}

// ScopeSession is one tracked session.
type ScopeSession struct {
	ID        string
	Workgroup string   // its workgroup (= coordinator session) id
	Label     string   // display label (name or id)
	Branch    string   // its branch, e.g. amux/<broker>/<wg>-<member>
	Repos     []string // GitHub repos it works in (owner/name)
}

// Entry is one observed thing attributed to a session. The Registry persists it
// and applies retention; Data is the source's own record, opaque here.
type Entry struct {
	Key  string `json:"key"`  // stable within the kind (e.g. owner/repo#79)
	Open bool   `json:"open"` // not finished: retention never drops it
	// EndedAt is when a finished item ended (zero: unknown, kept).
	EndedAt time.Time `json:"ended_at,omitzero"`
	// SortAt orders items newest first (the item's own last change).
	SortAt time.Time `json:"sort_at,omitzero"`
	// ObservedAt is when the source last saw this item.
	ObservedAt time.Time `json:"observed_at,omitzero"`
	// Stale: the part of the scope this item belongs to failed on the last
	// poll (e.g. its repo's listing), so it is last-known. Not persisted.
	Stale bool            `json:"-"`
	Data  json.RawMessage `json:"data"`
}

// Observation is one Poll's result.
type Observation struct {
	// Sessions is the complete item set per observed session id.
	Sessions map[string][]Entry
	// Changes are the item transitions seen, emitted in order.
	Changes []Change
}

// Change is one item transition. Payload is the source's own detail (e.g. a
// prwatch.Change for a PR) for kind-specific consumers.
type Change struct {
	Kind      string
	SessionID string
	Key       string
	Changes   []string
	Payload   any
}

// Failure classes (Failure.Class).
type FailureClass int

const (
	// FailTransient: backed off exponentially, capped.
	FailTransient FailureClass = iota
	// FailRateLimit: backed off exponentially, and never before ResetAt.
	FailRateLimit
	// FailSetup: the source cannot work until the host fixes something (gh
	// missing or logged out): logged once, retried slowly, stale at once.
	FailSetup
)

// Failure is a classified Poll error.
type Failure struct {
	Class FailureClass
	// Reason is the wire staleness reason (hp.Stale*, or a source's own
	// setup reason).
	Reason string
	// ResetAt, for a rate limit, is when it resets (zero: unknown).
	ResetAt time.Time
}

// LegacyDecoder turns a persisted document from before the Registry into a
// kind's items (ok=false when it is not one).
type LegacyDecoder func(doc []byte) (kind string, items []Entry, ok bool)
