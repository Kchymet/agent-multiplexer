// Package prwatch teaches amux which pull requests each workgroup member owns
// and wakes the member's coordinator when one of them changes state.
//
// Source (source.go) is the pull_request enrichment source the daemon's
// enrich.Registry runs: every interval (default 3 minutes) it lists the open
// PRs of each GitHub repository that active (non-archived) workgroup members
// work in — one `gh pr list --search head:<prefix>` per repository and branch
// prefix, so a tick costs one gh call per repo however many members and PRs
// there are — and attributes each PR to the member whose branch is its head
// (or that branch plus a "-suffix" or "/suffix": members may push several
// branches). A PR it was tracking that leaves the open list is read once with
// `gh pr view` to learn whether it merged or closed.
//
// Each observation is compared with the last one, which the registry persists
// per session, so transitions survive daemon restarts and are reported exactly
// once:
//
//   - every change is a `pr_state` event on the member's event stream;
//   - a meaningful one (CI turned red, CI green again after red, merge conflict,
//     review submitted, merged, closed, draft → ready) also queues a short
//     wake-up (QueuePR) on the daemon's one Watcher for the workgroup's
//     coordinator. Wake-ups are de-duplicated by the
//     PR's state hash, coalesced per coordinator, and handed to Deliver, which
//     decides whether the coordinator can take a prompt now (the daemon only
//     delivers to a live, idle coordinator) — undeliverable ones stay queued,
//     bounded in age.
//
// The same queue carries wake-ups the daemon observes itself (Notify): a
// member finished its turn, needs the user, or stopped. Those are coalesced in
// a short window per coordinator, the latest state per member winning, and the
// daemon Kicks the queue when a coordinator's own state moves, so a deferred
// wake-up is offered the moment the coordinator can take it.
//
// gh being absent or unauthenticated is the gh_unavailable setup condition
// (Source.Classify), which the registry logs once and retries slowly; other
// failures back the poll interval off exponentially, never before a rate limit
// resets. Nothing here ever fails the daemon.
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
	"time"

	"amux/internal/enrich"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// Defaults.
const (
	DefaultInterval = 3 * time.Minute
	// MaxBackoff caps the poll interval after repeated gh failures.
	MaxBackoff = enrich.DefaultMaxBackoff
	// FlushInterval is how often queued wake-ups are retried between polls, so
	// a coordinator that finishes its turn hears about a PR within seconds
	// rather than at the next poll.
	FlushInterval = 20 * time.Second
	// MaxPendingAge drops a wake-up its coordinator could not take for this
	// long: by then it is stale, and a coordinator that restarts is told to
	// re-check what it was waiting on anyway.
	MaxPendingAge = 2 * time.Hour
	// maxWakeLines bounds one coalesced wake-up prompt.
	maxWakeLines = 8
	// maxRecentKeys bounds the delivered-wake memory used for de-duplication.
	maxRecentKeys = 1024
	// NotifyWindow coalesces a burst of externally produced wake-ups (Notify):
	// the first one opens the window and everything that arrives within it is
	// offered to the coordinator together, as one prompt.
	NotifyWindow = 3 * time.Second
	// inboxSize bounds Notify's hand-off to Run; a full inbox drops the wake-up
	// (the coordinator's next wake or turn re-reads member state anyway).
	inboxSize = 256
)

// Wake kinds (Wake.Kind).
const (
	// KindPR is a PR transition the poller observed (the zero value).
	KindPR = ""
	// KindMember is a member's activity change the daemon observed and handed
	// in with Notify: it finished its turn, is waiting on the user, stopped.
	KindMember = "member"
	// KindItems announces upward-signal items (internal/items) a recipient has not
	// been told about. Workgroup and SessionID both name the recipient — a
	// coordinator, or the console — so a burst coalesces to one wake per
	// recipient; Deliver reads the items themselves at delivery time.
	KindItems = "items"
)

