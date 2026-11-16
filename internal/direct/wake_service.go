package direct

import (
	"context"
	"sort"
	"sync"
	"time"

	"amux/internal/core"
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
			if _, ok := s.store.mark(t.ClientID, t.RouteID, t.Nonce, sid); !ok {
				needBaseline = append(needBaseline, t)
			}
		}
		if !needIncarnation && len(needBaseline) == 0 {
			continue
		}
		// A baseline or reset needs the real head. If History cannot answer,
		// defer both to a later tick rather than fabricating head 0 (which
		// would replay old activity as new).
		page, ok := s.backend.History(ctx, sid, 0, 1)
		if !ok {
			continue
		}
		if needIncarnation {
			s.store.observeIncarnation(sid, inc, page.Head, nowMs)
		}
		for _, t := range needBaseline {
			s.store.baseline(t.ClientID, t.RouteID, t.Nonce, sid, page.Head)
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

// read reads sid forward from the lowest mark of its covering subscriptions,
// at most maxPages pages, processing each page atomically for the targets.
func (s *WakeService) read(ctx context.Context, sid, inc string, targets []wakeTarget, nowMs int64) bool {
	queued := false
	limit := s.pageLimit
	for p := 0; p < s.maxPages; p++ {
		low, any := int64(-1), false
		for _, t := range targets {
			if m, ok := s.store.mark(t.ClientID, t.RouteID, t.Nonce, sid); ok && (!any || m < low) {
				low, any = m, true
			}
		}
		if !any {
			return queued
		}
		before := low + 1 + int64(limit)
		page, ok := s.backend.History(ctx, sid, before, limit)
		if !ok || len(page.Events) == 0 {
			return queued
		}
		// Ordinals are dense (runtimeevents.historyPage: one per event; the
		// page is the run ending at page.Seq = min(before-1, head)). The reader
		// may trim the run's oldest events to fit its payload budget; then the
		// page starts above low+1 and processing it would skip the trimmed
		// events. Re-read with a smaller limit until the page starts at low+1
		// (one event always fits).
		first := page.Seq - int64(len(page.Events)) + 1
		if first > low+1 {
			if len(page.Events) <= 1 {
				return queued // cannot happen with a dense reader; never skip
			}
			limit = len(page.Events) - 1
			continue
		}
		limit = s.pageLimit
		proven := page.Seq
		events := make([]wakeEvent, 0, len(page.Events))
		for i, ev := range page.Events {
			if o := first + int64(i); o > low {
				events = append(events, wakeEvent{Ordinal: o, Event: ev})
			}
		}
		if len(events) > 0 && s.store.process(sid, inc, targets, events, nowMs) {
			queued = true
		}
		if proven <= low {
			return queued
		}
		s.store.advance(sid, targets, proven)
		if proven >= page.Head {
			return queued
		}
	}
	return queued
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
