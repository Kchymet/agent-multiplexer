package enrich

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// DefaultMaxBackoff caps a source's poll delay after failures, and is the
// retry interval while it has a setup condition.
const DefaultMaxBackoff = 30 * time.Minute

// RegistryConfig wires a Registry to its environment. Scope, Load and Save
// are required; the rest default.
type RegistryConfig struct {
	// Scope lists what the sources observe, read fresh before each poll.
	Scope func() (Scope, error)
	// Load returns the persisted documents by session id; Save stores one
	// ("" deletes it).
	Load func() (map[string]string, error)
	Save func(sessionID, doc string) error
	// Emit receives every item transition (from the polling goroutine).
	Emit func(Change)
	// Legacy decodes a pre-Registry document (optional).
	Legacy     LegacyDecoder
	Logf       func(format string, args ...any)
	Now        func() time.Time
	MaxBackoff time.Duration
}

// Registry runs the enrichment sources and holds what they observed: one poll
// per source per interval for the whole scope, generic backoff (exponential,
// capped, never before a rate-limit reset), setup conditions (logged once,
// retried slowly), staleness (no success within twice the interval, or a
// setup condition), persistence of the last observation per session per kind,
// and retention per kind. Reads are safe from any goroutine.
type Registry struct {
	cfg RegistryConfig

	mu      sync.RWMutex
	sources []*sourceState
	byKind  map[string]*sourceState
	entries map[string]map[string][]Entry // session id -> kind -> items
	loaded  bool
	started time.Time
}

type sourceState struct {
	src      Source
	pollMu   sync.Mutex // one poll at a time per source
	failures int
	last     *Failure // the last poll's failure (nil: it succeeded)
	lastOK   time.Time
	setupLog bool // the setup condition has been logged
}

// NewRegistry builds a Registry with no sources.
func NewRegistry(cfg RegistryConfig) *Registry {
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Emit == nil {
		cfg.Emit = func(Change) {}
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = DefaultMaxBackoff
	}
	return &Registry{cfg: cfg, byKind: map[string]*sourceState{}, entries: map[string]map[string][]Entry{}, started: cfg.Now()}
}

// Register adds a source. Call it before Run.
func (r *Registry) Register(s Source) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := &sourceState{src: s}
	r.sources = append(r.sources, st)
	r.byKind[s.Kind()] = st
}

// Kinds lists the registered kinds in registration order.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, st := range r.sources {
		out = append(out, st.src.Kind())
	}
	return out
}

// Run polls every source on its own schedule until ctx ends.
func (r *Registry) Run(ctx context.Context) {
	r.mu.RLock()
	sources := append([]*sourceState(nil), r.sources...)
	r.mu.RUnlock()
	var wg sync.WaitGroup
	for _, st := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timer := time.NewTimer(0)
			defer timer.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
					r.PollKind(ctx, st.src.Kind())
					timer.Reset(r.Delay(st.src.Kind()))
				}
			}
		}()
	}
	wg.Wait()
}

// Delay is a source's wait before its next poll: its interval; doubled per
// consecutive failure up to MaxBackoff; MaxBackoff during a setup condition;
// and never before a rate limit resets.
func (r *Registry) Delay(kind string) time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	st := r.byKind[kind]
	if st == nil {
		return r.cfg.MaxBackoff
	}
	return r.delayLocked(st)
}

func (r *Registry) delayLocked(st *sourceState) time.Duration {
	if st.last != nil && st.last.Class == FailSetup {
		return r.cfg.MaxBackoff
	}
	d := st.src.Interval()
	for i := 0; i < st.failures && d < r.cfg.MaxBackoff; i++ {
		d *= 2
	}
	d = min(d, r.cfg.MaxBackoff)
	if st.last != nil && st.last.Class == FailRateLimit && !st.last.ResetAt.IsZero() {
		if wait := st.last.ResetAt.Sub(r.cfg.Now()); wait > d {
			d = wait
		}
	}
	return d
}

// load reads the persisted documents once.
func (r *Registry) loadLocked() {
	if r.loaded {
		return
	}
	docs, err := r.cfg.Load()
	if err != nil {
		r.cfg.Logf("enrich: load state: %v", err)
		return
	}
	r.loaded = true
	for id, doc := range docs {
		if kinds := r.decode([]byte(doc)); len(kinds) > 0 {
			r.entries[id] = kinds
		}
	}
}

// persisted is the per-session document.
type persisted struct {
	V     int                `json:"v"`
	Kinds map[string][]Entry `json:"kinds"`
}

