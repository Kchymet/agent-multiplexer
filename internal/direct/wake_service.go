package direct

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"amux/internal/core"
	"amux/internal/runtimeevents"
	amuxstore "amux/internal/store"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// WakeService runs direct wake-ups inside `amux direct serve`: the detector
// (events -> records), the leased outbox the provider drains, and the data
// side of the direct wake API. It never holds the authority lock while doing
// wake work, and a wake failure never reaches the authority.
type WakeService struct {
	a       *Authority
	store   *wakeStore
	backend Backend
	now     func() time.Time
	logf    func(string, ...any)

	tick      time.Duration
	perTick   int
	pageLimit int
	maxPages  int

	mu        sync.Mutex
	leases    map[string]*wakeLease
	leasedIDs map[string]string // item key -> lease id
	notify    chan struct{}
	rr        int                  // round-robin cursor over sessions
	checked   map[string]time.Time // sid -> last detector read
	lastState map[string]string    // sid -> inventory state
	lagging   bool
	// actBackoff delays re-leasing an activation after a retryable answer.
	actBackoff map[string]wakeRetry
}

type wakeRetry struct {
	attempts int
	nextAt   time.Time
}

type wakeLease struct {
	expires time.Time
	items   []wakeLeaseEntry
}

type wakeLeaseEntry struct {
	ID   string
	Kind string // activate | wake
	// activate
	ClientID, RouteID, Nonce, Digest string
	// wake
	Out wakeOutItem
}

// WakeLeaseItem is what the provider receives for one leased item. Secret is
// set only on activate items.
type WakeLeaseItem struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	RouteID   string `json:"route_id"`
	WakeID    string `json:"wake_id,omitempty"`
	GrantRef  string `json:"grant_ref"`
	Requester string `json:"requester,omitempty"`
	Secret    string `json:"secret,omitempty"`
}

// WakeSettleResult is the provider's report of Harness's answer.
type WakeSettleResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// NewWakeService opens wake.json in root and binds the service.
func NewWakeService(a *Authority, root string, backend Backend, logf func(string, ...any)) *WakeService {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	ws, note := openWakeStore(root, nil, a.now)
	if note != "" {
		logf("direct wake: %s", note)
	}
	s := &WakeService{
		a: a, store: ws, backend: backend, now: a.now, logf: logf,
		tick: 2 * time.Second, perTick: 16, pageLimit: 128, maxPages: 8,
		leases: map[string]*wakeLease{}, leasedIDs: map[string]string{}, notify: make(chan struct{}, 1),
		checked: map[string]time.Time{}, lastState: map[string]string{}, actBackoff: map[string]wakeRetry{},
	}

	// Startup reconciliation: drop data the authority no longer vouches for.
	s.store.reconcile(a.wakeAuthSnapshot())
	_ = s.store.flush(true)
	return s
}

func (s *WakeService) poke() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Degraded reports wake-degraded state (store write uncertainty or detector lag).
func (s *WakeService) Degraded() (bool, string) {
	if r := s.store.degradedReason(); r != "" {
		return true, r
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lagging {
		return true, "wake detector lag exceeds 60s"
	}
	return false, ""
}

// Status is the policy status plus degraded state.
func (s *WakeService) Status() WakeStatus {
	st := s.a.WakeStatus()
	st.Degraded, st.Reason = s.Degraded()
	return st
}

// Run drives the detector until ctx ends.
func (s *WakeService) Run(ctx context.Context) {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		s.Tick(ctx)
		_ = s.store.flush(false)
		select {
		case <-ctx.Done():
			_ = s.store.flush(true)
			return
		case <-t.C:
		}
	}
}