// Member is one active workgroup member whose PRs are watched.
type Member struct {
	SessionID string
	Workgroup string // the workgroup (= its coordinator's session) id
	Label     string // display label for wake-ups (name or id)
	Branch    string // the member's branch, e.g. amux/<broker>/<wg>-<member>
	Repos     []string
}

// Change is one observed PR transition for a member.
type Change struct {
	SessionID string
	Workgroup string
	Label     string // the member's display label (name or id)
	Branch    string // the member branch that owns the PR
	PR        harnessproto.SessionPR
	// Changes describes everything that moved since the last observation;
	// Wake is the subset meaningful enough to wake the coordinator.
	Changes []string
	Wake    []string
	// Notify is a user notification this transition warrants (a Notify*
	// value), or "": set only for the kinds Config.Notify enables. Each is
	// reported once per entry into the phase (transitions are reported once).
	Notify string
}

// User notification kinds (Change.Notify). Both are off unless enabled.
const (
	// NotifyReadyToMerge: a PR entered the merge phase with CI green and
	// nothing blocking.
	NotifyReadyToMerge = "ready_to_merge"
	// NotifyNeedsAttention: a PR entered the attention phase. It is the
	// coordinator's job, so it is never on by default.
	NotifyNeedsAttention = "needs_attention"
)

// Delivery is Deliver's verdict on a queued wake-up.
type Delivery int

const (
	// Delivered: the coordinator was prompted; the wake-ups are done.
	Delivered Delivery = iota
	// Deferred: the coordinator cannot take a prompt now; retry later.
	Deferred
	// Indeterminate: prompt admission may have taken effect, but no response
	// proved it. Suppress automatic retry without claiming delivery; a visible
	// diagnostic directs manual reconciliation.
	Indeterminate
	// Dropped: the coordinator is gone (archived, deleted); discard.
	Dropped
)

// Wake is one queued notification together with the authority-bearing identity
// observed when it was produced. Deliver must re-resolve this relationship at
// the exact prompt boundary; Text alone is never authority to disclose it.
type Wake struct {
	SessionID string
	Workgroup string
	Repo      string
	Branch    string // the member branch that owned the PR
	PRBranch  string // the observed PR head (Branch or a permitted suffix)
	Text      string
	// Kind is KindPR, KindMember or KindItems. A member wake carries the member State it
	// reports instead of Repo/Branch/PRBranch, and Deliver drops it when the
	// member has moved on by delivery time.
	Kind  string
	State string
}

// Config wires a Watcher (the coordinator wake-up queue) to its environment.
// Deliver is required.
type Config struct {
	// Deliver revalidates and prompts a workgroup's coordinator. Its result is
	// per wake so a mixed batch can drop stale relationships without withholding
	// independent eligible notifications. Indeterminate suppresses retry without
	// claiming delivery. A missing/misaligned result is treated as Deferred.
	Deliver func(ctx context.Context, workgroup string, wakes []Wake) []Delivery
	Logf    func(format string, args ...any)
	Now     func() time.Time
	// Window overrides NotifyWindow (tests).
	Window time.Duration
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
	wake Wake
	at   time.Time
}

// Watcher is the coordinator wake-up queue: PR transitions (QueuePR) and
// member activity (Notify), coalesced and delivered by Run. The queue is only
// touched on Run's goroutine; QueuePR, Notify and Kick are safe from any.
type Watcher struct {
	cfg Config

	pending map[string][]pendingWake // by workgroup
	recent  map[string]struct{}
	order   []string

	inbox   chan Wake
	prInbox chan Change
	kick    chan struct{}
}

// New builds a Watcher.
func New(cfg Config) *Watcher {
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Window <= 0 {
		cfg.Window = NotifyWindow
	}
	return &Watcher{
		cfg:     cfg,
		pending: map[string][]pendingWake{},
		recent:  map[string]struct{}{},
		inbox:   make(chan Wake, inboxSize),
		prInbox: make(chan Change, inboxSize),
		kick:    make(chan struct{}, 1),
	}
}