func (r *Registry) decode(doc []byte) map[string][]Entry {
	if len(doc) > 0 && doc[0] == '[' {
		if r.cfg.Legacy == nil {
			return nil
		}
		kind, items, ok := r.cfg.Legacy(doc)
		if !ok || len(items) == 0 {
			return nil
		}
		return map[string][]Entry{kind: items}
	}
	var p persisted
	if json.Unmarshal(doc, &p) != nil {
		return nil
	}
	return p.Kinds
}

func (r *Registry) ensureLoaded() {
	r.mu.RLock()
	loaded := r.loaded
	r.mu.RUnlock()
	if !loaded {
		r.mu.Lock()
		r.loadLocked()
		r.mu.Unlock()
	}
}

// PollKind runs one poll of a kind's source now (Run calls it; tests too).
func (r *Registry) PollKind(ctx context.Context, kind string) {
	r.ensureLoaded()
	r.mu.RLock()
	st := r.byKind[kind]
	loaded := r.loaded
	r.mu.RUnlock()
	if st == nil || !loaded {
		return
	}
	st.pollMu.Lock()
	defer st.pollMu.Unlock()

	scope, err := r.cfg.Scope()
	if err != nil {
		r.cfg.Logf("enrich: %s: list scope: %v", kind, err)
		return
	}
	r.mu.RLock()
	prev := map[string][]Entry{}
	for id, kinds := range r.entries {
		if items := kinds[kind]; len(items) > 0 {
			prev[id] = append([]Entry(nil), items...)
		}
	}
	r.mu.RUnlock()

	obs, perr := st.src.Poll(ctx, scope, prev)
	if ctx.Err() != nil {
		return
	}
	now := r.cfg.Now()
	ret := st.src.Retention()

	saves := map[string]string{}
	r.mu.Lock()
	for id, items := range obs.Sessions {
		items = retain(items, ret, now)
		old := r.entries[id][kind]
		if r.entries[id] == nil {
			r.entries[id] = map[string][]Entry{}
		}
		if len(items) == 0 {
			delete(r.entries[id], kind)
		} else {
			r.entries[id][kind] = items
		}
		if len(r.entries[id]) == 0 {
			delete(r.entries, id)
		}
		if !sameEntries(old, items) {
			saves[id] = r.docLocked(id)
		}
	}
	r.noteLocked(st, perr, now)
	r.mu.Unlock()

	ids := make([]string, 0, len(saves))
	for id := range saves {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := r.cfg.Save(id, saves[id]); err != nil {
			r.cfg.Logf("enrich: save %s: %v", id, err)
		}
	}
	for _, c := range obs.Changes {
		c.Kind = kind
		r.cfg.Emit(c)
	}
}

// noteLocked records a poll's outcome: success, or a classified failure.
func (r *Registry) noteLocked(st *sourceState, err error, now time.Time) {
	kind := st.src.Kind()
	if err == nil {
		if st.last != nil && st.last.Class == FailSetup {
			r.cfg.Logf("enrich: %s: available again, polling resumed", kind)
		}
		st.last, st.failures, st.lastOK, st.setupLog = nil, 0, now, false
		return
	}
	f := st.src.Classify(err)
	st.last = &f
	switch f.Class {
	case FailSetup:
		if !st.setupLog {
			r.cfg.Logf("enrich: %s unavailable (%s), polling paused (retrying every %v): %v", kind, f.Reason, r.cfg.MaxBackoff, err)
			st.setupLog = true
		}
	case FailRateLimit:
		st.failures++
		r.cfg.Logf("enrich: %s: rate limited, backing off to %v: %v", kind, r.delayLocked(st), err)
	default:
		st.failures++
		if st.failures == 1 || st.failures%5 == 0 {
			r.cfg.Logf("enrich: %s: poll failed (%d in a row, next in %v): %v", kind, st.failures, r.delayLocked(st), err)
		}
	}
}

func (r *Registry) docLocked(id string) string {
	kinds := r.entries[id]
	if len(kinds) == 0 {
		return ""
	}
	b, _ := json.Marshal(persisted{V: 2, Kinds: kinds})
	return string(b)
}

// sameEntries compares two item lists ignoring when they were observed.
func sameEntries(a, b []Entry) bool {
	if len(a) != len(b) {
		return false
	}
	strip := func(items []Entry) []byte {
		cp := make([]Entry, len(items))
		for i, it := range items {
			it.ObservedAt = time.Time{}
			cp[i] = it
		}
		out, _ := json.Marshal(cp)
		return out
	}
	return string(strip(a)) == string(strip(b))
}

