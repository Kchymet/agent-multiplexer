package enrich

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

var now = time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC)

func open(n int, mut ...func(*hp.SessionPR)) hp.SessionPR {
	pr := hp.SessionPR{Repo: "o/r", Number: n, URL: "https://github.com/o/r/pull/1", Branch: "amux/wg-m1", State: "open",
		Mergeable: "mergeable", MergeState: "clean", Checks: "pass"}
	for _, m := range mut {
		m(&pr)
	}
	return pr
}

func red(pr *hp.SessionPR) {
	pr.Checks, pr.FailedChecks, pr.MergeState = "fail", []string{"test", "lint"}, "unstable"
}
func pending(pr *hp.SessionPR)     { pr.Checks, pr.MergeState = "pending", "blocked" }
func approved(pr *hp.SessionPR)    { pr.ReviewDecision = "approved" }
func needsReview(pr *hp.SessionPR) { pr.ReviewDecision = "review_required" }
func changes(pr *hp.SessionPR)     { pr.ReviewDecision = "changes_requested" }
func draft(pr *hp.SessionPR)       { pr.Draft = true }
func conflict(pr *hp.SessionPR)    { pr.Mergeable, pr.MergeState = "conflicting", "dirty" }
func behind(pr *hp.SessionPR)      { pr.MergeState = "behind" }
func noChecks(pr *hp.SessionPR)    { pr.Checks = "" }
func closed(pr *hp.SessionPR)      { pr.State = "closed" }
func mergedAgo(d time.Duration) func(*hp.SessionPR) {
	return func(pr *hp.SessionPR) { pr.State, pr.MergedAt = "merged", now.Add(-d).Format(time.RFC3339) }
}

// PR → normalized status (state, detail, reasons), the tone it implies, the
// v1 phase and the chip.
func TestPRStatusPhaseToneLabel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pr        hp.SessionPR
		state     string
		detail    string
		reasons   string
		phase     string
		attention string
		tone      string
		label     string
	}{
		{"approved and green", open(79, approved), hp.StatusAwaitingMerge, "approved", "", hp.PRPhaseMerge, "", hp.ToneOK, "#79 ✓ approved"},
		{"no review needed", open(5), hp.StatusAwaitingMerge, "ready to merge", "", hp.PRPhaseMerge, "", hp.ToneOK, "#5 ✓ ready"},
		{"no checks", open(5, noChecks), hp.StatusAwaitingMerge, "ready to merge", "", hp.PRPhaseMerge, "", hp.ToneOK, "#5 ✓ ready"},
		{"approved, CI running", open(6, approved, pending), hp.StatusAwaitingMerge, "approved, CI running", "ci_pending", hp.PRPhaseMerge, "", hp.ToneOK, "#6 CI…"},
		{"review required", open(7, needsReview), hp.StatusAwaitingReview, "awaiting review", "", hp.PRPhaseReview, "", hp.TonePending, "#7 awaiting review"},
		{"review required, CI running", open(7, needsReview, pending), hp.StatusAwaitingReview, "awaiting review, CI running", "ci_pending", hp.PRPhaseReview, "", hp.TonePending, "#7 CI… review"},
		{"CI red", open(245, red), hp.StatusAttention, "CI red: test", "ci_red", hp.PRPhaseAttention, "ci_red", hp.ToneAttention, "#245 ✗ CI"},
		{"conflict", open(8, conflict), hp.StatusAttention, "conflict", "conflict", hp.PRPhaseAttention, "conflict", hp.ToneAttention, "#8 ✗ conflict"},
		{"changes requested", open(9, changes), hp.StatusAttention, "changes requested", "changes_requested", hp.PRPhaseAttention, "changes_requested", hp.ToneAttention, "#9 ✗ changes requested"},
		{"behind", open(10, behind), hp.StatusAttention, "behind", "behind", hp.PRPhaseAttention, "behind", hp.ToneAttention, "#10 behind"},
		{"draft", open(11, draft), hp.StatusAttention, "draft", "draft", hp.PRPhaseAttention, "draft", hp.ToneAttention, "#11 draft"},
		{"draft red conflicting", open(12, draft, red, conflict), hp.StatusAttention, "CI red: test", "draft,ci_red,conflict", hp.PRPhaseAttention, "draft,ci_red,conflict", hp.ToneAttention, "#12 ✗ CI"},
		{"merged", open(13, mergedAgo(time.Hour)), hp.StatusDone, "merged", "merged", hp.PRPhaseDone, "", hp.ToneDone, "#13 merged"},
		{"closed", open(14, closed), hp.StatusDone, "closed", "closed", hp.PRPhaseDone, "", hp.ToneDone, "#14 closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := Status(tc.pr)
			phase, att := Phase(tc.pr)
			if st.State != tc.state || st.Detail != tc.detail || strings.Join(st.Reasons, ",") != tc.reasons {
				t.Fatalf("status = %+v, want %s/%q/%s", st, tc.state, tc.detail, tc.reasons)
			}
			if phase != tc.phase || strings.Join(att, ",") != tc.attention || Tone(tc.pr) != tc.tone || Label(tc.pr) != tc.label {
				t.Fatalf("phase/attention/tone/label = %s/%v/%s/%q, want %s/%s/%s/%q",
					phase, att, Tone(tc.pr), Label(tc.pr), tc.phase, tc.attention, tc.tone, tc.label)
			}
		})
	}
}