// Notify queues a wake-up produced outside the poller — a member's activity
// change — for its workgroup's coordinator. It never blocks and is safe from
// any goroutine; Run enqueues it and offers the batch after NotifyWindow. A
// newer wake for the same member replaces an undelivered older one (the latest
// state wins), so a burst collapses to one line per member.
func (w *Watcher) Notify(wake Wake) {
	if wake.Workgroup == "" || wake.SessionID == "" {
		return
	}
	select {
	case w.inbox <- wake:
	default:
		w.cfg.Logf("prwatch: wake inbox full; dropping wake-up for %s member %s", wake.Workgroup, wake.SessionID)
	}
}

// Kick asks Run to offer queued wake-ups now instead of at the next
// FlushInterval — the daemon calls it when a coordinator's own state moves
// (it finished a turn, its goal changed), which is exactly when a deferred
// wake-up may become deliverable. Never blocks; kicks coalesce.
func (w *Watcher) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// enqueueNotified queues a Notify wake-up, replacing an undelivered one for
// the same member.
func (w *Watcher) enqueueNotified(wake Wake, now time.Time) {
	key := wake.Kind + "|" + wake.SessionID
	queue := w.pending[wake.Workgroup]
	for i, p := range queue {
		if p.key == key {
			queue[i] = pendingWake{key: key, wake: wake, at: now}
			return
		}
	}
	w.pending[wake.Workgroup] = append(queue, pendingWake{key: key, wake: wake, at: now})
}

// QueuePR hands a PR transition that warrants a wake-up (Change.Wake) to Run,
// which queues it for the member's coordinator, de-duplicated by the PR's
// state hash, and offers it at once. Never blocks; safe from any goroutine.
func (w *Watcher) QueuePR(c Change) {
	if c.Workgroup == "" || len(c.Wake) == 0 {
		return
	}
	select {
	case w.prInbox <- c:
	default:
		w.cfg.Logf("prwatch: wake inbox full; dropping PR wake-up for %s member %s", c.Workgroup, c.SessionID)
	}
}

// drainPR queues every PR wake-up handed in so far and offers the queue
// (Run does this as they arrive; tests call it directly).
func (w *Watcher) drainPR(ctx context.Context) {
	for {
		select {
		case c := <-w.prInbox:
			w.enqueue(c, w.cfg.Now())
		default:
			w.Flush(ctx)
			return
		}
	}
}

// Run serves QueuePR, Notify and Kick, and retries queued wake-ups every
// FlushInterval until ctx ends. The queue is only touched here.
func (w *Watcher) Run(ctx context.Context) {
	flush := time.NewTicker(FlushInterval)
	defer flush.Stop()
	window := time.NewTimer(time.Hour)
	window.Stop()
	defer window.Stop()
	windowOpen := false
	for {
		select {
		case <-ctx.Done():
			return
		case c := <-w.prInbox:
			w.enqueue(c, w.cfg.Now())
			w.drainPR(ctx)
		case wake := <-w.inbox:
			w.enqueueNotified(wake, w.cfg.Now())
			if !windowOpen {
				windowOpen = true
				window.Reset(w.cfg.Window)
			}
		case <-window.C:
			windowOpen = false
			w.Flush(ctx)
		case <-w.kick:
			if !windowOpen {
				w.Flush(ctx)
			}
		case <-flush.C:
			w.Flush(ctx)
		}
	}
}

// Owns reports whether a PR head branch belongs to a member branch: the branch
// itself or the branch plus a "-suffix" or "/suffix" (enrich.Owns, which is
// ghpr's rule, so the archive guard and the coordinator agree).
func Owns(branch, head string) bool { return enrich.Owns(branch, head) }

func prKey(pr harnessproto.SessionPR) string { return fmt.Sprintf("%s#%d", pr.Repo, pr.Number) }