// retain drops finished items past the retention window and keeps at most
// Max of the rest (the most recently ended), preserving order.
func retain(items []Entry, ret Retention, now time.Time) []Entry {
	var done []int
	keep := make([]bool, len(items))
	for i, it := range items {
		switch {
		case it.Open:
			keep[i] = true
		case it.EndedAt.IsZero() || ret.Window <= 0 || now.Sub(it.EndedAt) <= ret.Window:
			keep[i] = true
			done = append(done, i)
		}
	}
	if ret.Max > 0 && len(done) > ret.Max {
		sort.SliceStable(done, func(a, b int) bool { return items[done[a]].EndedAt.After(items[done[b]].EndedAt) })
		for _, i := range done[ret.Max:] {
			keep[i] = false
		}
	}
	out := make([]Entry, 0, len(items))
	for i, it := range items {
		if keep[i] {
			out = append(out, it)
		}
	}
	return out
}

// Entries returns a session's retained entries of one kind, whether or not its
// source is registered (persisted observations stay readable).
func (r *Registry) Entries(sessionID, kind string) []Entry {
	r.ensureLoaded()
	r.mu.RLock()
	defer r.mu.RUnlock()
	items := r.entries[sessionID][kind]
	if len(items) == 0 {
		return nil
	}
	ret := Retention{Window: Retention3Days, Max: MaxTerminal}
	if st := r.byKind[kind]; st != nil {
		ret = st.src.Retention()
	}
	return retain(append([]Entry(nil), items...), ret, r.cfg.Now())
}

// Retention3Days is the default retention window (PRs).
const Retention3Days = 3 * 24 * time.Hour

// Enrichments renders a session's items of every registered kind: open
// first, then newest. Nil when there is none.
func (r *Registry) Enrichments(sessionID string) *hp.Enrichments {
	r.ensureLoaded()
	now := r.cfg.Now()
	r.mu.RLock()
	type entry struct {
		e    hp.Enrichment
		open bool
		at   time.Time
		ord  int
	}
	var list []entry
	for ord, st := range r.sources {
		kind := st.src.Kind()
		items := retain(append([]Entry(nil), r.entries[sessionID][kind]...), st.src.Retention(), now)
		status := r.statusLocked(st, now)
		for _, it := range items {
			e := st.src.Render(it)
			e.Kind = kind
			if e.Status.State == "" {
				e.Status.State = hp.StatusUnknown
			}
			if e.Status.Reasons == nil {
				e.Status.Reasons = []string{}
			}
			e.Tone = hp.ToneFor(e.Status.State)
			e.ObservedAt = rfc3339(it.ObservedAt)
			e.Stale = it.Open && (it.Stale || status.Stale)
			list = append(list, entry{e: e, open: it.Open, at: it.SortAt, ord: ord})
		}
	}
	r.mu.RUnlock()
	if len(list) == 0 {
		return nil
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.open != b.open {
			return a.open
		}
		if !a.at.Equal(b.at) {
			return a.at.After(b.at)
		}
		return a.ord < b.ord
	})
	out := make(hp.Enrichments, len(list))
	for i, en := range list {
		out[i] = en.e
	}
	return &out
}

// SourceStatus is one source's freshness, for the rollup.
type SourceStatus struct {
	Kind     string
	Interval time.Duration
	LastOK   time.Time
	// LastReason is the last poll's failure reason ("" when it succeeded).
	LastReason  string
	Stale       bool
	StaleReason string
}

// Wire is the published form (hp.Rollup.Sources).
func (s SourceStatus) Wire() hp.EnrichmentSource {
	return hp.EnrichmentSource{Kind: s.Kind, Stale: s.Stale, StaleReason: s.StaleReason,
		ObservedAt: rfc3339(s.LastOK), IntervalS: int(s.Interval / time.Second)}
}

// Sources reports every registered source's freshness at now.
func (r *Registry) Sources() []SourceStatus {
	now := r.cfg.Now()
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]SourceStatus, 0, len(r.sources))
	for _, st := range r.sources {
		out = append(out, r.statusLocked(st, now))
	}
	return out
}

// statusLocked: stale with a setup condition, or with no success within
// twice the interval (since the registry started, before the first one).
func (r *Registry) statusLocked(st *sourceState, now time.Time) SourceStatus {
	s := SourceStatus{Kind: st.src.Kind(), Interval: st.src.Interval(), LastOK: st.lastOK}
	if st.last != nil {
		s.LastReason = st.last.Reason
	}
	base := st.lastOK
	if base.IsZero() {
		base = r.started
	}
	switch {
	case st.last != nil && st.last.Class == FailSetup:
		s.Stale, s.StaleReason = true, st.last.Reason
	case s.Interval > 0 && now.Sub(base) > 2*s.Interval:
		s.Stale, s.StaleReason = true, s.LastReason
		if s.StaleReason == "" {
			s.StaleReason = hp.StaleError
		}
	}
	return s
}
