package direct

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"amux/internal/runtimeevents"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// wake.json: high-frequency wake data (docs/direct-wake-contract.md §6.2-§8).
// It is a separate file with its own lock so wake-path disk trouble can never
// degrade the authority: a failed write here only makes wake-ups degraded.
// Every entry is keyed by (grant client id, route id, policy nonce) and is
// inert unless the authority still vouches for that triple.

const (
	wakeFile            = "wake.json"
	wakeFileVersion     = 1
	wakeRetention       = 24 * time.Hour
	wakeDeliverDeadline = 900 * time.Second
	wakeMaxClosed       = 128 // non-open records per subscription
	wakeMaxOpen         = 256 // open permissions per subscription
	wakeMaxTombs        = 1024
	wakeMaxWatches      = 64
	wakeFlushEvery      = 250 * time.Millisecond
	wakeLeaseTTL        = 30 * time.Second
	wakeBackoffMin      = 2 * time.Second
	wakeBackoffMax      = 60 * time.Second
	wakeFetchMax        = 64
)

// Record kinds and states (fetch vocabulary).
const (
	WakeKindPermission = "permission_request"
	WakeKindTurnEnd    = "turn_end"

	WakeStateOpen      = "open"
	WakeStateResolved  = "resolved"
	WakeStateStale     = "stale"
	WakeStateWithdrawn = "withdrawn"
	WakeStateDone      = "done"
)

// Outbox states.
const (
	wakeOutPending   = "pending"
	wakeOutDelivered = "delivered"
	wakeOutGone      = "gone"
	wakeOutExpired   = "expired"
)

// Gap reasons.
const (
	WakeGapCap          = "cap"
	WakeGapExpired      = "expired"
	WakeGapOpenOverflow = "open_overflow"
	WakeGapReset        = "reset"
)

var errWakeDegraded = errors.New("wake_degraded")

type wakeData struct {
	V    int                     `json:"v"`
	Subs map[string]*wakeSubData `json:"subs"`
	// Inc is the last observed store incarnation per session id (shared:
	// incarnation is a fact about the session, not about a subscription).
	Inc map[string]string `json:"inc,omitempty"`
	// Gen is the last observed generation per (session, source kind): marks
	// and record addresses are meaningful only within one source generation.
	Gen map[string]string `json:"gen,omitempty"`
}

type wakeSubData struct {
	ClientID string `json:"clientId"`
	RouteID  string `json:"routeId"`
	Nonce    string `json:"nonce"`
	Seq      int64  `json:"seq"`
	// Marks are per (session, source kind): key sid+"\x00"+kind. Based marks
	// the sessions this subscription has baselined; a source kind absent from
	// Marks for a based session is a source that appeared later (starts at 0).
	Marks   map[string]int64       `json:"marks,omitempty"`
	Based   map[string]bool        `json:"based,omitempty"`
	Watches map[string]string      `json:"watches,omitempty"` // sid -> incarnation
	Records map[string]*wakeRecord `json:"records,omitempty"` // wake id ->
	Tombs   map[string]int64       `json:"tombs,omitempty"`   // event identity -> expiry (unix)
	Outbox  map[string]*wakeOut    `json:"outbox,omitempty"`  // wake id ->
	// GapThrough/GapReason: the highest seq of a record that was dropped
	// without the phone acknowledging it, and why.
	GapThrough int64  `json:"gapThrough,omitempty"`
	GapReason  string `json:"gapReason,omitempty"`
	// LossThrough is a monotonic watermark: the highest seq of a pending
	// record (open permission, unacked turn end) that was ever dropped.
	// RecoveredThrough is the highest watermark the phone confirmed it has
	// reconciled (by a full direct read). A snapshot is complete only when
	// RecoveredThrough >= LossThrough; an incomplete snapshot alone never
	// clears the loss.
	LossThrough      int64 `json:"lossThrough,omitempty"`
	RecoveredThrough int64 `json:"recoveredThrough,omitempty"`
}

type wakeRecord struct {
	WakeID            string `json:"wakeId"`
	Identity          string `json:"identity"`
	Kind              string `json:"kind"`
	State             string `json:"state"`
	Seq               int64  `json:"seq"`
	Acked             bool   `json:"acked,omitempty"`
	SessionID         string `json:"sessionId"`
	Incarnation       string `json:"incarnation,omitempty"`
	ItemID            string `json:"itemId,omitempty"`
	RequestID         string `json:"requestId,omitempty"`
	RuntimeGeneration string `json:"runtimeGeneration,omitempty"`
	// Source and Index locate the event: the source kind and the event's
	// 1-based index within that append-only source (stable, unlike a joined
	// ordinal).
	Source    string `json:"source"`
	Index     int64  `json:"index"`
	SourceGen string `json:"sourceGen,omitempty"`
	CreatedAt int64  `json:"createdAt"` // unix millis
	Delivered bool   `json:"delivered,omitempty"`
}