// Tick runs one detector pass (exported for tests).
func (s *WakeService) Tick(ctx context.Context) {
	auth := s.a.wakeAuthSnapshot()
	s.store.reconcile(auth)
	if auth == nil || len(auth.Subs) == 0 || s.store.degradedReason() != "" {
		return
	}
	rows, err := s.backend.Snapshot(ctx)
	if err != nil {
		return
	}
	byID := indexRows(rows)
	// Which subscriptions cover which sessions now.
	cover := map[string][]wakeTarget{}
	for _, sub := range auth.Subs {
		for _, row := range rows {
			if !validSessionID(row.ID) || !inScope(sub.Grant, row, byID) || effectivelyArchived(row, byID) {
				continue
			}
			cover[row.ID] = append(cover[row.ID], wakeTarget{ClientID: sub.ClientID, RouteID: sub.RouteID, Nonce: auth.Nonce})
		}
	}
	sids := make([]string, 0, len(cover))
	for sid := range cover {
		sids = append(sids, sid)
	}
	sort.Strings(sids)
	incs := s.identities(ctx, sids)
	// A session whose identity is unknown this tick (lookup failed, or the
	// store has none) is not read at all: reading it with an empty
	// incarnation would lose watched turn ends and stale permissions.
	known := sids[:0:0]
	for _, sid := range sids {
		if incs[sid] != "" {
			known = append(known, sid)
		}
	}
	sids = known
	now := s.now()
	nowMs := now.UnixMilli()
	// Baselines and incarnation changes come first, for every covered session.
	for _, sid := range sids {
		inc := incs[sid]
		needIncarnation := inc != "" && s.store.incarnation(sid) != inc
		var needBaseline []wakeTarget
		for _, t := range cover[sid] {
			if !s.store.based(t.ClientID, t.RouteID, t.Nonce, sid) {
				needBaseline = append(needBaseline, t)
			}
		}
		// Did each unbaselined subscription's eager pass observe this
		// session? (Decided before observing it now.)
		origin := map[wakeTarget]int{}
		for _, t := range needBaseline {
			origin[t] = s.store.origin(t.ClientID, t.RouteID, t.Nonce, sid, inc)
		}
		if !needIncarnation && len(needBaseline) == 0 {
			continue
		}
		// A baseline or reset needs every source's real head, from one
		// coherent per-source read. If the read fails, defer both rather than
		// fabricating heads of 0 (which would replay old activity as new).
		heads, gens, ok := s.sourceHeads(ctx, sid)
		if !ok {
			continue
		}
		if needIncarnation {
			s.store.observeIncarnation(sid, inc, heads, gens, nowMs)
		}
		for _, t := range needBaseline {
			start := heads
			switch origin[t] {
			case originNew:
				// Created after the subscription's eager pass: every event
				// of it is new to the subscription.
				start = zeroHeads(heads)
			case originReused:
				start = s.store.reusedStart(sid, heads, gens)
			}
			s.store.baseline(t.ClientID, t.RouteID, t.Nonce, sid, start, gens)
		}
	}
	// Read: sessions whose inventory state changed first, then round-robin.
	s.mu.Lock()
	var order []string
	seen := map[string]bool{}
	// Changed-state sessions get at most half a tick, so sessions that change
	// on every tick can never starve the round-robin set.
	for _, sid := range sids {
		if st := byID[sid].State; st != s.lastState[sid] && len(order) < s.perTick/2 {
			order = append(order, sid)
			seen[sid] = true
		}
		s.lastState[sid] = byID[sid].State
	}
	scanned := 0
	for ; scanned < len(sids) && len(order) < s.perTick; scanned++ {
		sid := sids[(s.rr+scanned)%len(sids)]
		if !seen[sid] {
			order = append(order, sid)
			seen[sid] = true
		}
	}
	if len(sids) > 0 {
		// Advance by the positions actually scanned, so a tick shared with
		// changed-state sessions never skips round-robin sessions.
		s.rr = (s.rr + scanned) % len(sids)
	}
	if len(order) > s.perTick {
		order = order[:s.perTick]
	}
	s.mu.Unlock()
	queued := false
	for _, sid := range order {
		if s.read(ctx, sid, incs[sid], cover[sid], nowMs) {
			queued = true
		}
		s.mu.Lock()
		s.checked[sid] = now
		s.mu.Unlock()
	}
	// Lag: any covered session unread for more than 60 s.
	s.mu.Lock()
	s.lagging = false
	for _, sid := range sids {
		if t, ok := s.checked[sid]; !ok || now.Sub(t) > 60*time.Second {
			if ok || len(sids) > s.perTick*30 {
				s.lagging = true
			}
		}
	}
	for sid := range s.checked {
		if _, ok := cover[sid]; !ok {
			delete(s.checked, sid)
			delete(s.lastState, sid)
		}
	}
	s.mu.Unlock()
	if queued {
		s.poke()
	}
}

