// Package prwatch teaches amux which pull requests each workgroup member owns
// and wakes the member's coordinator when one of them changes state.
//
// The daemon runs one Watcher. Every Interval (default 3 minutes) it lists the
// open PRs of each GitHub repository that active (non-archived) workgroup
// members work in — one `gh pr list --search head:<prefix>` per repository and
// branch prefix, so a tick costs one gh call per repo however many members and
// PRs there are — and attributes each PR to the member whose branch is its head
// (or that branch plus a "-suffix": members may push several branches). A PR it
// was tracking that leaves the open list is read once with `gh pr view` to learn
// whether it merged or closed.
//
// Each observation is compared with the last one, which the store persists per
// session, so transitions survive daemon restarts and are reported exactly
// once:
//
//   - every change is a `pr_state` event on the member's event stream;
//   - a meaningful one (CI turned red, CI green again after red, merge conflict,
//     review submitted, merged, closed, draft → ready) also queues a short
//     wake-up for the workgroup's coordinator. Wake-ups are de-duplicated by the
//     PR's state hash, coalesced per coordinator, and handed to Deliver, which
//     decides whether the coordinator can take a prompt now (the daemon only
//     delivers to a live, idle coordinator) — undeliverable ones stay queued,
//     bounded in age.
//
// gh being absent or unauthenticated is a setup condition, logged once; other
// failures back the poll interval off exponentially. Nothing here ever fails
// the daemon.
package prwatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// Defaults.
const (
	DefaultInterval = 3 * time.Minute
	// MaxBackoff caps the poll interval after repeated gh failures.
	MaxBackoff = 30 * time.Minute
	// FlushInterval is how often queued wake-ups are retried between polls, so
	// a coordinator that finishes its turn hears about a PR within seconds
	// rather than at the next poll.
	FlushInterval = 20 * time.Second
	// MaxPendingAge drops a wake-up its coordinator could not take for this
	// long: by then it is stale, and a coordinator that restarts is told to
	// re-check what it was waiting on anyway.
	MaxPendingAge = 2 * time.Hour
	// maxTerminal bounds how many merged/closed PRs are remembered per session.
	maxTerminal = 5
	// maxWakeLines bounds one coalesced wake-up prompt.
	maxWakeLines = 8
	// maxRecentKeys bounds the delivered-wake memory used for de-duplication.
	maxRecentKeys = 1024
)

// Member is one active workgroup member whose PRs are watched.
type Member struct {
	SessionID string
	Workgroup string // the workgroup (= its coordinator's session) id
	Label     string // display label for wake-ups (name or id)
	Branch    string // the member's branch, e.g. amux/<wg>-<member>
	Repos     []string
}

// Change is one observed PR transition for a member.
type Change struct {
	SessionID string
	Workgroup string
	PR        harnessproto.SessionPR
	// Changes describes everything that moved since the last observation;
	// Wake is the subset meaningful enough to wake the coordinator.
	Changes []string
	Wake    []string
}

// Delivery is Deliver's verdict on a queued wake-up.
type Delivery int

const (
	// Delivered: the coordinator was prompted; the wake-ups are done.
	Delivered Delivery = iota
	// Deferred: the coordinator cannot take a prompt now; retry later.
	Deferred
	// Dropped: the coordinator is gone (archived, deleted); discard.
	Dropped
)

// Config wires a Watcher to its environment. Every function is required
// except Logf, Now and Interval.
type Config struct {
	Run     Runner
	Members func() ([]Member, error)
	// Load returns the persisted state documents by session id; Save stores
	// one ("" deletes it).
	Load func() (map[string]string, error)
	Save func(sessionID, state string) error
	// Emit records a change on the member's event stream.
	Emit func(Change)
	// Deliver prompts a workgroup's coordinator with wake-up lines.
	Deliver  func(ctx context.Context, workgroup string, lines []string) Delivery
	Logf     func(format string, args ...any)
	Now      func() time.Time
	Interval time.Duration
}

// record is the persisted per-PR state: the published summary plus what
// transition detection needs that the summary does not show.
type record struct {
	PR harnessproto.SessionPR `json:"pr"`
	// RedSeen: CI failed since it last passed, so the next pass is "green
	// again" (worth a wake) rather than routine.
	RedSeen bool `json:"redSeen,omitempty"`
}

type pendingWake struct {
	key  string
	line string
	at   time.Time
}