type wakeOut struct {
	CreatedAt int64  `json:"createdAt"` // unix seconds
	NextAt    int64  `json:"nextAt"`
	Attempts  int    `json:"attempts,omitempty"`
	State     string `json:"state"`
	// Record is the record wake id this outbox item announces (equal to the
	// key for a new record; a resolution wake has its own id).
	Record string `json:"record,omitempty"`
}

// wakeStore owns wake.json.
type wakeStore struct {
	mu        sync.Mutex
	dir       string
	write     func(dir, name string, data []byte) error
	now       func() time.Time
	data      *wakeData
	dirty     bool
	lastFlush time.Time
	degraded  string
}

func subKey(clientID, routeID, nonce string) string {
	return clientID + "\x00" + routeID + "\x00" + nonce
}

func newWakeData() *wakeData {
	return &wakeData{V: wakeFileVersion, Subs: map[string]*wakeSubData{}, Inc: map[string]string{}, Gen: map[string]string{}}
}

// openWakeStore loads wake.json. An unreadable or unknown-version file is
// discarded (it only ever holds re-derivable or best-effort data); that is
// logged by the caller through the returned note.
func openWakeStore(dir string, write func(string, string, []byte) error, now func() time.Time) (*wakeStore, string) {
	if write == nil {
		write = writeAtomic
	}
	if now == nil {
		now = time.Now
	}
	ws := &wakeStore{dir: dir, write: write, now: now, data: newWakeData()}
	raw, err := os.ReadFile(filepath.Join(dir, wakeFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return ws, ""
	case err != nil:
		return ws, fmt.Sprintf("wake store unreadable (%v); starting empty", err)
	}
	var d wakeData
	if err := json.Unmarshal(raw, &d); err != nil || d.V != wakeFileVersion {
		ws.dirty = true
		return ws, "wake store malformed or from another version; starting empty"
	}
	if d.Subs == nil {
		d.Subs = map[string]*wakeSubData{}
	}
	if d.Inc == nil {
		d.Inc = map[string]string{}
	}
	if d.Gen == nil {
		d.Gen = map[string]string{}
	}
	ws.data = &d
	for k, sub := range d.Subs {
		if sub == nil {
			delete(d.Subs, k)
			continue
		}
		ws.subLocked(sub.ClientID, sub.RouteID, sub.Nonce) // nil maps from omitempty fields
	}
	return ws, ""
}

// reconcile drops every subscription the authority no longer vouches for
// (startup and every detector tick). Nil auth (wake off) drops everything.
func (ws *wakeStore) reconcile(auth *wakeAuth) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	keep := map[string]bool{}
	if auth != nil {
		for _, s := range auth.Subs {
			keep[subKey(s.ClientID, s.RouteID, auth.Nonce)] = true
		}
	}
	for k := range ws.data.Subs {
		if !keep[k] {
			delete(ws.data.Subs, k)
			ws.dirty = true
		}
	}
	if len(ws.data.Subs) == 0 && len(ws.data.Inc) > 0 {
		ws.data.Inc = map[string]string{}
		ws.dirty = true
	}
}

// sub returns the data for an authorized subscription, creating it.
func (ws *wakeStore) subLocked(clientID, routeID, nonce string) *wakeSubData {
	k := subKey(clientID, routeID, nonce)
	s := ws.data.Subs[k]
	if s == nil {
		s = &wakeSubData{ClientID: clientID, RouteID: routeID, Nonce: nonce, Marks: map[string]int64{}, Based: map[string]bool{},
			Watches: map[string]string{}, Records: map[string]*wakeRecord{}, Tombs: map[string]int64{}, Outbox: map[string]*wakeOut{}}
		ws.data.Subs[k] = s
		ws.dirty = true
	}
	if s.Marks == nil {
		s.Marks = map[string]int64{}
	}
	if s.Based == nil {
		s.Based = map[string]bool{}
	}
	if s.Watches == nil {
		s.Watches = map[string]string{}
	}
	if s.Records == nil {
		s.Records = map[string]*wakeRecord{}
	}
	if s.Tombs == nil {
		s.Tombs = map[string]int64{}
	}
	if s.Outbox == nil {
		s.Outbox = map[string]*wakeOut{}
	}
	return s
}