func (s *WakeService) identities(ctx context.Context, sids []string) map[string]string {
	out := map[string]string{}
	for i := 0; i < len(sids); i += amuxstore.MaxIdentityLookup {
		j := min(i+amuxstore.MaxIdentityLookup, len(sids))
		got, err := s.backend.SessionIdentities(ctx, sids[i:j])
		if err != nil {
			continue
		}
		for id, v := range got {
			out[id] = v.Incarnation
		}
	}
	return out
}

func zeroHeads(heads map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(heads))
	for k := range heads {
		out[k] = 0
	}
	return out
}

// BaselineSubscription baselines a just-created subscription eagerly: every
// session in its grant's scope is baselined at its current heads NOW (so an
// event raised right after subscribing is never mistaken for history), and
// every visible session's incarnation is recorded as seen (so a session that
// existed before the subscription is never treated as new later).
func (s *WakeService) BaselineSubscription(ctx context.Context, g Grant, routeID string) {
	auth := s.a.wakeAuthSnapshot()
	if auth == nil {
		return
	}
	rows, err := s.backend.Snapshot(ctx)
	if err != nil {
		return
	}
	byID := indexRows(rows)
	var ids []string
	for _, r := range rows {
		if validSessionID(r.ID) {
			ids = append(ids, r.ID)
		}
	}
	incs, complete := s.identitiesStrict(ctx, ids)
	seen := map[string]string{}
	for _, r := range rows {
		inc := incs[r.ID]
		if inc == "" {
			continue
		}
		// Every visible session is observed, in scope or not, whether or not
		// its heads can be read now: a session that existed at subscribe
		// time is never later mistaken for a new one.
		seen[r.ID] = inc
		if inScope(g, r, byID) && !effectivelyArchived(r, byID) {
			if heads, gens, ok := s.sourceHeads(ctx, r.ID); ok {
				s.store.baseline(g.ClientID, routeID, auth.Nonce, r.ID, heads, gens)
			}
		}
	}
	if complete {
		s.store.markEager(g.ClientID, routeID, auth.Nonce, seen)
	}
	_ = s.store.flush(true)
}

// sourceHeads reads the current head of each of sid's sources.
func (s *WakeService) sourceHeads(ctx context.Context, sid string) (map[string]int64, map[string]string, bool) {
	page, ok := s.backend.SourceEvents(ctx, sid, map[string]int64{}, 1)
	if !ok {
		return nil, nil, false
	}
	heads, gens := map[string]int64{}, map[string]string{}
	for _, src := range page.Sources {
		heads[src.Kind], gens[src.Kind] = src.Head, src.Generation
	}
	return heads, gens, true
}

// read reads sid's sources forward, each from the lowest mark of its covering
// subscriptions for that source, at most maxPages rounds. Each source is read
// by its own stable index (runtimeevents.ReadSources), never by a joined
// ordinal: an append to one source cannot shift another's positions. A
// source whose head fell below the cursor was replaced and is re-baselined.
func (s *WakeService) read(ctx context.Context, sid, inc string, targets []wakeTarget, nowMs int64) bool {
	queued := false
	for p := 0; p < s.maxPages; p++ {
		low, ok := s.store.lowMarks(sid, targets)
		if !ok {
			return queued
		}
		page, ok := s.backend.SourceEvents(ctx, sid, low, s.pageLimit)
		if !ok {
			return queued
		}
		more := false
		for _, src := range page.Sources {
			if src.Generation == "" {
				// An absent source (an existing file always has a
				// generation) is unknown, not empty or truncated: its marks
				// and last known generation stand, so a restored copy later
				// continues instead of replaying history.
				continue
			}
			if s.store.checkGeneration(sid, src.Kind, src.Generation) {
				more = true // renewed source: re-read it from index 0
				continue
			}
			if src.Head < low[src.Kind] {
				s.store.resetSource(sid, src.Kind, src.Head)
				continue
			}
			if len(src.Events) == 0 {
				continue
			}
			if s.store.processSource(sid, inc, src.Kind, src.Generation, src.First, src.Events, targets, nowMs) {
				queued = true
			}
			if src.First+int64(len(src.Events))-1 < src.Head {
				more = true
			}
		}
		if !more {
			return queued
		}
	}
	return queued
}