// Watcher polls member PRs. Poll and Flush must not run concurrently (Run
// serializes them); PRs may be called from any goroutine.
type Watcher struct {
	cfg Config

	mu    sync.RWMutex
	state map[string][]record // by session id; guarded by mu for PRs()

	loaded  bool
	pending map[string][]pendingWake // by workgroup
	recent  map[string]struct{}
	order   []string

	failures   int
	setupError bool
}

// New builds a Watcher.
func New(cfg Config) *Watcher {
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	return &Watcher{
		cfg:     cfg,
		state:   map[string][]record{},
		pending: map[string][]pendingWake{},
		recent:  map[string]struct{}{},
	}
}

// PRs returns the last-known PRs a session owns, newest first.
func (w *Watcher) PRs(sessionID string) []harnessproto.SessionPR {
	w.mu.RLock()
	defer w.mu.RUnlock()
	recs := w.state[sessionID]
	if len(recs) == 0 {
		return nil
	}
	out := make([]harnessproto.SessionPR, len(recs))
	for i, r := range recs {
		out[i] = r.PR
	}
	return out
}

// Run polls every interval (backed off on failures) and retries queued
// wake-ups every FlushInterval until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	flush := time.NewTicker(FlushInterval)
	defer flush.Stop()
	next := time.NewTimer(0)
	defer next.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-flush.C:
			w.Flush(ctx)
		case <-next.C:
			w.Poll(ctx)
			next.Reset(w.delay())
		}
	}
}

// delay is the wait before the next poll: the interval, doubled per
// consecutive failure up to MaxBackoff; a setup error waits MaxBackoff.
func (w *Watcher) delay() time.Duration {
	if w.setupError {
		return MaxBackoff
	}
	d := w.cfg.Interval
	for i := 0; i < w.failures && d < MaxBackoff; i++ {
		d *= 2
	}
	if d > MaxBackoff {
		d = MaxBackoff
	}
	return d
}

func (w *Watcher) load() {
	if w.loaded {
		return
	}
	docs, err := w.cfg.Load()
	if err != nil {
		w.cfg.Logf("prwatch: load state: %v", err)
		return
	}
	w.loaded = true
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, doc := range docs {
		var recs []record
		if json.Unmarshal([]byte(doc), &recs) == nil {
			w.state[id] = recs
		}
	}
}

// Poll runs one observation pass and then tries to deliver queued wake-ups.
func (w *Watcher) Poll(ctx context.Context) {
	w.load()
	if !w.loaded {
		return
	}
	members, err := w.cfg.Members()
	if err != nil {
		w.cfg.Logf("prwatch: list members: %v", err)
		return
	}
	// One query per (repo, branch prefix): members of every workgroup in a repo
	// share the "amux/" prefix, so this is normally one gh call per repo.
	type query struct{ repo, prefix, head string }
	queries := map[query]bool{}
	for _, m := range members {
		if m.Branch == "" {
			continue
		}
		for _, repo := range m.Repos {
			if i := strings.Index(m.Branch, "/"); i > 0 {
				queries[query{repo: repo, prefix: m.Branch[:i+1]}] = true
			} else {
				queries[query{repo: repo, head: m.Branch}] = true
			}
		}
	}
	keys := make([]query, 0, len(queries))
	for q := range queries {
		keys = append(keys, q)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].repo != keys[j].repo {
			return keys[i].repo < keys[j].repo
		}
		return keys[i].prefix+keys[i].head < keys[j].prefix+keys[j].head
	})
	open := map[string][]harnessproto.SessionPR{} // by repo
	failed := map[string]bool{}                   // repos whose listing failed this tick
	var firstErr error
	for _, q := range keys {
		if failed[q.repo] {
			continue
		}
		var prs []harnessproto.SessionPR
		var err error
		if q.head != "" {
			prs, err = listHead(ctx, w.cfg.Run, q.repo, q.head)
		} else {
			prs, err = listOpen(ctx, w.cfg.Run, q.repo, q.prefix)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			failed[q.repo] = true
			if firstErr == nil {
				firstErr = err
			}
			if classify(err) == errSetup {
				break // gh itself is unusable: every other call would fail the same way
			}
			continue
		}
		open[q.repo] = append(open[q.repo], prs...)
	}
	if firstErr != nil && w.noteFailure(firstErr) {
		return
	}
	if firstErr == nil {
		w.noteSuccess()
	}

	now := w.cfg.Now()
	for _, m := range members {
		w.observe(ctx, m, open, failed, now)
	}
	w.Flush(ctx)
}