func TestPullRequestEnvelopeShape(t *testing.T) {
	pr := open(79, approved, func(pr *hp.SessionPR) {
		pr.Title, pr.BaseBranch, pr.GitHubUpdatedAt, pr.CreatedAt = "chips", "master", "2026-11-22T17:58:40Z", "2026-11-21T09:00:00Z"
		pr.Reviews = []hp.PRReview{{Author: "octocat", State: "approved"}, {Author: "zed", State: "commented"}}
	})
	b, _ := json.Marshal(PullRequest(pr))
	want := `{"kind":"pull_request","id":"o/r#79","title":"chips","url":"https://github.com/o/r/pull/1",` +
		`"status":{"state":"awaiting_merge","detail":"approved","reasons":[]},"tone":"ok","label":"#79 ✓ approved",` +
		`"observed_at":"","updated_at":"2026-11-22T17:58:40Z","created_at":"2026-11-21T09:00:00Z","stale":false,` +
		`"pull_request":{"repo":"o/r","number":79,"title":"chips","url":"https://github.com/o/r/pull/1","state":"open","head_branch":"amux/wg-m1","base_branch":"master","phase":"merge","attention":[],` +
		`"ci":{"status":"green","failing_check":"","failing_checks":[]},"review":{"state":"approved","by":["octocat"],"requested":[]},` +
		`"mergeable":{"conflict":false,"behind":false,"blocked_by_checks":false},"updated_at":"2026-11-22T17:58:40Z","merged_at":"","closed_at":""}}`
	if string(b) != want {
		t.Fatalf("envelope JSON:\n got %s\nwant %s", b, want)
	}
	if e := PullRequest(open(1, mergedAgo(time.Hour))); e.FinishedAt != now.Add(-time.Hour).Format(time.RFC3339) {
		t.Fatalf("merged PR finished_at = %q", e.FinishedAt)
	}
	if e := PullRequest(open(1, red)); e.PullRequest.CI.FailingCheck != "test" || !e.PullRequest.Mergeable.BlockedByChecks {
		t.Fatalf("red envelope = %+v", e)
	}
}

func TestToneForEveryState(t *testing.T) {
	want := map[string]string{
		hp.StatusAttention: hp.ToneAttention, hp.StatusFailed: hp.ToneAttention,
		hp.StatusAwaitingReview: hp.TonePending, hp.StatusPending: hp.TonePending, hp.StatusActive: hp.TonePending,
		hp.StatusAwaitingMerge: hp.ToneOK, hp.StatusOK: hp.ToneOK, hp.StatusDone: hp.ToneDone,
		hp.StatusUnknown: hp.ToneNeutral, "brand_new_state": hp.ToneNeutral,
	}
	for state, tone := range want {
		if got := hp.ToneFor(state); got != tone {
			t.Errorf("ToneFor(%s) = %s, want %s", state, got, tone)
		}
	}
}

func TestMemberSettled(t *testing.T) {
	if !MemberSettled(nil) || !MemberSettled([]hp.SessionPR{open(1, mergedAgo(time.Hour))}) {
		t.Fatal("no open PR should be settled")
	}
	if MemberSettled([]hp.SessionPR{open(1, mergedAgo(time.Hour)), open(2, draft)}) {
		t.Fatal("an open draft is not settled")
	}
	c := open(3, closed)
	if !AllMerged([]hp.SessionPR{open(1, mergedAgo(time.Hour))}) || AllMerged(nil) ||
		AllMerged([]hp.SessionPR{open(1, mergedAgo(time.Hour)), c}) || !MemberSettled([]hp.SessionPR{c}) {
		t.Fatal("AllMerged: only a non-empty, all-merged set qualifies; closed-unmerged settles but does not merge")
	}
}

// prSource is the PR kind's freshness as the rollup sees it.
var prSource = SourceStatus{Kind: hp.EnrichmentPullRequest, Interval: 3 * time.Minute, LastOK: now}

func mem(id, state string, prs ...hp.SessionPR) Member {
	m := Member{ID: id, Label: id, State: state}
	for _, pr := range prs {
		m.Enrichments = append(m.Enrichments, PullRequest(pr))
	}
	return m
}

