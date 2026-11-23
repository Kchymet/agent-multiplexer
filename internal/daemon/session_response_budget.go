package daemon

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"amux/internal/core"
	"amux/internal/sessionrpc"
)

var errSessionResponseCapacity = errors.New("daemon session response capacity exceeded")

// sessionResponseBudget admits daemon-created response files across every
// session mailbox. It enforces, in one decision:
//
//   - a daemon-wide ceiling (files, bytes, receipt slots), sized for many
//     sessions rather than one;
//   - a per-session ceiling, so one noisy session or a burst of new agents can
//     never take the capacity every other session needs;
//   - priority tiers: best-effort telemetry may only fill the lower part of
//     either ceiling and normal calls a larger part, so credential reads and
//     control actions always find capacity reserved for them.
//
// It also keeps the counters `amux top` and `amux doctor` show.
type sessionResponseBudget struct {
	mu sync.Mutex

	maxFiles    int
	maxBytes    int64
	maxReceipts int
	// Per-subject ceilings.
	subjectFiles    int
	subjectBytes    int64
	subjectReceipts int
	// share is the fraction (num/den) of each ceiling a priority may fill.
	share map[sessionrpc.Priority]budgetShare

	files    int
	bytes    int64
	receipts int
	leases   map[sessionResponseKey]*sessionResponseLease
	subjects map[string]*subjectResponseUsage

	stats budgetCounters
	now   func() time.Time
}

// budgetShare is the part of a ceiling a priority may fill: admission needs
// usage below max*num/den.
type budgetShare struct{ num, den int }

func (s budgetShare) of(max int) int {
	if s.den == 0 {
		return max
	}
	return max * s.num / s.den
}

func (s budgetShare) ofBytes(max int64) int64 {
	if s.den == 0 {
		return max
	}
	return max / int64(s.den) * int64(s.num)
}

type subjectResponseUsage struct {
	files    int
	bytes    int64
	receipts int
	byClass  map[sessionrpc.Priority]int
}

type budgetCounters struct {
	admitted           uint64
	rejected           uint64
	rejectedByPriority map[string]uint64
	rejectedByVerb     map[string]uint64
	rejectedBySubject  map[string]uint64
	lastRejected       time.Time
	byClass            map[sessionrpc.Priority]int
}

// budgetCounterKeys bounds the rejection maps: verbs and subject ids come
// from client-written requests, so a hostile client must not grow them.
const budgetCounterKeys = 256

type sessionResponseKey struct {
	subjectID string
	requestID string
}

type sessionResponseLease struct {
	budget *sessionResponseBudget
	key    sessionResponseKey
	state  sessionResponseLeaseState
}

type sessionResponseLeaseState struct {
	reservedBytes int64
	receipt       bool
	committed     bool
	released      bool
	priority      sessionrpc.Priority
}

// daemonResponseSessions sizes the daemon-wide response budget for this many
// sessions' worth of full per-session queues. Each session is additionally held
// to one queue's worth (MaxResponseFiles / MaxResponseBytes), matching what its
// mailbox enforces on disk (Server.ensureResponseCapacity).
//
// It used to be sized for ONE session (128 files, 8 MiB) while being shared by
// every session. Each in-flight request reserves the worst-case envelope
// (~93 KiB) until commit, so only ~87 requests could be in flight across the
// whole daemon. With ~25 agents reporting hooks and status lines, a burst
// filled it; deferred clients timed out after 10s; their uncollected responses
// then held slots until MaxResponseAge (60s), so the budget never drained and
// credential-read starved for every agent (incident 2026-11-22, daemon
// livelock).
const daemonResponseSessions = 32

// Priority shares of each ceiling (daemon-wide and per session). Control may
// fill everything; normal calls leave the top eighth to control; telemetry
// stops at half the daemon-wide ceiling and a quarter of its session's, so a
// telemetry flood is shed long before anything else is refused.
var productionResponseShares = map[sessionrpc.Priority]budgetShare{
	sessionrpc.PriorityTelemetry: {1, 2},
	sessionrpc.PriorityNormal:    {7, 8},
	sessionrpc.PriorityControl:   {1, 1},
}

// subjectTelemetryShare narrows telemetry further within one session.
var subjectTelemetryShare = budgetShare{1, 4}