// noteFailure records a failed tick; it reports whether the whole tick should
// be abandoned (gh unusable).
func (w *Watcher) noteFailure(err error) bool {
	switch classify(err) {
	case errSetup:
		if !w.setupError {
			w.cfg.Logf("prwatch: gh unavailable, PR watching paused (retrying every %v): %v", MaxBackoff, err)
		}
		w.setupError = true
		return true
	}
	// gh ran and reached GitHub, so it is set up; this failure is transient.
	w.setupError = false
	switch classify(err) {
	case errRateLimit:
		w.failures++
		w.cfg.Logf("prwatch: GitHub rate limit, backing off to %v: %v", w.delay(), err)
	default:
		w.failures++
		if w.failures == 1 || w.failures%5 == 0 {
			w.cfg.Logf("prwatch: gh failed (%d in a row, next poll in %v): %v", w.failures, w.delay(), err)
		}
	}
	return false
}

func (w *Watcher) noteSuccess() {
	if w.setupError {
		w.cfg.Logf("prwatch: gh available again, PR watching resumed")
	}
	w.setupError = false
	w.failures = 0
}

// owns reports whether a PR head branch belongs to a member branch: the branch
// itself or the branch plus a "-suffix".
func owns(branch, head string) bool {
	return head == branch || strings.HasPrefix(head, branch+"-")
}

func prKey(pr harnessproto.SessionPR) string { return fmt.Sprintf("%s#%d", pr.Repo, pr.Number) }

// observe folds this tick's observation into one member's records.
func (w *Watcher) observe(ctx context.Context, m Member, open map[string][]harnessproto.SessionPR, failed map[string]bool, now time.Time) {
	w.mu.RLock()
	prev := w.state[m.SessionID]
	w.mu.RUnlock()
	prevBy := map[string]record{}
	for _, r := range prev {
		prevBy[prKey(r.PR)] = r
	}
	inRepo := map[string]bool{}
	for _, r := range m.Repos {
		inRepo[r] = true
	}

	var next []record
	var changes []Change
	seen := map[string]bool{}
	add := func(prior *record, cur harnessproto.SessionPR) {
		rec, ch, wake := transition(prior, cur, now)
		next = append(next, rec)
		seen[prKey(cur)] = true
		if len(ch) > 0 {
			changes = append(changes, Change{SessionID: m.SessionID, Workgroup: m.Workgroup, PR: rec.PR, Changes: ch, Wake: wake})
		}
	}
	for _, repo := range m.Repos {
		for _, pr := range open[repo] {
			if !owns(m.Branch, pr.Branch) || seen[prKey(pr)] {
				continue
			}
			if p, ok := prevBy[prKey(pr)]; ok {
				add(&p, pr)
			} else {
				add(nil, pr)
			}
		}
	}
	for _, r := range prev {
		k := prKey(r.PR)
		if seen[k] {
			continue
		}
		seen[k] = true
		// A tracked PR in a repo the member no longer works in, or whose listing
		// failed this tick, or one already merged/closed: keep it as it was.
		if r.PR.State != "open" || !inRepo[r.PR.Repo] || failed[r.PR.Repo] {
			next = append(next, r)
			continue
		}
		// It left the open list: learn how.
		cur, err := view(ctx, w.cfg.Run, r.PR.Repo, r.PR.Number)
		if err != nil || cur.State == "" {
			next = append(next, r)
			continue
		}
		r := r
		add(&r, cur)
	}
	next = trimTerminal(next)

	if !sameRecords(prev, next) {
		w.mu.Lock()
		if len(next) == 0 {
			delete(w.state, m.SessionID)
		} else {
			w.state[m.SessionID] = next
		}
		w.mu.Unlock()
		doc := ""
		if len(next) > 0 {
			b, _ := json.Marshal(next)
			doc = string(b)
		}
		if err := w.cfg.Save(m.SessionID, doc); err != nil {
			w.cfg.Logf("prwatch: save %s: %v", m.SessionID, err)
		}
	}
	for _, c := range changes {
		w.cfg.Emit(c)
		if len(c.Wake) > 0 {
			w.enqueue(m, c, now)
		}
	}
}

// trimTerminal orders records newest first and keeps every open PR but only
// the most recent maxTerminal merged/closed ones.
func trimTerminal(recs []record) []record {
	sort.SliceStable(recs, func(i, j int) bool {
		if recs[i].PR.Repo != recs[j].PR.Repo {
			return recs[i].PR.Repo < recs[j].PR.Repo
		}
		return recs[i].PR.Number > recs[j].PR.Number
	})
	out := recs[:0]
	terminal := 0
	for _, r := range recs {
		if r.PR.State != "open" {
			if terminal >= maxTerminal {
				continue
			}
			terminal++
		}
		out = append(out, r)
	}
	return out
}