// sourceEvent reads the event at index of source kind in sid, or nil.
func (s *WakeService) sourceEvent(ctx context.Context, sid, kind, gen string, index int64) *hp.RuntimeEvent {
	if index < 1 {
		return nil
	}
	page, ok := s.backend.SourceEvents(ctx, sid, map[string]int64{kind: index - 1}, 1)
	if !ok {
		return nil
	}
	for _, src := range page.Sources {
		// The index is meaningful only in the generation it was recorded in:
		// a replaced or rewritten file never yields unrelated content.
		if src.Kind == kind && src.First == index && len(src.Events) == 1 && runtimeevents.ContinuesGeneration(gen, src.Generation) {
			ev := src.Events[0]
			return &ev
		}
	}
	return nil
}

// ---- outbox (provider bridge) ----

func activateKey(clientID, routeID, digest string) string {
	return "a\x00" + clientID + "\x00" + routeID + "\x00" + digest
}

// Lease returns up to max leasable items, waiting up to wait for one.
// Activation items come from the authority; wake items only for active
// subscriptions (activation settles before any wake).
func (s *WakeService) Lease(ctx context.Context, max int, wait time.Duration) []WakeLeaseItem {
	if max <= 0 || max > hp.MaxDirectWakeBatch {
		max = hp.MaxDirectWakeBatch
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		if items := s.leaseOnce(max); len(items) > 0 {
			return items
		}
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		case <-s.notify:
		case <-time.After(time.Second):
		}
	}
}