func newSessionResponseBudget() *sessionResponseBudget {
	b := newSessionResponseBudgetWithLimits(
		sessionrpc.MaxResponseFiles*daemonResponseSessions,
		sessionrpc.MaxResponseBytes*daemonResponseSessions,
		sessionrpc.MaxPendingReceipts*daemonResponseSessions,
	)
	b.subjectFiles = sessionrpc.MaxResponseFiles
	b.subjectBytes = sessionrpc.MaxResponseBytes
	b.subjectReceipts = sessionrpc.MaxPendingReceipts
	b.share = productionResponseShares
	return b
}

// newSessionResponseBudgetWithLimits is a flat budget: every priority and
// every subject may use the whole of the given ceilings.
func newSessionResponseBudgetWithLimits(maxFiles int, maxBytes int64, maxReceipts int) *sessionResponseBudget {
	return &sessionResponseBudget{
		maxFiles: maxFiles, maxBytes: maxBytes, maxReceipts: maxReceipts,
		subjectFiles: maxFiles, subjectBytes: maxBytes, subjectReceipts: maxReceipts,
		leases:   make(map[sessionResponseKey]*sessionResponseLease),
		subjects: make(map[string]*subjectResponseUsage),
		now:      time.Now,
	}
}

func (b *sessionResponseBudget) shareOf(p sessionrpc.Priority) budgetShare {
	if b.share == nil {
		return budgetShare{}
	}
	return b.share[p]
}

// admitsLocked reports whether a new reservation of class p fits both the
// daemon-wide and the subject's ceilings for that class.
func (b *sessionResponseBudget) admitsLocked(subject *subjectResponseUsage, p sessionrpc.Priority, bytes int64, receipt bool) bool {
	share := b.shareOf(p)
	if b.files >= share.of(b.maxFiles) || bytes > share.ofBytes(b.maxBytes)-b.bytes ||
		(receipt && b.receipts >= share.of(b.maxReceipts)) {
		return false
	}
	if subject == nil {
		subject = &subjectResponseUsage{}
	}
	if subject.files >= share.of(b.subjectFiles) || bytes > share.ofBytes(b.subjectBytes)-subject.bytes ||
		(receipt && subject.receipts >= share.of(b.subjectReceipts)) {
		return false
	}
	if p < sessionrpc.PriorityNormal && b.share != nil &&
		subject.byClass[p] >= subjectTelemetryShare.of(b.subjectFiles) {
		return false
	}
	return true
}