func (ws *wakeStore) usableLocked() error {
	if ws.degraded != "" {
		return errWakeDegraded
	}
	return nil
}

// flush writes wake.json if dirty. force bypasses the batching interval. A
// rolled-back write keeps the data dirty for the next attempt; an ambiguous
// one makes wake-ups degraded until restart. Neither touches the authority.
func (ws *wakeStore) flush(force bool) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if !ws.dirty || ws.degraded != "" {
		return nil
	}
	if !force && ws.now().Sub(ws.lastFlush) < wakeFlushEvery {
		return nil
	}
	data, err := json.Marshal(ws.data)
	if err != nil {
		return err
	}
	err = ws.write(ws.dir, wakeFile, data)
	ws.lastFlush = ws.now()
	switch {
	case err == nil:
		ws.dirty = false
		return nil
	case errors.Is(err, errNotCommitted):
		return err
	default:
		ws.degraded = "wake store write durability unknown; restart `amux direct serve` to reconcile"
		return err
	}
}

func (ws *wakeStore) degradedReason() string {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.degraded
}

func eventIdentity(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func newWakeID() string {
	s, err := randomB64(16)
	if err != nil {
		panic(err)
	}
	return hp.DirectWakeIDPrefix + s
}

// ---- detector side ----

func markKey(sid, kind string) string { return sid + "\x00" + kind }

// based reports whether the subscription has baselined sid.
func (ws *wakeStore) based(clientID, routeID, nonce, sid string) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	s := ws.data.Subs[subKey(clientID, routeID, nonce)]
	return s != nil && s.Based[sid]
}

// lowMarks is, per source kind, the lowest mark among the targets that have
// baselined sid (a kind a target lacks counts as 0: a source that appeared
// after its baseline). ok is false when no target has baselined sid.
func (ws *wakeStore) lowMarks(sid string, targets []wakeTarget) (map[string]int64, bool) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	low := map[string]int64{}
	var subs []*wakeSubData
	for _, t := range targets {
		if s := ws.data.Subs[subKey(t.ClientID, t.RouteID, t.Nonce)]; s != nil && s.Based[sid] {
			subs = append(subs, s)
		}
	}
	if len(subs) == 0 {
		return nil, false
	}
	kinds := map[string]bool{}
	prefix := sid + "\x00"
	for _, s := range subs {
		for k := range s.Marks {
			if len(k) > len(prefix) && k[:len(prefix)] == prefix {
				kinds[k[len(prefix):]] = true
			}
		}
	}
	for kind := range kinds {
		first := true
		for _, s := range subs {
			m := s.Marks[markKey(sid, kind)] // absent = 0
			if first || m < low[kind] {
				low[kind], first = m, false
			}
		}
	}
	return low, true
}

// baseline sets ONLY this subscription's marks for sid to the sources' current
// heads (a new subscriber never moves another subscription's read position).
func (ws *wakeStore) baseline(clientID, routeID, nonce, sid string, heads map[string]int64, gens map[string]string) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	s := ws.subLocked(clientID, routeID, nonce)
	if s.Based[sid] {
		return
	}
	for kind, h := range heads {
		s.Marks[markKey(sid, kind)] = h
	}
	for kind, g := range gens {
		if _, seen := ws.data.Gen[markKey(sid, kind)]; !seen {
			ws.data.Gen[markKey(sid, kind)] = g
		}
	}
	s.Based[sid] = true
	ws.dirty = true
}

// resetSource re-baselines one source of sid at head for every subscription
// (the source was replaced or truncated: its old indexes mean nothing now, and
// nothing in it is replayed as new).
func (ws *wakeStore) resetSource(sid, kind string, head int64) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, s := range ws.data.Subs {
		if s.Based[sid] {
			if m, ok := s.Marks[markKey(sid, kind)]; !ok || m != head {
				s.Marks[markKey(sid, kind)] = head
				ws.dirty = true
			}
		}
	}
}