// The precedence matrix: waiting-on-user > running > pr_attention >
// waiting_review / waiting_merge > ready / idle, with the legacy state and
// sub-state each maps to.
func TestRollupPrecedence(t *testing.T) {
	ready, idle, running, waiting, unknown := hp.StateReady, hp.StateIdle, hp.StateRunning, hp.StateWaiting, hp.StateUnknown
	for _, tc := range []struct {
		name          string
		own, goal     string
		members       []Member
		rollup, state string
		sub           string
		count         int
		label         string
		reason        string
	}{
		{"member waiting beats everything", ready, "", []Member{mem("a", waiting), mem("b", running, open(1, red))},
			hp.RollupWaitingUser, waiting, hp.SubStateSupervising, 1, "waiting on you", "needs you: a; supervising b running, b PR #1 CI red: test"},
		{"coordinator waiting", waiting, "", []Member{mem("a", running)}, hp.RollupWaitingUser, waiting, "", 1, "waiting on you", "needs you: coordinator"},
		{"goal input between turns", ready, "input", []Member{mem("a", running)}, hp.RollupWaitingUser, waiting, hp.SubStateSupervising, 1, "waiting on you", "needs you: coordinator; supervising a running"},
		{"goal input mid-turn stays running", running, "input", nil, hp.RollupRunning, running, "", 0, "running", ""},
		{"running member beats a red PR", ready, "", []Member{mem("a", running), mem("b", ready, open(1, red))},
			hp.RollupRunning, running, hp.SubStateSupervising, 1, "running · 1 agent", "supervising a running, b PR #1 CI red: test"},
		{"unknown member counts as running", ready, "", []Member{mem("a", unknown)},
			hp.RollupRunning, running, hp.SubStateSupervising, 1, "running · 1 agent", "supervising a unknown"},
		{"coordinator running", running, "", []Member{mem("a", ready, open(1, approved))}, hp.RollupRunning, running, "", 0, "running", ""},
		{"attention beats review and merge", ready, "", []Member{mem("a", ready, open(1, red), open(2, needsReview)), mem("b", idle, open(3, approved))},
			hp.RollupPRAttention, running, hp.SubStatePRAttention, 1, "PR attention · 1 PR", "a PR #1 CI red: test"},
		{"attention with a stopped coordinator is ready", idle, "", []Member{mem("a", idle, open(1, conflict))},
			hp.RollupPRAttention, ready, hp.SubStatePRAttention, 1, "PR attention · 1 PR", "a PR #1 conflict; coordinator stopped"},
		{"review beats merge", ready, "", []Member{mem("a", ready, open(1, needsReview)), mem("b", ready, open(2, approved), open(3))},
			hp.RollupWaitingReview, ready, hp.SubStateWaitingReview, 1, "waiting for review · 1 PR · 2 ready to merge", "a PR #1 awaiting review, b PR #2 approved, b PR #3 ready to merge"},
		{"merge only", ready, "waiting", []Member{mem("a", ready, open(1, approved)), mem("b", idle, open(2, pending))},
			hp.RollupWaitingMerge, ready, hp.SubStateWaitingMerge, 2, "waiting for merge · 2 PRs", "a PR #1 approved, b PR #2 ready to merge, CI running"},
		{"merged PRs do not count", ready, "", []Member{mem("a", ready, open(1, mergedAgo(time.Hour)))}, hp.RollupReady, ready, "", 0, "ready", ""},
		{"idle", idle, "", []Member{mem("a", idle)}, hp.RollupIdle, idle, "", 0, "idle", ""},
		{"unknown coordinator alone", unknown, "", nil, hp.RollupReady, unknown, "", 0, "ready", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Rollup(Input{OwnState: tc.own, Goal: tc.goal, Members: tc.members, Sources: []SourceStatus{prSource}})
			ro := r.Rollup
			if ro.State != tc.rollup || r.State != tc.state || r.SubState != tc.sub || ro.Count != tc.count || ro.Label != tc.label {
				t.Fatalf("rollup/state/sub/count/label = %s/%s/%q/%d/%q, want %s/%s/%q/%d/%q",
					ro.State, r.State, r.SubState, ro.Count, ro.Label, tc.rollup, tc.state, tc.sub, tc.count, tc.label)
			}
			if ro.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", ro.Reason, tc.reason)
			}
			for _, s := range []string{"pr_attention", "waiting_review", "waiting_merge"} {
				if r.State == s {
					t.Errorf("legacy state carries a rollup state: %q", r.State)
				}
			}
		})
	}
}