// readyToMerge: in the merge phase with CI settled (green or none) and
// nothing blocking.
func readyToMerge(pr harnessproto.SessionPR) bool {
	phase, _ := enrich.Phase(pr)
	return phase == harnessproto.PRPhaseMerge && enrich.CI(pr).Status != harnessproto.CIPending &&
		!enrich.Mergeable(pr).BlockedByChecks
}

// notifyKind is the user notification a transition from prev to cur warrants
// ("" for none): a PR becoming ready to merge, or entering the attention
// phase. A PR seen for the first time never notifies (no page on startup).
func notifyKind(prev, cur harnessproto.SessionPR) string {
	if readyToMerge(cur) && !readyToMerge(prev) {
		return NotifyReadyToMerge
	}
	was, _ := enrich.Phase(prev)
	now, _ := enrich.Phase(cur)
	if now == harnessproto.PRPhaseAttention && was != harnessproto.PRPhaseAttention {
		return NotifyNeedsAttention
	}
	return ""
}

// Hash is a PR summary's state hash: every field except when amux and GitHub
// last saw it change (Updated, GitHubUpdatedAt), which move without a state
// change.
func Hash(pr harnessproto.SessionPR) string {
	pr.Updated = 0
	pr.GitHubUpdatedAt = ""
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
func (w *Watcher) enqueue(c Change, now time.Time) {
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
	label := c.Label
	if label == "" {
		label = c.SessionID
	}
	if label != c.SessionID {
		label = c.SessionID + " (" + label + ")"
	}
	line := fmt.Sprintf("member %s PR %s: %s — %s", label, prKey(c.PR), strings.Join(c.Wake, "; "), c.PR.URL)
	w.pending[c.Workgroup] = append(w.pending[c.Workgroup], pendingWake{key: key, wake: Wake{
		SessionID: c.SessionID, Workgroup: c.Workgroup, Repo: c.PR.Repo,
		Branch: c.Branch, PRBranch: c.PR.Branch, Text: line,
	}, at: now})
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
				w.cfg.Logf("prwatch: dropping stale wake-up for %s member %s repo %s", wg, p.wake.SessionID, p.wake.Repo)
			}
		}
		if len(queue) == 0 {
			delete(w.pending, wg)
			continue
		}
		w.pending[wg] = queue
		wakes := make([]Wake, len(queue))
		for i, p := range queue {
			wakes[i] = p.wake
		}
		results := w.cfg.Deliver(ctx, wg, wakes)
		remaining := queue[:0]
		for i, p := range queue {
			result := Deferred
			if i < len(results) {
				result = results[i]
			}
			switch result {
			case Delivered:
				w.remember(p.key)
			case Indeterminate:
				w.remember(p.key)
				w.cfg.Logf("prwatch: wake-up outcome indeterminate for %s member %s repo %s; automatic retry suppressed", wg, p.wake.SessionID, p.wake.Repo)
			case Dropped:
				if p.wake.Kind == KindMember {
					w.cfg.Logf("prwatch: dropped member wake-up for %s member %s (moved on or out of scope)", wg, p.wake.SessionID)
				}
			default:
				remaining = append(remaining, p)
			}
		}
		if len(remaining) == 0 {
			delete(w.pending, wg)
		} else {
			w.pending[wg] = remaining
		}
	}
}

// WakeLines renders a bounded batch after delivery has removed unauthorized
// entries. It never receives stale text from the daemon's authorization seam.
func WakeLines(wakes []Wake) []string {
	lines := make([]string, 0, min(len(wakes), maxWakeLines+1))
	for i, wake := range wakes {
		if i == maxWakeLines {
			lines = append(lines, fmt.Sprintf("(+%d more updates)", len(wakes)-maxWakeLines))
			break
		}
		lines = append(lines, wake.Text)
	}
	return lines
}

// Pending reports how many wake-ups are queued for a workgroup (tests,
// diagnostics).
func (w *Watcher) Pending(workgroup string) int { return len(w.pending[workgroup]) }