// checkGeneration compares a source's observed generation with the stored
// one. A changed generation (file replaced, or truncated and rewritten, even
// to the same or a larger head) is a new source incarnation: every
// subscription's mark for it restarts at 0, so nothing written to the new file
// is skipped. It reports whether the source was renewed.
func (ws *wakeStore) checkGeneration(sid, kind, gen string) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	k := markKey(sid, kind)
	prev, seen := ws.data.Gen[k]
	if seen && !runtimeevents.SameGeneration(prev, gen) && runtimeevents.ContinuesGeneration(prev, gen) {
		// A copy of the same transcript (restore-before-resume): indexes keep
		// their meaning, so marks stand and copied history is not replayed.
		ws.data.Gen[k] = gen
		ws.dirty = true
		return false
	}
	if !seen || runtimeevents.SameGeneration(prev, gen) {
		ws.data.Gen[k] = runtimeevents.MergeGeneration(prev, gen)
		if ws.data.Gen[k] != prev {
			ws.dirty = true
		}
		return false
	}
	ws.data.Gen[k] = gen
	for _, s := range ws.data.Subs {
		if s.Based[sid] {
			s.Marks[k] = 0
		}
	}
	ws.dirty = true
	return true
}

func (ws *wakeStore) generation(sid, kind string) string {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.data.Gen[markKey(sid, kind)]
}

// observeIncarnation records sid's incarnation. On a change it re-baselines
// every subscription's marks for sid at heads, deletes watches bound to
// another incarnation, and makes open records of the old incarnation stale.
func (ws *wakeStore) observeIncarnation(sid, inc string, heads map[string]int64, gens map[string]string, nowMs int64) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	prev, seen := ws.data.Inc[sid]
	if seen && prev == inc {
		return false
	}
	ws.data.Inc[sid] = inc
	ws.dirty = true
	if !seen {
		return false
	}
	prefix := sid + "\x00"
	for _, s := range ws.data.Subs {
		if s.Based[sid] {
			for k := range s.Marks {
				if len(k) > len(prefix) && k[:len(prefix)] == prefix {
					delete(s.Marks, k)
				}
			}
			for kind, h := range heads {
				s.Marks[markKey(sid, kind)] = h
			}
		}
		for kind, g := range gens {
			ws.data.Gen[markKey(sid, kind)] = g
		}
		if w, ok := s.Watches[sid]; ok && w != inc {
			delete(s.Watches, sid)
		}
		for _, r := range s.Records {
			if r.SessionID == sid && r.Incarnation != inc && r.State == WakeStateOpen {
				ws.reviseLocked(s, r, WakeStateStale, nowMs)
			}
		}
	}
	return true
}

func (ws *wakeStore) incarnation(sid string) string {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.data.Inc[sid]
}

// reviseLocked gives a record a new state and a new, unacked revision; a
// delivered record also gets one resolution wake so its card clears promptly.
func (ws *wakeStore) reviseLocked(s *wakeSubData, r *wakeRecord, state string, nowMs int64) {
	if r.State == state {
		return
	}
	r.State = state
	s.Seq++
	r.Seq = s.Seq
	r.Acked = false
	ws.dirty = true
	if r.Delivered && state != WakeStateOpen {
		id := newWakeID()
		now := nowMs / 1000
		s.Outbox[id] = &wakeOut{CreatedAt: now, NextAt: now, State: wakeOutPending, Record: r.WakeID}
	}
}

// wakeEvent is one runtime event with its index within its source.
type wakeEvent struct {
	Source string
	Gen    string
	Index  int64
	Event  hp.RuntimeEvent
}

// wakeTarget is one subscription evaluating a session's events.
type wakeTarget struct {
	ClientID, RouteID, Nonce string
}

// processSource evaluates one source's contiguous run of events (indexes
// first..first+len-1) for every target that has baselined sid, skipping the
// events at or below that target's mark for this source, then raises the
// target's mark to the end of the run, all in one batch.
func (ws *wakeStore) processSource(sid, inc, kind, gen string, first int64, events []hp.RuntimeEvent, targets []wakeTarget, nowMs int64) bool {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	queued := false
	last := first + int64(len(events)) - 1
	for _, t := range targets {
		if s := ws.data.Subs[subKey(t.ClientID, t.RouteID, t.Nonce)]; s == nil || !s.Based[sid] {
			continue
		}
		s := ws.subLocked(t.ClientID, t.RouteID, t.Nonce) // normalizes maps decoded as nil
		mk := markKey(sid, kind)
		mark := s.Marks[mk]
		for i, ev := range events {
			idx := first + int64(i)
			if idx <= mark {
				continue
			}
			if ws.evaluateLocked(s, sid, inc, wakeEvent{Source: kind, Gen: gen, Index: idx, Event: ev}, nowMs) {
				queued = true
			}
		}
		if last > mark {
			s.Marks[mk] = last
			ws.dirty = true
		}
		ws.retainLocked(s, nowMs/1000)
	}
	return queued
}