// The rollup reads only normalized statuses: an item whose payload says
// "merged" but whose status says attention drives attention.
func TestRollupReadsOnlyStatus(t *testing.T) {
	e := PullRequest(open(1, mergedAgo(time.Hour)))
	e.Status = hp.EnrichmentStatus{State: hp.StatusAttention, Detail: "custom"}
	r := Rollup(Input{OwnState: hp.StateReady, Members: []Member{{ID: "a", Label: "a", State: hp.StateReady, Enrichments: []hp.Enrichment{e}}},
		Sources: []SourceStatus{prSource}})
	if r.Rollup.State != hp.RollupPRAttention || r.Rollup.Reason != "a PR #1 custom" {
		t.Fatalf("rollup = %+v", r.Rollup)
	}
	// An unknown state is unknown: counted, never driving the state.
	e.Status.State = "brand_new_state"
	r = Rollup(Input{OwnState: hp.StateReady, Members: []Member{{ID: "a", Label: "a", State: hp.StateReady, Enrichments: []hp.Enrichment{e}}},
		Sources: []SourceStatus{prSource}})
	if r.Rollup.State != hp.RollupReady || r.Rollup.Totals[hp.StatusUnknown] != 1 {
		t.Fatalf("unknown-state rollup = %+v", r.Rollup)
	}
}

func TestRollupCountsAndStaleness(t *testing.T) {
	ready := hp.StateReady
	members := []Member{
		mem("a", ready, open(1, red), open(2, draft, pending), open(3, mergedAgo(time.Hour))),
		mem("b", ready, open(4, needsReview), open(5, approved), open(6, closed)),
	}
	r := Rollup(Input{OwnState: ready, Members: members, Sources: []SourceStatus{prSource}})
	c := r.Rollup.Enrichments.PullRequest
	want := hp.PullRequestCounts{Open: 4, Draft: 1, Attention: 2, WaitingReview: 1, WaitingMerge: 1, CIPending: 1, Merged: 1, Closed: 1}
	if c == nil || *c != want {
		t.Fatalf("counts = %+v, want %+v", c, want)
	}
	if got := r.Rollup.Totals; got[hp.StatusAttention] != 2 || got[hp.StatusAwaitingReview] != 1 || got[hp.StatusAwaitingMerge] != 1 ||
		got[hp.StatusDone] != 2 || len(got) != len(hp.EnrichmentStates) {
		t.Fatalf("totals = %v", got)
	}
	if r.Rollup.Stale || r.Rollup.ObservedAt != "2026-11-22T18:00:00Z" ||
		len(r.Rollup.Sources) != 1 || r.Rollup.Sources[0] != (hp.EnrichmentSource{Kind: "pull_request", ObservedAt: "2026-11-22T18:00:00Z", IntervalS: 180}) {
		t.Fatalf("fresh rollup = %+v", r.Rollup)
	}

	// A stale source marks the rollup stale; the state stays computed.
	staleSrc := prSource
	staleSrc.Stale, staleSrc.StaleReason = true, hp.StaleGHUnavailable
	r = Rollup(Input{OwnState: ready, Members: members, Sources: []SourceStatus{staleSrc}})
	if !r.Rollup.Stale || r.Rollup.StaleReason != hp.StaleGHUnavailable || r.Rollup.State != hp.RollupPRAttention {
		t.Fatalf("stale rollup = %+v (state must stay computed from last-known data)", r.Rollup)
	}
	// A stale item (its repo's listing failed) too, with the source's last reason.
	staleMembers := []Member{mem("a", ready, open(1, red))}
	staleMembers[0].Enrichments[0].Stale = true
	lastFailed := prSource
	lastFailed.LastReason = hp.StaleRateLimited
	r = Rollup(Input{OwnState: ready, Members: staleMembers, Sources: []SourceStatus{lastFailed}})
	if !r.Rollup.Stale || r.Rollup.StaleReason != hp.StaleRateLimited {
		t.Fatalf("stale-item rollup = %+v", r.Rollup)
	}

	// No source registered: items neither count nor drive a state.
	r = Rollup(Input{OwnState: ready, Members: members})
	if r.Rollup.Enrichments.PullRequest != nil || r.Rollup.State != hp.RollupReady || r.Rollup.Stale {
		t.Fatalf("unobserved rollup = %+v", r.Rollup)
	}
	b, _ := json.Marshal(r.Rollup)
	if !strings.Contains(string(b), `"enrichments":{}`) || !strings.Contains(string(b), `"sources":[]`) {
		t.Fatalf("unobserved rollup JSON = %s", b)
	}
}

func TestNormalizeUnknownRollupState(t *testing.T) {
	for in, want := range map[string]string{
		"waiting_merge": "waiting_merge", "pr_attention": "pr_attention", "": "ready", "waiting_deploy": "ready",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}