func sameRecords(a, b []record) bool {
	if len(a) != len(b) {
		return false
	}
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// Hash is a PR summary's state hash: every field except Updated.
func Hash(pr harnessproto.SessionPR) string {
	pr.Updated = 0
	b, _ := json.Marshal(pr)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// transition compares a fresh observation with the prior record (nil for a PR
// seen for the first time) and returns the next record, every change, and the
// changes that warrant waking the coordinator.
func transition(prior *record, cur harnessproto.SessionPR, now time.Time) (next record, changes, wake []string) {
	if prior == nil {
		cur.Updated = now.UnixMilli()
		next = record{PR: cur, RedSeen: cur.Checks == "fail"}
		changes = []string{"tracking PR (" + describe(cur) + ")"}
		return next, changes, nil
	}
	prev := prior.PR
	// GitHub recomputes mergeability lazily and reports UNKNOWN meanwhile; keep
	// the last known answer so a conflict that persists is not reported twice.
	if cur.Mergeable == "unknown" && prev.Mergeable != "" {
		cur.Mergeable, cur.MergeState = prev.Mergeable, prev.MergeState
	}
	next = record{PR: cur, RedSeen: prior.RedSeen}
	if Hash(cur) == Hash(prev) {
		next.PR.Updated = prev.Updated
		return next, nil, nil
	}
	next.PR.Updated = now.UnixMilli()
	note := func(w bool, s string) {
		changes = append(changes, s)
		if w {
			wake = append(wake, s)
		}
	}
	if cur.State != prev.State {
		switch cur.State {
		case "merged":
			note(true, "merged")
		case "closed":
			note(true, "closed without merging")
		case "open":
			note(false, "reopened")
		default:
			note(false, "state "+cur.State)
		}
	}
	if cur.State == "open" {
		if prev.Draft && !cur.Draft {
			note(true, "marked ready for review")
		} else if !prev.Draft && cur.Draft {
			note(false, "converted to draft")
		}
		if cur.Checks != prev.Checks {
			switch cur.Checks {
			case "fail":
				note(prev.Checks != "fail", "CI failed ("+checkList(cur.FailedChecks)+")")
				next.RedSeen = true
			case "pass":
				if prior.RedSeen {
					note(true, "CI green again")
				} else {
					note(false, "CI passed")
				}
				next.RedSeen = false
			case "pending":
				note(false, "CI running")
			default:
				note(false, "no CI checks")
			}
		} else if cur.Checks == "fail" && strings.Join(cur.FailedChecks, ",") != strings.Join(prev.FailedChecks, ",") {
			note(false, "CI still failing ("+checkList(cur.FailedChecks)+")")
		}
		if cur.Mergeable != prev.Mergeable {
			switch cur.Mergeable {
			case "conflicting":
				note(true, "has merge conflicts")
			case "mergeable":
				if prev.Mergeable == "conflicting" {
					note(false, "merge conflicts resolved")
				}
			}
		}
	}
	if r := cur.LastReview; r != nil && (prev.LastReview == nil || *r != *prev.LastReview) {
		who := ""
		if r.Author != "" {
			who = " by " + r.Author
		}
		note(true, "review submitted: "+strings.ReplaceAll(r.State, "_", " ")+who)
	}
	if cur.ReviewDecision != prev.ReviewDecision && (cur.LastReview == nil || (prev.LastReview != nil && *cur.LastReview == *prev.LastReview)) {
		note(false, "review decision "+orNone(cur.ReviewDecision))
	}
	if len(changes) == 0 {
		changes = []string{"updated (" + describe(cur) + ")"}
	}
	return next, changes, wake
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return strings.ReplaceAll(s, "_", " ")
}

func checkList(names []string) string {
	if len(names) == 0 {
		return "check failed"
	}
	if len(names) > 3 {
		return fmt.Sprintf("checks %s, +%d more", strings.Join(names[:3], ", "), len(names)-3)
	}
	if len(names) == 1 {
		return "check " + names[0]
	}
	return "checks " + strings.Join(names, ", ")
}

// describe is a compact one-line state of a PR.
func describe(pr harnessproto.SessionPR) string {
	parts := []string{pr.State}
	if pr.Draft {
		parts = append(parts, "draft")
	}
	if pr.Checks != "" {
		parts = append(parts, "CI "+pr.Checks)
	}
	if pr.Mergeable == "conflicting" {
		parts = append(parts, "conflicting")
	}
	if pr.ReviewDecision != "" {
		parts = append(parts, strings.ReplaceAll(pr.ReviewDecision, "_", " "))
	}
	return strings.Join(parts, ", ")
}

// Summary is a short rail/status suffix for a session's PRs ("" when none):
// each open PR as #N plus its notable state, merged/closed ones only when no
// PR is open.
func Summary(prs []harnessproto.SessionPR) string {
	var open, done []string
	for _, pr := range prs {
		if pr.State == "open" {
			var flags []string
			if pr.Draft {
				flags = append(flags, "draft")
			}
			switch pr.Checks {
			case "fail":
				flags = append(flags, "CI✗")
			case "pending":
				flags = append(flags, "CI…")
			case "pass":
				flags = append(flags, "CI✓")
			}
			if pr.Mergeable == "conflicting" {
				flags = append(flags, "conflict")
			}
			switch pr.ReviewDecision {
			case "approved":
				flags = append(flags, "approved")
			case "changes_requested":
				flags = append(flags, "changes requested")
			}
			s := fmt.Sprintf("#%d", pr.Number)
			if len(flags) > 0 {
				s += " " + strings.Join(flags, " ")
			}
			open = append(open, s)
		} else if len(done) == 0 {
			done = append(done, fmt.Sprintf("#%d %s", pr.Number, pr.State))
		}
	}
	if len(open) > 0 {
		return "PR " + strings.Join(open, ", ")
	}
	if len(done) > 0 {
		return "PR " + done[0]
	}
	return ""
}

// enqueue queues a wake-up for the member's coordinator, de-duplicated by the
// PR's state hash: the same transition is never queued or delivered twice.
func (w *Watcher) enqueue(m Member, c Change, now time.Time) {
	if c.Workgroup == "" {
		return
	}
	key := c.SessionID + "|" + prKey(c.PR) + "|" + Hash(c.PR) + "|" + strings.Join(c.Wake, ";")
	if _, done := w.recent[key]; done {
		return
	}
	for _, p := range w.pending[c.Workgroup] {
		if p.key == key {
			return
		}
	}
	label := m.Label
	if label == "" {
		label = m.SessionID
	}
	if label != m.SessionID {
		label = m.SessionID + " (" + label + ")"
	}
	line := fmt.Sprintf("member %s PR %s: %s — %s", label, prKey(c.PR), strings.Join(c.Wake, "; "), c.PR.URL)
	w.pending[c.Workgroup] = append(w.pending[c.Workgroup], pendingWake{key: key, line: line, at: now})
}

func (w *Watcher) remember(key string) {
	if _, ok := w.recent[key]; ok {
		return
	}
	w.recent[key] = struct{}{}
	w.order = append(w.order, key)
	if len(w.order) > maxRecentKeys {
		delete(w.recent, w.order[0])
		w.order = w.order[1:]
	}
}

// Flush offers each coordinator its queued wake-ups as one prompt.
func (w *Watcher) Flush(ctx context.Context) {
	if len(w.pending) == 0 {
		return
	}
	now := w.cfg.Now()
	groups := make([]string, 0, len(w.pending))
	for wg := range w.pending {
		groups = append(groups, wg)
	}
	sort.Strings(groups)
	for _, wg := range groups {
		queue := w.pending[wg][:0]
		for _, p := range w.pending[wg] {
			if now.Sub(p.at) <= MaxPendingAge {
				queue = append(queue, p)
			} else {
				w.cfg.Logf("prwatch: dropping stale wake-up for %s: %s", wg, p.line)
			}
		}
		if len(queue) == 0 {
			delete(w.pending, wg)
			continue
		}
		w.pending[wg] = queue
		lines := make([]string, 0, len(queue))
		for i, p := range queue {
			if i == maxWakeLines {
				lines = append(lines, fmt.Sprintf("(+%d more PR updates)", len(queue)-maxWakeLines))
				break
			}
			lines = append(lines, p.line)
		}
		switch w.cfg.Deliver(ctx, wg, lines) {
		case Delivered:
			for _, p := range queue {
				w.remember(p.key)
			}
			delete(w.pending, wg)
		case Dropped:
			delete(w.pending, wg)
		}
	}
}

// Pending reports how many wake-ups are queued for a workgroup (tests,
// diagnostics).
func (w *Watcher) Pending(workgroup string) int { return len(w.pending[workgroup]) }