type permPayload struct {
	RequestID         string `json:"request_id"`
	RuntimeGeneration string `json:"runtime_generation"`
}

func (ws *wakeStore) evaluateLocked(s *wakeSubData, sid, inc string, e wakeEvent, nowMs int64) bool {
	ev := e.Event
	var p permPayload
	_ = json.Unmarshal(ev.Payload, &p)
	// A different runtime generation means the runtime that raised earlier
	// open permissions in this session is gone.
	if p.RuntimeGeneration != "" {
		for _, r := range s.Records {
			if r.SessionID == sid && r.State == WakeStateOpen && r.RuntimeGeneration != "" && r.RuntimeGeneration != p.RuntimeGeneration {
				ws.reviseLocked(s, r, WakeStateStale, nowMs)
			}
		}
	}
	switch ev.Type {
	case hp.TypePermissionRequest:
		if ev.ItemID == "" || p.RequestID == "" || p.RuntimeGeneration == "" {
			return false // generation-free history is never answerable
		}
		id := eventIdentity(sid, inc, WakeKindPermission, ev.ItemID, p.RequestID, p.RuntimeGeneration)
		return ws.addRecordLocked(s, &wakeRecord{Identity: id, Kind: WakeKindPermission, State: WakeStateOpen, SessionID: sid,
			Incarnation: inc, ItemID: ev.ItemID, RequestID: p.RequestID, RuntimeGeneration: p.RuntimeGeneration,
			Source: e.Source, Index: e.Index, SourceGen: e.Gen}, nowMs)
	case hp.TypePermissionResolved:
		for _, r := range s.Records {
			if r.SessionID == sid && r.State == WakeStateOpen && r.RequestID == p.RequestID &&
				(p.RuntimeGeneration == "" || r.RuntimeGeneration == p.RuntimeGeneration) {
				ws.reviseLocked(s, r, WakeStateResolved, nowMs)
			}
		}
	case hp.TypeTurnEnd:
		if w, ok := s.Watches[sid]; !ok || w != inc {
			return false
		}
		// The generation's file identity part is stable across first-line
		// completion; identity uses it so a renewed file's turn ends are new.
		genID, _, _ := strings.Cut(e.Gen, ".")
		id := eventIdentity(sid, inc, WakeKindTurnEnd, e.Source, genID, fmt.Sprint(e.Index))
		return ws.addRecordLocked(s, &wakeRecord{Identity: id, Kind: WakeKindTurnEnd, State: WakeStateDone, SessionID: sid,
			Incarnation: inc, Source: e.Source, Index: e.Index, SourceGen: e.Gen}, nowMs)
	}
	return false
}

func (ws *wakeStore) addRecordLocked(s *wakeSubData, r *wakeRecord, nowMs int64) bool {
	if _, dead := s.Tombs[r.Identity]; dead {
		return false
	}
	for _, x := range s.Records {
		if x.Identity == r.Identity {
			return false
		}
	}
	r.WakeID = newWakeID()
	r.CreatedAt = nowMs
	s.Seq++
	r.Seq = s.Seq
	s.Records[r.WakeID] = r
	now := nowMs / 1000
	s.Outbox[r.WakeID] = &wakeOut{CreatedAt: now, NextAt: now, State: wakeOutPending, Record: r.WakeID}
	ws.dirty = true
	return true
}

func pendingRecord(r *wakeRecord) bool {
	return r.State == WakeStateOpen || (r.Kind == WakeKindTurnEnd && r.State == WakeStateDone && !r.Acked)
}