func (b *sessionResponseBudget) ReserveResponse(ctx context.Context, request sessionrpc.ResponseReservation) (sessionrpc.ResponseLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.SubjectID == "" || request.RequestID == "" || request.MaxBytes <= 0 {
		return nil, errSessionResponseCapacity
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	key := sessionResponseKey{subjectID: request.SubjectID, requestID: request.RequestID}
	if lease := b.leases[key]; lease != nil {
		if request.Existing {
			return lease, nil
		}
		return nil, errSessionResponseCapacity
	}
	subject := b.subjects[request.SubjectID]
	// Existing responses are already durable disk reality from a prior daemon
	// incarnation. Adopt and account them even when the retained set exceeds the
	// current limits; rejecting adoption would wedge startup before cleanup can
	// release anything. Only new amplification is subject to admission limits.
	if !request.Existing && !b.admitsLocked(subject, request.Priority, request.MaxBytes, request.ReceiptSlot) {
		b.countRejectionLocked(request)
		return nil, errSessionResponseCapacity
	}
	if subject == nil {
		subject = &subjectResponseUsage{byClass: make(map[sessionrpc.Priority]int)}
		b.subjects[request.SubjectID] = subject
	}

	lease := &sessionResponseLease{
		budget: b,
		key:    key,
		state: sessionResponseLeaseState{
			reservedBytes: request.MaxBytes,
			receipt:       request.ReceiptSlot,
			priority:      request.Priority,
		},
	}
	b.leases[key] = lease
	b.files++
	b.bytes += request.MaxBytes
	subject.files++
	subject.bytes += request.MaxBytes
	subject.byClass[request.Priority]++
	if request.ReceiptSlot {
		b.receipts++
		subject.receipts++
	}
	if b.stats.byClass == nil {
		b.stats.byClass = make(map[sessionrpc.Priority]int)
	}
	b.stats.byClass[request.Priority]++
	if !request.Existing {
		b.stats.admitted++
	}
	return lease, nil
}

func (b *sessionResponseBudget) countRejectionLocked(request sessionrpc.ResponseReservation) {
	c := &b.stats
	c.rejected++
	c.lastRejected = b.now()
	if c.rejectedByPriority == nil {
		c.rejectedByPriority = make(map[string]uint64)
		c.rejectedByVerb = make(map[string]uint64)
		c.rejectedBySubject = make(map[string]uint64)
	}
	c.rejectedByPriority[request.Priority.String()]++
	bump := func(m map[string]uint64, key string) {
		if key == "" {
			key = "(none)"
		}
		if _, ok := m[key]; !ok && len(m) >= budgetCounterKeys {
			key = "(other)"
		}
		m[key]++
	}
	verb := request.Verb
	if len(verb) > 64 {
		verb = verb[:64]
	}
	bump(c.rejectedByVerb, verb)
	bump(c.rejectedBySubject, request.SubjectID)
}

// subjectLocked returns a live lease's subject usage.
func (b *sessionResponseBudget) subjectLocked(id string) *subjectResponseUsage {
	subject := b.subjects[id]
	if subject == nil {
		// Unreachable: a live lease always has its subject. Keep accounting
		// non-negative rather than panic in a release path.
		subject = &subjectResponseUsage{byClass: make(map[sessionrpc.Priority]int)}
		b.subjects[id] = subject
	}
	return subject
}

func (l *sessionResponseLease) Commit(actualBytes int64, receiptPending bool) {
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	state := l.state
	if state.released || state.committed {
		return
	}
	subject := b.subjectLocked(l.key.subjectID)
	if actualBytes > 0 && actualBytes < state.reservedBytes {
		b.bytes -= state.reservedBytes - actualBytes
		subject.bytes -= state.reservedBytes - actualBytes
		state.reservedBytes = actualBytes
	}
	state.committed = true
	if state.receipt && !receiptPending {
		b.receipts--
		subject.receipts--
		state.receipt = false
	}
	l.state = state
}

func (l *sessionResponseLease) ReleaseReceipt() {
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	state := l.state
	if state.released || !state.receipt {
		return
	}
	b.receipts--
	b.subjectLocked(l.key.subjectID).receipts--
	state.receipt = false
	l.state = state
}

func (l *sessionResponseLease) Release() {
	b := l.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	state := l.state
	if state.released {
		return
	}
	state.released = true
	subject := b.subjectLocked(l.key.subjectID)
	b.files--
	b.bytes -= state.reservedBytes
	subject.files--
	subject.bytes -= state.reservedBytes
	subject.byClass[state.priority]--
	b.stats.byClass[state.priority]--
	if state.receipt {
		b.receipts--
		subject.receipts--
		state.receipt = false
	}
	if subject.files <= 0 && subject.receipts <= 0 {
		delete(b.subjects, l.key.subjectID)
	}
	delete(b.leases, l.key)
	l.state = state
}

// budgetTopSessions bounds the per-session rows a snapshot carries.
const budgetTopSessions = 10

// snapshot is the core.QueryRPCBudget read model.
func (b *sessionResponseBudget) snapshot() core.RPCBudget {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := core.RPCBudget{
		Files: b.files, MaxFiles: b.maxFiles, Bytes: b.bytes, MaxBytes: b.maxBytes,
		Receipts: b.receipts, MaxReceipts: b.maxReceipts,
		SessionMaxFiles: b.subjectFiles, Sessions: len(b.subjects),
		Admitted: b.stats.admitted, Rejected: b.stats.rejected,
		ByPriority:         map[string]int{},
		RejectedByPriority: copyCounts(b.stats.rejectedByPriority),
		RejectedByVerb:     copyCounts(b.stats.rejectedByVerb),
		RejectedBySession:  copyCounts(b.stats.rejectedBySubject),
	}
	for p, n := range b.stats.byClass {
		if n > 0 {
			out.ByPriority[p.String()] += n
		}
	}
	if !b.stats.lastRejected.IsZero() {
		out.LastRejectedAt = b.stats.lastRejected.UnixMilli()
	}
	for id, s := range b.subjects {
		out.Top = append(out.Top, core.RPCBudgetSession{ID: id, Files: s.files, Bytes: s.bytes})
	}
	sort.Slice(out.Top, func(i, j int) bool {
		if out.Top[i].Files != out.Top[j].Files {
			return out.Top[i].Files > out.Top[j].Files
		}
		return out.Top[i].ID < out.Top[j].ID
	})
	if len(out.Top) > budgetTopSessions {
		out.Top = out.Top[:budgetTopSessions]
	}
	return out
}

func copyCounts(m map[string]uint64) map[string]uint64 {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