func (s *WakeService) leaseOnce(max int) []WakeLeaseItem {
	auth := s.a.wakeAuthSnapshot()
	if auth == nil || s.store.degradedReason() != "" {
		return nil
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, l := range s.leases {
		if now.After(l.expires) {
			for _, e := range l.items {
				delete(s.leasedIDs, e.ID)
			}
			delete(s.leases, id)
		}
	}
	var out []WakeLeaseItem
	var entries []wakeLeaseEntry
	active := map[string]bool{}
	refs := map[string]wakeAuthSub{}
	for _, sub := range auth.Subs {
		k := subKey(sub.ClientID, sub.RouteID, auth.Nonce)
		refs[k] = sub
		switch sub.State {
		case wakeSubStateActive:
			active[k] = true
		case wakeSubStateActivate:
			key := activateKey(sub.ClientID, sub.RouteID, sub.SecretDigest)
			if _, leased := s.leasedIDs[key]; leased || len(out) == max || now.Before(s.actBackoff[key].nextAt) {
				continue
			}
			out = append(out, WakeLeaseItem{ID: key, Kind: "activate", RouteID: sub.RouteID, GrantRef: sub.GrantRef, Requester: sub.Requester, Secret: sub.Secret})
			entries = append(entries, wakeLeaseEntry{ID: key, Kind: "activate", ClientID: sub.ClientID, RouteID: sub.RouteID, Nonce: auth.Nonce, Digest: sub.SecretDigest})
		}
	}
	if len(out) < max {
		leased := map[string]bool{}
		for id := range s.leasedIDs {
			leased[id] = true
		}
		for _, it := range s.store.leasable(active, leased, max-len(out), now.Unix()) {
			sub := refs[subKey(it.ClientID, it.RouteID, it.Nonce)]
			out = append(out, WakeLeaseItem{ID: it.WakeID, Kind: "wake", RouteID: it.RouteID, WakeID: it.WakeID, GrantRef: sub.GrantRef})
			entries = append(entries, wakeLeaseEntry{ID: it.WakeID, Kind: "wake", Out: it})
		}
	}
	if len(out) == 0 {
		return nil
	}
	leaseID := newWakeID()
	s.leases[leaseID] = &wakeLease{expires: now.Add(wakeLeaseTTL), items: entries}
	for _, e := range entries {
		s.leasedIDs[e.ID] = leaseID
	}
	for i := range out {
		out[i].ID = leaseID + "\x00" + out[i].ID
	}
	return out
}

// Settle applies the provider's report of Harness's answers. Items whose
// lease expired or whose subscription the authority no longer vouches for are
// ignored (an expired lease's item is simply leased again).
func (s *WakeService) Settle(results []WakeSettleResult) {
	now := s.now()
	s.mu.Lock()
	var acts []wakeLeaseEntry
	var actStatus []string
	var wakes []wakeLeaseEntry
	var wakeStatus []string
	for _, r := range results {
		leaseID, itemID, ok := cutNUL(r.ID)
		if !ok {
			continue
		}
		l := s.leases[leaseID]
		if l == nil || now.After(l.expires) {
			continue
		}
		for _, e := range l.items {
			if e.ID != itemID || s.leasedIDs[e.ID] != leaseID {
				continue
			}
			delete(s.leasedIDs, e.ID)
			if e.Kind == "activate" {
				acts, actStatus = append(acts, e), append(actStatus, r.Status)
			} else {
				wakes, wakeStatus = append(wakes, e), append(wakeStatus, r.Status)
			}
		}
	}
	s.mu.Unlock()
	for i, e := range acts {
		key := activateKey(e.ClientID, e.RouteID, e.Digest)
		switch actStatus[i] {
		case hp.DirectWakeAccepted:
			_ = s.a.WakeSettleActivation(e.ClientID, e.RouteID, e.Nonce, e.Digest, true, "")
			s.clearActBackoff(key)
		case hp.DirectWakeRouteGone:
			_ = s.a.WakeSettleActivation(e.ClientID, e.RouteID, e.Nonce, e.Digest, false, WakeFailRouteGone)
			s.clearActBackoff(key)
		default:
			s.mu.Lock()
			r := s.actBackoff[key]
			r.attempts++
			d := wakeBackoffMin << min(r.attempts-1, 5)
			if d > wakeBackoffMax {
				d = wakeBackoffMax
			}
			r.nextAt = now.Add(d)
			s.actBackoff[key] = r
			s.mu.Unlock()
		}
	}
	auth := s.a.wakeAuthSnapshot()
	vouched := map[string]bool{}
	if auth != nil {
		for _, sub := range auth.Subs {
			vouched[subKey(sub.ClientID, sub.RouteID, auth.Nonce)] = true
		}
	}
	for i, e := range wakes {
		if !vouched[subKey(e.Out.ClientID, e.Out.RouteID, e.Out.Nonce)] {
			continue
		}
		s.store.settle(e.Out, wakeStatus[i], now.Unix())
		if wakeStatus[i] == hp.DirectWakeRouteGone {
			_ = s.a.WakeFailRoute(e.Out.ClientID, e.Out.RouteID, e.Out.Nonce)
		}
	}
	_ = s.store.flush(true)
	s.poke()
}

func (s *WakeService) clearActBackoff(key string) {
	s.mu.Lock()
	delete(s.actBackoff, key)
	s.mu.Unlock()
}

func cutNUL(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}

// Routes is the complete route-sync snapshot: every activating and active
// subscription (bounded by maxWakeSubsPerHost, so never truncated).
func (s *WakeService) Routes() []hp.DirectWakeRouteRef {
	auth := s.a.wakeAuthSnapshot()
	out := []hp.DirectWakeRouteRef{}
	if auth == nil {
		return out
	}
	for _, sub := range auth.Subs {
		out = append(out, hp.DirectWakeRouteRef{RouteID: sub.RouteID, GrantRef: sub.GrantRef, State: sub.State})
	}
	return out
}

// ---- data side of the direct API ----

// authorizedSub confirms the grant's subscription is vouched for now.
func (s *WakeService) authorizedSub(l *Lease, routeID string) (string, bool) {
	auth := s.a.wakeAuthSnapshot()
	if auth == nil {
		return "", false
	}
	for _, sub := range auth.Subs {
		if sub.ClientID == l.ClientID && sub.RouteID == routeID {
			return auth.Nonce, true
		}
	}
	return auth.Nonce, false
}

// visibility builds the fetch-time visibility check for a grant from a fresh
// inventory and the sessions' current incarnations.
func (s *WakeService) visibility(ctx context.Context, g Grant) (wakeVisibility, error) {
	rows, err := s.backend.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	byID := indexRows(rows)
	var ids []string
	for _, r := range rows {
		if validSessionID(r.ID) {
			ids = append(ids, r.ID)
		}
	}
	incs := s.identities(ctx, ids)
	return func(sid, inc string) (bool, bool) {
		row, ok := byID[sid]
		if !ok {
			return false, false
		}
		in := inScope(g, row, byID) && !effectivelyArchived(row, byID) && !isConsole(row)
		cur := incs[sid]
		return in, cur == "" || cur == inc
	}, nil
}

// verifiedEvent reads a pending item's event from its source and returns it
// only if it is provably the recorded event: same source generation, the
// recorded event type, and for a permission the exact item_id, request_id and
// runtime_generation; and the session is still at the record's incarnation
// AFTER the read (so a replacement or an id reuse between the fetch-time
// visibility check and this lookup can never put unrelated content beside a
// recorded permission). On any mismatch the event is omitted.
func (s *WakeService) verifiedEvent(ctx context.Context, it WakeItem) *hp.RuntimeEvent {
	ev := s.sourceEvent(ctx, it.SessionID, it.Source, it.Generation, it.Index)
	if ev == nil {
		return nil
	}
	switch it.Kind {
	case WakeKindPermission:
		var p permPayload
		_ = json.Unmarshal(ev.Payload, &p)
		if ev.Type != hp.TypePermissionRequest || ev.ItemID != it.ItemID || p.RequestID != it.RequestID || p.RuntimeGeneration != it.RuntimeGeneration {
			return nil
		}
	case WakeKindTurnEnd:
		if ev.Type != hp.TypeTurnEnd {
			return nil
		}
	default:
		return nil
	}
	cur, ok := s.identitiesStrict(ctx, []string{it.SessionID})
	if !ok || cur[it.SessionID] != it.Incarnation {
		return nil
	}
	return ev
}

// uncovered lists the grant's in-scope sessions with no store identity.
func (s *WakeService) uncovered(ctx context.Context, g Grant) []string {
	out := []string{}
	rows, err := s.backend.Snapshot(ctx)
	if err != nil {
		return out
	}
	byID := indexRows(rows)
	var ids []string
	for _, r := range rows {
		if validSessionID(r.ID) && inScope(g, r, byID) && !effectivelyArchived(r, byID) {
			ids = append(ids, r.ID)
		}
	}
	got, ok := s.identitiesStrict(ctx, ids)
	if !ok {
		return out // unknown right now: claim nothing either way
	}
	for _, id := range ids {
		if got[id] == "" {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// identitiesStrict is identities that also reports whether every lookup
// succeeded.
func (s *WakeService) identitiesStrict(ctx context.Context, sids []string) (map[string]string, bool) {
	out := map[string]string{}
	for i := 0; i < len(sids); i += amuxstore.MaxIdentityLookup {
		j := min(i+amuxstore.MaxIdentityLookup, len(sids))
		got, err := s.backend.SessionIdentities(ctx, sids[i:j])
		if err != nil {
			return nil, false
		}
		for id, v := range got {
			out[id] = v.Incarnation
		}
	}
	return out, true
}

// sessionIncarnation returns sid's current store incarnation if sid is in the
// grant's scope.
func (s *WakeService) sessionIncarnation(ctx context.Context, g Grant, sid string) (string, bool, error) {
	rows, err := s.backend.Snapshot(ctx)
	if err != nil {
		return "", false, err
	}
	byID := indexRows(rows)
	row, ok := byID[sid]
	if !ok || !inScope(g, row, byID) || isConsole(row) {
		return "", false, nil
	}
	return s.identities(ctx, []string{sid})[sid], true, nil
}

var _ = core.Session{}