// retainLocked enforces the retention rules: 24 h; at most wakeMaxClosed
// non-open records (oldest first, never an open one); open permissions bounded
// separately with an open_overflow gap; tombstones bounded; finished outbox
// items dropped.
func (ws *wakeStore) retainLocked(s *wakeSubData, now int64) {
	drop := func(r *wakeRecord, reason string) {
		delete(s.Records, r.WakeID)
		delete(s.Outbox, r.WakeID)
		s.Tombs[r.Identity] = now + int64(wakeRetention/time.Second)
		if !r.Acked || r.State == WakeStateOpen {
			if r.Seq > s.GapThrough {
				s.GapThrough, s.GapReason = r.Seq, reason
			}
			if pendingRecord(r) && r.Seq > s.LossThrough {
				s.LossThrough = r.Seq
			}
		}
		ws.dirty = true
	}
	var open, closed []*wakeRecord
	for _, r := range s.Records {
		if now*1000-r.CreatedAt >= wakeRetention.Milliseconds() {
			drop(r, WakeGapExpired)
			continue
		}
		if r.State == WakeStateOpen {
			open = append(open, r)
		} else {
			closed = append(closed, r)
		}
	}
	bySeq := func(rs []*wakeRecord) { sort.Slice(rs, func(i, j int) bool { return rs[i].Seq < rs[j].Seq }) }
	if len(closed) > wakeMaxClosed {
		bySeq(closed)
		for _, r := range closed[:len(closed)-wakeMaxClosed] {
			drop(r, WakeGapCap)
		}
	}
	if len(open) > wakeMaxOpen {
		bySeq(open)
		for _, r := range open[:len(open)-wakeMaxOpen] {
			drop(r, WakeGapOpenOverflow)
		}
	}
	for id, exp := range s.Tombs {
		if now >= exp {
			delete(s.Tombs, id)
			ws.dirty = true
		}
	}
	if len(s.Tombs) > wakeMaxTombs {
		type kv struct {
			k string
			v int64
		}
		var all []kv
		for k, v := range s.Tombs {
			all = append(all, kv{k, v})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].v < all[j].v })
		for _, x := range all[:len(all)-wakeMaxTombs] {
			delete(s.Tombs, x.k)
		}
		ws.dirty = true
	}
	for id, o := range s.Outbox {
		if o.State != wakeOutPending {
			delete(s.Outbox, id)
			ws.dirty = true
		} else if now >= o.CreatedAt+int64(wakeDeliverDeadline/time.Second) {
			delete(s.Outbox, id) // expired: the record stays fetchable
			ws.dirty = true
		}
	}
}

// ---- watches ----

func (ws *wakeStore) setWatch(clientID, routeID, nonce, sid, inc string, on bool) (bool, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if err := ws.usableLocked(); err != nil {
		return false, err
	}
	s := ws.subLocked(clientID, routeID, nonce)
	if !on {
		if _, ok := s.Watches[sid]; ok {
			delete(s.Watches, sid)
			ws.dirty = true
		}
		return false, nil
	}
	if _, ok := s.Watches[sid]; !ok && len(s.Watches) >= wakeMaxWatches {
		return false, errWakeCapacity
	}
	if s.Watches[sid] != inc {
		s.Watches[sid] = inc
		ws.dirty = true
	}
	return true, nil
}

