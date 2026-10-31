package prwatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// fakeGH serves `gh pr list` / `gh pr view` from in-memory PRs.
type fakeGH struct {
	mu        sync.Mutex
	prs       map[int]map[string]any // number -> gh JSON object
	err       error
	calls     []string
	beforeRun func()
}

func newFakeGH() *fakeGH { return &fakeGH{prs: map[int]map[string]any{}} }

func (f *fakeGH) set(n int, branch, state string, mut func(map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr := map[string]any{
		"number": n, "url": fmt.Sprintf("https://github.com/o/r/pull/%d", n), "state": state,
		"isDraft": false, "mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN",
		"headRefName": branch, "reviewDecision": "", "latestReviews": []any{},
		"statusCheckRollup": []any{check("build", "COMPLETED", "SUCCESS")},
	}
	if mut != nil {
		mut(pr)
	}
	f.prs[n] = pr
}

func (f *fakeGH) update(n int, mut func(map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mut(f.prs[n])
}

func check(name, status, conclusion string) map[string]any {
	return map[string]any{"__typename": "CheckRun", "name": name, "status": status, "conclusion": conclusion}
}

func (f *fakeGH) run(_ context.Context, args ...string) ([]byte, error) {
	if f.beforeRun != nil {
		before := f.beforeRun
		f.beforeRun = nil
		before()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	if f.err != nil {
		return nil, f.err
	}
	arg := func(name string) string {
		for i, a := range args {
			if a == name && i+1 < len(args) {
				return args[i+1]
			}
		}
		return ""
	}
	switch {
	case len(args) >= 2 && args[0] == "pr" && args[1] == "list":
		var out []map[string]any
		prefix := strings.TrimPrefix(arg("--search"), "head:")
		head := arg("--head")
		for n := 1; n <= 1000; n++ {
			pr, ok := f.prs[n]
			if !ok || pr["state"] != "OPEN" {
				continue
			}
			b := pr["headRefName"].(string)
			if (head != "" && b == head) || (head == "" && strings.HasPrefix(b, prefix)) {
				out = append(out, pr)
			}
		}
		return json.Marshal(out)
	case len(args) >= 3 && args[0] == "pr" && args[1] == "view":
		var n int
		fmt.Sscan(args[2], &n)
		pr, ok := f.prs[n]
		if !ok {
			return nil, errors.New("not found")
		}
		return json.Marshal(pr)
	}
	return nil, fmt.Errorf("unexpected gh %v", args)
}

func (f *fakeGH) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type harness struct {
	t         *testing.T
	gh        *fakeGH
	w         *Watcher
	saved     map[string]string
	events    []Change
	wakes     [][]string
	deliver   Delivery
	deliverFn func(string, []Wake) []Delivery
	logs      []string
	now       time.Time
	members   []Member
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, gh: newFakeGH(), saved: map[string]string{}, deliver: Delivered, now: time.Unix(1_700_000_000, 0)}
	h.members = []Member{
		{SessionID: "m1", Workgroup: "wg", Label: "alice", Branch: "amux/wg-m1", Repos: []string{"o/r"}},
		{SessionID: "m2", Workgroup: "wg", Branch: "amux/wg-m2", Repos: []string{"o/r"}},
	}
	h.w = h.build()
	return h
}

// build makes a watcher over the harness's persisted state (a "daemon restart"
// when called again).
func (h *harness) build() *Watcher {
	return New(Config{
		Run:     h.gh.run,
		Members: func() ([]Member, error) { return append([]Member(nil), h.members...), nil },
		Load: func() (map[string]string, error) {
			out := map[string]string{}
			for k, v := range h.saved {
				out[k] = v
			}
			return out, nil
		},
		Save: func(id, state string) error {
			if state == "" {
				delete(h.saved, id)
			} else {
				h.saved[id] = state
			}
			return nil
		},
		Emit: func(c Change) { h.events = append(h.events, c) },
		Deliver: func(_ context.Context, wg string, wakes []Wake) []Delivery {
			if h.deliverFn != nil {
				return h.deliverFn(wg, wakes)
			}
			results := make([]Delivery, len(wakes))
			for i := range results {
				results[i] = h.deliver
			}
			if h.deliver == Delivered {
				h.wakes = append(h.wakes, WakeLines(wakes))
			}
			return results
		},
		Logf: func(format string, args ...any) { h.logs = append(h.logs, fmt.Sprintf(format, args...)) },
		Now:  func() time.Time { return h.now },
	})
}

func (h *harness) poll() {
	h.now = h.now.Add(3 * time.Minute)
	h.w.Poll(context.Background())
}

func (h *harness) takeEvents() []Change {
	ev := h.events
	h.events = nil
	return ev
}

func (h *harness) takeWakes() [][]string {
	w := h.wakes
	h.wakes = nil
	return w
}

func TestFirstObservationTracksWithoutWaking(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1", "OPEN", nil)
	h.gh.set(8, "amux/wg-m1-b", "OPEN", nil) // suffixed branch: still m1's
	h.gh.set(9, "amux/wg-m10", "OPEN", nil)  // a different member's prefix: not m1's
	h.poll()
	ev := h.takeEvents()
	if len(ev) != 2 {
		t.Fatalf("events = %+v, want 2 tracking events for m1", ev)
	}
	for _, e := range ev {
		if e.SessionID != "m1" || len(e.Wake) != 0 || !strings.HasPrefix(e.Changes[0], "tracking PR") {
			t.Errorf("unexpected first event %+v", e)
		}
	}
	if w := h.takeWakes(); len(w) != 0 {
		t.Fatalf("first observation woke the coordinator: %v", w)
	}
	if got := h.w.PRs("m1"); len(got) != 2 || got[0].Number != 8 || got[1].Number != 7 {
		t.Fatalf("PRs(m1) = %+v", got)
	}
	if h.gh.callCount() != 1 {
		t.Fatalf("gh calls = %v, want one list per repo", h.gh.calls)
	}
}

func TestTransitionsWakeExactlyOnce(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1", "OPEN", func(pr map[string]any) { pr["isDraft"] = true })
	h.poll()
	h.takeEvents()

	steps := []struct {
		name   string
		mutate func(map[string]any)
		wake   string // substring of the single wake line; "" = no wake
	}{
		{"ci pending", func(pr map[string]any) {
			pr["statusCheckRollup"] = []any{check("build", "IN_PROGRESS", "")}
		}, ""},
		{"ci red", func(pr map[string]any) {
			pr["statusCheckRollup"] = []any{check("build", "COMPLETED", "FAILURE"), check("lint", "COMPLETED", "SUCCESS")}
		}, "CI failed (check build)"},
		{"ci green after red", func(pr map[string]any) {
			pr["statusCheckRollup"] = []any{check("build", "COMPLETED", "SUCCESS")}
		}, "CI green again"},
		{"draft to ready", func(pr map[string]any) { pr["isDraft"] = false }, "marked ready for review"},
		{"conflict", func(pr map[string]any) { pr["mergeable"] = "CONFLICTING"; pr["mergeStateStatus"] = "DIRTY" }, "has merge conflicts"},
		{"mergeability recomputing", func(pr map[string]any) { pr["mergeable"] = "UNKNOWN"; pr["mergeStateStatus"] = "UNKNOWN" }, ""},
		{"still conflicting", func(pr map[string]any) { pr["mergeable"] = "CONFLICTING"; pr["mergeStateStatus"] = "DIRTY" }, ""},
		{"changes requested", func(pr map[string]any) {
			pr["reviewDecision"] = "CHANGES_REQUESTED"
			pr["latestReviews"] = []any{map[string]any{"author": map[string]any{"login": "bob"}, "state": "CHANGES_REQUESTED", "submittedAt": "2026-10-31T10:00:00Z"}}
		}, "review submitted: changes requested by bob"},
		{"merged", func(pr map[string]any) { pr["state"] = "MERGED" }, "merged"},
	}
	for _, step := range steps {
		h.gh.update(7, step.mutate)
		h.poll()
		ev := h.takeEvents()
		wakes := h.takeWakes()
		if step.wake == "" {
			if len(wakes) != 0 {
				t.Fatalf("%s: unexpected wake %v", step.name, wakes)
			}
		} else {
			if len(wakes) != 1 || len(wakes[0]) != 1 || !strings.Contains(wakes[0][0], step.wake) {
				t.Fatalf("%s: wakes = %v, want one containing %q", step.name, wakes, step.wake)
			}
			if !strings.Contains(wakes[0][0], "member m1 (alice) PR o/r#7") {
				t.Errorf("%s: wake line %q lacks member/PR", step.name, wakes[0][0])
			}
		}
		if step.name != "mergeability recomputing" && step.name != "still conflicting" && len(ev) != 1 {
			t.Fatalf("%s: events = %+v, want 1", step.name, ev)
		}
		// Unchanged state: no event, no wake.
		h.poll()
		if ev, w := h.takeEvents(), h.takeWakes(); len(ev) != 0 || len(w) != 0 {
			t.Fatalf("%s: unchanged poll produced events %+v wakes %v", step.name, ev, w)
		}
	}
	if got := h.w.PRs("m1"); len(got) != 1 || got[0].State != "merged" {
		t.Fatalf("PRs(m1) after merge = %+v", got)
	}
	// A merged PR is no longer queried.
	before := h.gh.callCount()
	h.poll()
	if h.gh.callCount() != before+1 {
		t.Fatalf("merged PR still viewed: %v", h.gh.calls[before:])
	}
}

func TestStateSurvivesRestartWithoutRewaking(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1", "OPEN", nil)
	h.poll()
	h.gh.update(7, func(pr map[string]any) {
		pr["statusCheckRollup"] = []any{check("build", "COMPLETED", "FAILURE")}
	})
	h.poll()
	if w := h.takeWakes(); len(w) != 1 {
		t.Fatalf("wakes = %v, want 1", w)
	}
	h.takeEvents()
	h.w = h.build() // daemon restart: state comes back from the store
	h.poll()
	if ev, w := h.takeEvents(), h.takeWakes(); len(ev) != 0 || len(w) != 0 {
		t.Fatalf("restart re-reported: events %+v wakes %v", ev, w)
	}
	// RedSeen survived too: the next pass is "green again".
	h.gh.update(7, func(pr map[string]any) {
		pr["statusCheckRollup"] = []any{check("build", "COMPLETED", "SUCCESS")}
	})
	h.poll()
	if w := h.takeWakes(); len(w) != 1 || !strings.Contains(w[0][0], "CI green again") {
		t.Fatalf("wakes = %v, want CI green again", w)
	}
}

func TestDeferredWakeIsHeldAndDeliveredOnce(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1", "OPEN", nil)
	h.gh.set(8, "amux/wg-m2", "OPEN", nil)
	h.poll()
	h.deliver = Deferred
	h.gh.update(7, func(pr map[string]any) { pr["state"] = "CLOSED" })
	h.gh.update(8, func(pr map[string]any) { pr["mergeable"] = "CONFLICTING" })
	h.poll()
	if h.w.Pending("wg") != 2 {
		t.Fatalf("pending = %d, want 2", h.w.Pending("wg"))
	}
	h.poll() // nothing changed: nothing new queued
	if h.w.Pending("wg") != 2 {
		t.Fatalf("pending after unchanged poll = %d, want 2", h.w.Pending("wg"))
	}
	h.deliver = Delivered
	h.w.Flush(context.Background())
	w := h.takeWakes()
	if len(w) != 1 || len(w[0]) != 2 {
		t.Fatalf("wakes = %v, want one prompt with two lines", w)
	}
	h.w.Flush(context.Background())
	h.poll()
	if w := h.takeWakes(); len(w) != 0 {
		t.Fatalf("redelivered: %v", w)
	}
}

func TestDeferredWakeRetainsAuthorityIdentity(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1-topic", "OPEN", nil)
	h.poll()
	h.deliver = Deferred
	h.gh.update(7, func(pr map[string]any) { pr["state"] = "MERGED" })
	h.poll()

	var got []Wake
	h.deliverFn = func(workgroup string, wakes []Wake) []Delivery {
		if workgroup != "wg" {
			t.Fatalf("workgroup = %q, want wg", workgroup)
		}
		got = append([]Wake(nil), wakes...)
		return []Delivery{Deferred}
	}
	h.w.Flush(context.Background())
	if len(got) != 1 {
		t.Fatalf("wakes = %+v, want one", got)
	}
	wake := got[0]
	if wake.SessionID != "m1" || wake.Workgroup != "wg" || wake.Repo != "o/r" ||
		wake.Branch != "amux/wg-m1" || wake.PRBranch != "amux/wg-m1-topic" ||
		!strings.Contains(wake.Text, "member m1 (alice) PR o/r#7: merged") {
		t.Fatalf("wake lost authority identity: %+v", wake)
	}
}

func TestMixedDeliveryKeepsOnlyDeferredEntries(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1", "OPEN", nil)
	h.gh.set(8, "amux/wg-m2", "OPEN", nil)
	h.poll()
	h.deliver = Deferred
	h.gh.update(7, func(pr map[string]any) { pr["state"] = "CLOSED" })
	h.gh.update(8, func(pr map[string]any) { pr["mergeable"] = "CONFLICTING" })
	h.poll()

	h.deliverFn = func(_ string, wakes []Wake) []Delivery {
		results := make([]Delivery, len(wakes))
		for i, wake := range wakes {
			switch wake.SessionID {
			case "m1":
				results[i] = Dropped
			case "m2":
				results[i] = Deferred
			default:
				t.Fatalf("unexpected wake %+v", wake)
			}
		}
		return results
	}
	h.w.Flush(context.Background())
	if got := h.w.Pending("wg"); got != 1 {
		t.Fatalf("pending after mixed drop/defer = %d, want 1", got)
	}
	h.deliverFn = nil
	h.deliver = Delivered
	h.w.Flush(context.Background())
	wakes := h.takeWakes()
	if len(wakes) != 1 || len(wakes[0]) != 1 || !strings.Contains(wakes[0][0], "member m2 PR o/r#8") {
		t.Fatalf("delivered wakes = %v, want only m2", wakes)
	}
}

func TestIndeterminateDeliverySuppressesOnlyUncertainRetry(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1", "OPEN", nil)
	h.gh.set(8, "amux/wg-m2", "OPEN", nil)
	h.poll()
	h.deliver = Deferred
	h.gh.update(7, func(pr map[string]any) { pr["state"] = "CLOSED" })
	h.gh.update(8, func(pr map[string]any) { pr["mergeable"] = "CONFLICTING" })
	h.poll()

	calls := 0
	h.deliverFn = func(_ string, wakes []Wake) []Delivery {
		calls++
		results := make([]Delivery, len(wakes))
		for i, wake := range wakes {
			switch wake.SessionID {
			case "m1":
				results[i] = Indeterminate
			case "m2":
				results[i] = Deferred
			default:
				t.Fatalf("unexpected wake %+v", wake)
			}
		}
		return results
	}
	h.w.Flush(context.Background())
	if got := h.w.Pending("wg"); got != 1 {
		t.Fatalf("pending after indeterminate/deferred = %d, want only deferred entry", got)
	}
	if calls != 1 {
		t.Fatalf("delivery calls = %d, want 1", calls)
	}
	if wakes := h.takeWakes(); len(wakes) != 0 {
		t.Fatalf("indeterminate outcome was claimed as delivered: %v", wakes)
	}
	if got := strings.Join(h.logs, "\n"); !strings.Contains(got, "indeterminate") || !strings.Contains(got, "automatic retry suppressed") {
		t.Fatalf("missing reconciliation diagnostic: %q", got)
	}

	h.deliverFn = nil
	h.deliver = Delivered
	h.w.Flush(context.Background())
	wakes := h.takeWakes()
	if len(wakes) != 1 || len(wakes[0]) != 1 || !strings.Contains(wakes[0][0], "member m2 PR o/r#8") {
		t.Fatalf("delivered wakes = %v, want only independently deferred m2", wakes)
	}
	h.w.Flush(context.Background())
	if calls != 1 || h.w.Pending("wg") != 0 || len(h.takeWakes()) != 0 {
		t.Fatal("indeterminate notification was automatically retried")
	}
}

func TestMembershipChangeDuringObservationDropsOldRoute(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1", "OPEN", nil)
	h.poll()
	h.gh.update(7, func(pr map[string]any) {
		pr["statusCheckRollup"] = []any{check("build", "COMPLETED", "FAILURE")}
	})
	h.gh.beforeRun = func() { h.members[0].Workgroup = "other" }
	var observed []Wake
	h.deliverFn = func(workgroup string, wakes []Wake) []Delivery {
		observed = append([]Wake(nil), wakes...)
		results := make([]Delivery, len(wakes))
		for i, wake := range wakes {
			if workgroup == wake.Workgroup && wake.Workgroup == h.members[0].Workgroup {
				results[i] = Delivered
			} else {
				results[i] = Dropped
			}
		}
		return results
	}
	h.poll()
	if len(observed) != 1 || observed[0].Workgroup != "wg" || observed[0].SessionID != "m1" {
		t.Fatalf("observed wake = %+v, want original structured route", observed)
	}
	if wakes := h.takeWakes(); len(wakes) != 0 {
		t.Fatalf("old-route text delivered after move: %v", wakes)
	}
	if got := h.w.Pending("wg"); got != 0 {
		t.Fatalf("old-route wake remained pending after definitive drop: %d", got)
	}
}

func TestStaleWakeDropped(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1", "OPEN", nil)
	h.poll()
	h.deliver = Deferred
	h.gh.update(7, func(pr map[string]any) { pr["state"] = "MERGED" })
	h.poll()
	h.now = h.now.Add(MaxPendingAge + time.Minute)
	h.deliver = Delivered
	h.w.Flush(context.Background())
	if w := h.takeWakes(); len(w) != 0 || h.w.Pending("wg") != 0 {
		t.Fatalf("stale wake delivered: %v (pending %d)", w, h.w.Pending("wg"))
	}
}

func TestGHFailureIsQuiet(t *testing.T) {
	h := newHarness(t)
	h.gh.set(7, "amux/wg-m1", "OPEN", nil)
	h.poll()
	h.takeEvents()

	// gh missing: logged once, nothing changes, slow retry.
	h.gh.err = fmt.Errorf("exec: %w", exec.ErrNotFound)
	for i := 0; i < 3; i++ {
		h.poll()
	}
	if ev, w := h.takeEvents(), h.takeWakes(); len(ev) != 0 || len(w) != 0 {
		t.Fatalf("gh failure produced events %+v wakes %v", ev, w)
	}
	if len(h.logs) != 1 || !strings.Contains(h.logs[0], "gh unavailable") {
		t.Fatalf("logs = %q, want one setup message", h.logs)
	}
	if h.w.delay() != MaxBackoff {
		t.Fatalf("delay = %v, want %v", h.w.delay(), MaxBackoff)
	}
	if got := h.w.PRs("m1"); len(got) != 1 || got[0].State != "open" {
		t.Fatalf("PRs lost on failure: %+v", got)
	}

	// A transient failure backs off and never closes tracked PRs.
	h.gh.err = errors.New("HTTP 502: bad gateway")
	h.poll()
	h.poll()
	if d := h.w.delay(); d != 4*DefaultInterval {
		t.Fatalf("delay after 2 failures = %v, want %v", d, 4*DefaultInterval)
	}
	if ev := h.takeEvents(); len(ev) != 0 {
		t.Fatalf("transient failure produced events %+v", ev)
	}

	h.gh.err = nil
	h.poll()
	if h.w.delay() != DefaultInterval {
		t.Fatalf("delay after recovery = %v", h.w.delay())
	}
	if ev := h.takeEvents(); len(ev) != 0 {
		t.Fatalf("recovery produced events %+v", ev)
	}
}

func TestRepoSlug(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/Kchymet/agent-multiplexer.git": "Kchymet/agent-multiplexer",
		"git@github.com:o/r.git":                           "o/r",
		"ssh://git@github.com/o/r":                         "o/r",
		"o/r":                                              "o/r",
		"/home/me/src/r":                                   "",
		"https://gitlab.com/o/r.git":                       "",
	} {
		if got := RepoSlug(in); got != want {
			t.Errorf("RepoSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSummary(t *testing.T) {
	prs := []harnessproto.SessionPR{
		{Number: 9, State: "open", Draft: true, Checks: "fail", Mergeable: "conflicting"},
		{Number: 5, State: "merged"},
	}
	if got := Summary(prs); got != "PR #9 draft CI✗ conflict" {
		t.Fatalf("Summary = %q", got)
	}
	if got := Summary(prs[1:]); got != "PR #5 merged" {
		t.Fatalf("Summary = %q", got)
	}
	if got := Summary(nil); got != "" {
		t.Fatalf("Summary(nil) = %q", got)
	}
}