func (ws *wakeStore) watches(clientID, routeID, nonce string) []WakeWatch {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	s := ws.data.Subs[subKey(clientID, routeID, nonce)]
	out := []WakeWatch{}
	if s == nil {
		return out
	}
	for sid, inc := range s.Watches {
		out = append(out, WakeWatch{SessionID: sid, Incarnation: inc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// ---- fetch / ack ----

// WakeItem is one fetched record. A clearing revision (resolved, stale,
// withdrawn) carries only the fields without omitempty plus CreatedAt.
type WakeItem struct {
	Seq               int64  `json:"seq"`
	WakeID            string `json:"wake_id"`
	Kind              string `json:"kind"`
	State             string `json:"state"`
	Acked             bool   `json:"acked"`
	SessionID         string `json:"session_id,omitempty"`
	Incarnation       string `json:"incarnation,omitempty"`
	ItemID            string `json:"item_id,omitempty"`
	RequestID         string `json:"request_id,omitempty"`
	RuntimeGeneration string `json:"runtime_generation,omitempty"`
	// Source and Index locate the event within its append-only source.
	Source string `json:"source,omitempty"`
	Index  int64  `json:"index,omitempty"`
	// Generation is the source file generation the index belongs to (opaque).
	Generation string `json:"generation,omitempty"`
	// Event is the runtime event itself, read from its source when fetched
	// (pending items only), over this authenticated direct connection.
	Event     *hp.RuntimeEvent `json:"event,omitempty"`
	CreatedAt int64            `json:"created_at"`
}

// WakeGap reports dropped records.
type WakeGap struct {
	ThroughSeq int64  `json:"through_seq"`
	Reason     string `json:"reason"`
}

// WakeWatch is one watched session instance.
type WakeWatch struct {
	SessionID   string `json:"session_id"`
	Incarnation string `json:"incarnation"`
}

// wakeVisibility answers, at fetch time, whether a record's session may be
// shown in full: in scope (and not console), and at its current incarnation.
type wakeVisibility func(sid, inc string) (inScope, current bool)

func itemOf(r *wakeRecord) WakeItem {
	it := WakeItem{Seq: r.Seq, WakeID: r.WakeID, Kind: r.Kind, State: r.State, Acked: r.Acked, CreatedAt: r.CreatedAt}
	if pendingRecord(r) {
		it.SessionID, it.Incarnation, it.Source, it.Index, it.Generation = r.SessionID, r.Incarnation, r.Source, r.Index, r.SourceGen
		if r.Kind == WakeKindPermission {
			it.ItemID, it.RequestID, it.RuntimeGeneration = r.ItemID, r.RequestID, r.RuntimeGeneration
		}
	}
	return it
}

// applyVisibilityLocked turns pending records that may no longer be shown
// into clearing revisions: out of scope -> withdrawn; another incarnation ->
// stale (an unacked turn end becomes withdrawn either way: it carries no
// action).
func (ws *wakeStore) applyVisibilityLocked(s *wakeSubData, vis wakeVisibility, nowMs int64) {
	for _, r := range s.Records {
		if !pendingRecord(r) {
			continue
		}
		inScope, current := vis(r.SessionID, r.Incarnation)
		switch {
		case !inScope:
			ws.reviseLocked(s, r, WakeStateWithdrawn, nowMs)
		case !current && r.Kind == WakeKindPermission:
			ws.reviseLocked(s, r, WakeStateStale, nowMs)
		case !current:
			ws.reviseLocked(s, r, WakeStateWithdrawn, nowMs)
		}
	}
}

// WakeFetchRequest is the body of POST /direct/v1/wake/events.
type WakeFetchRequest struct {
	RouteID    string `json:"route_id"`
	AfterSeq   int64  `json:"after_seq,omitempty"`
	Limit      int    `json:"limit,omitempty"`
	Snapshot   bool   `json:"snapshot,omitempty"`
	SnapshotHW int64  `json:"snapshot_hw,omitempty"`
	// RecoveredThrough (snapshot only) confirms the phone reconciled every
	// pending record lost through this loss watermark (from LossThrough).
	RecoveredThrough int64 `json:"recovered_through,omitempty"`
}

// WakeFetchResponse answers it.
type WakeFetchResponse struct {
	RouteID    string     `json:"route_id"`
	Items      []WakeItem `json:"items"`
	NextSeq    int64      `json:"next_seq"`
	HasMore    bool       `json:"has_more"`
	Gap        *WakeGap   `json:"gap"`
	SnapshotHW int64      `json:"snapshot_hw,omitempty"`
	Complete   *bool      `json:"complete,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	// LossThrough is set on an incomplete final snapshot page: echo it as
	// recovered_through after reconciling from history.
	LossThrough int64 `json:"loss_through,omitempty"`
}

func (ws *wakeStore) fetch(clientID, routeID, nonce string, req WakeFetchRequest, vis wakeVisibility, nowMs int64) (WakeFetchResponse, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	resp := WakeFetchResponse{RouteID: routeID, Items: []WakeItem{}, NextSeq: req.AfterSeq}
	if err := ws.usableLocked(); err != nil {
		return resp, err
	}
	limit := req.Limit
	if limit <= 0 || limit > wakeFetchMax {
		limit = wakeFetchMax
	}
	s := ws.data.Subs[subKey(clientID, routeID, nonce)]
	if s == nil {
		if req.Snapshot {
			t := true
			resp.Complete = &t
		}
		return resp, nil
	}
	ws.applyVisibilityLocked(s, vis, nowMs)
	ws.retainLocked(s, nowMs/1000)
	recs := make([]*wakeRecord, 0, len(s.Records))
	for _, r := range s.Records {
		recs = append(recs, r)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Seq < recs[j].Seq })
	if !req.Snapshot {
		if req.AfterSeq > s.Seq {
			// The client's cursor is past this store's sequence (an older
			// wake.json was restored): tell it to resync, never jump forward.
			resp.NextSeq = s.Seq
			resp.Gap = &WakeGap{ThroughSeq: s.Seq, Reason: WakeGapReset}
			return resp, nil
		}
		for _, r := range recs {
			if r.Seq <= req.AfterSeq {
				continue
			}
			if len(resp.Items) == limit {
				resp.HasMore = true
				break
			}
			resp.Items = append(resp.Items, itemOf(r))
			resp.NextSeq = r.Seq
		}
		if s.GapThrough > req.AfterSeq {
			resp.Gap = &WakeGap{ThroughSeq: s.GapThrough, Reason: s.GapReason}
		}
		return resp, nil
	}
	hw := req.SnapshotHW
	if hw == 0 {
		hw = s.Seq // captured on the first page
	}
	if hw > s.Seq || req.AfterSeq > hw || req.RecoveredThrough > s.LossThrough {
		return resp, errBadRequest
	}
	if req.RecoveredThrough > s.RecoveredThrough {
		s.RecoveredThrough = req.RecoveredThrough
		ws.dirty = true
	}
	resp.SnapshotHW = hw
	for _, r := range recs {
		if r.Seq <= req.AfterSeq || r.Seq > hw || !pendingRecord(r) {
			continue
		}
		if len(resp.Items) == limit {
			resp.HasMore = true
			break
		}
		resp.Items = append(resp.Items, itemOf(r))
		resp.NextSeq = r.Seq
	}
	if !resp.HasMore {
		resp.NextSeq = hw
		complete := s.RecoveredThrough >= s.LossThrough
		resp.Complete = &complete
		if !complete {
			resp.Reason, resp.LossThrough = s.GapReason, s.LossThrough
		}
	}
	return resp, nil
}

// WakeAckItem acknowledges one revision.
type WakeAckItem struct {
	WakeID string `json:"wake_id"`
	Seq    int64  `json:"seq"`
}

// ack applies only to the named current revision. An acked open permission
// stays pending; an acked turn end or clearing revision is removed.
func (ws *wakeStore) ack(clientID, routeID, nonce string, items []WakeAckItem, nowMs int64) (int, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if err := ws.usableLocked(); err != nil {
		return 0, err
	}
	s := ws.data.Subs[subKey(clientID, routeID, nonce)]
	if s == nil {
		return 0, nil
	}
	n := 0
	now := nowMs / 1000
	for _, it := range items {
		r := s.Records[it.WakeID]
		if r == nil || r.Seq != it.Seq {
			continue // unknown or stale revision: ignored
		}
		n++
		if r.Acked {
			continue
		}
		r.Acked = true
		ws.dirty = true
		if r.State != WakeStateOpen {
			delete(s.Records, r.WakeID)
			for id, o := range s.Outbox {
				if id == r.WakeID || o.Record == r.WakeID {
					delete(s.Outbox, id) // the phone already cleared this card
				}
			}
			s.Tombs[r.Identity] = now + int64(wakeRetention/time.Second)
		}
	}
	return n, nil
}

// ---- outbox ----

// wakeOutItem is a leasable wake outbox entry.
type wakeOutItem struct {
	ClientID, RouteID, Nonce, WakeID string
}

func (ws *wakeStore) leasable(active map[string]bool, leased map[string]bool, max int, now int64) []wakeOutItem {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	var out []wakeOutItem
	keys := make([]string, 0, len(ws.data.Subs))
	for k := range ws.data.Subs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !active[k] {
			continue
		}
		s := ws.data.Subs[k]
		ids := make([]string, 0, len(s.Outbox))
		for id := range s.Outbox {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return s.Outbox[ids[i]].CreatedAt < s.Outbox[ids[j]].CreatedAt })
		for _, id := range ids {
			o := s.Outbox[id]
			if o.State != wakeOutPending || o.NextAt > now || leased[id] {
				continue
			}
			if now >= o.CreatedAt+int64(wakeDeliverDeadline/time.Second) {
				o.State = wakeOutExpired
				ws.dirty = true
				continue
			}
			out = append(out, wakeOutItem{ClientID: s.ClientID, RouteID: s.RouteID, Nonce: s.Nonce, WakeID: id})
			if len(out) == max {
				return out
			}
		}
	}
	return out
}

// settle records Harness's answer for a wake outbox item.
func (ws *wakeStore) settle(it wakeOutItem, status string, now int64) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	s := ws.data.Subs[subKey(it.ClientID, it.RouteID, it.Nonce)]
	if s == nil {
		return
	}
	o := s.Outbox[it.WakeID]
	if o == nil || o.State != wakeOutPending {
		return
	}
	switch status {
	case hp.DirectWakeAccepted, hp.DirectWakeNoRecipient:
		o.State = wakeOutDelivered
		if r := s.Records[o.Record]; r != nil {
			r.Delivered = true
		}
	case hp.DirectWakeRouteGone:
		o.State = wakeOutGone
	default:
		o.Attempts++
		d := wakeBackoffMin << min(o.Attempts-1, 5)
		if d > wakeBackoffMax {
			d = wakeBackoffMax
		}
		o.NextAt = now + int64(d/time.Second)
	}
	ws.dirty = true
}
