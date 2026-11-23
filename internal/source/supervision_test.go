package source

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/enrich"
	"amux/internal/store"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func member(id, name, state string, prs ...harnessproto.SessionPR) memberView {
	v := memberView{session: store.Session{ID: id, Name: name}, state: state, prs: prs}
	for _, pr := range prs {
		v.items = append(v.items, enrich.PullRequest(pr))
	}
	return v
}

// prSources is a fresh pull_request source.
func prSources() []enrich.SourceStatus {
	return []enrich.SourceStatus{{Kind: harnessproto.EnrichmentPullRequest, Interval: 3 * time.Minute, LastOK: time.Now()}}
}

// watching is a prContext with the PR source registered and fresh.
func watching() prContext { return prContext{sources: prSources()} }

// fakeProbe renders a fixed set of PRs per session, as the registry would.
type fakeProbe map[string][]harnessproto.SessionPR

func (f fakeProbe) Enrichments(id string) *harnessproto.Enrichments {
	var list harnessproto.Enrichments
	for _, pr := range f[id] {
		list = append(list, enrich.PullRequest(pr))
	}
	if len(list) == 0 {
		return nil
	}
	return &list
}

func (f fakeProbe) Sources() []enrich.SourceStatus { return prSources() }

func TestSuperviseCoordinator(t *testing.T) {
	openPending := harnessproto.SessionPR{Repo: "o/r", Number: 12, State: "open", Checks: "pending"}
	merged := harnessproto.SessionPR{Repo: "o/r", Number: 9, State: "merged"}
	openRed := harnessproto.SessionPR{Repo: "o/r", Number: 13, State: "open", Checks: "fail", FailedChecks: []string{"test"}}
	cases := []struct {
		name               string
		own, goal, note    string
		members            []memberView
		state, sub, lead   string
		reason             string // substring; "" means empty
		supervisedMemberID []string
	}{
		{
			name: "idle when it and all members are idle",
			own:  core.StateReady, members: []memberView{member("m1", "a", core.StateReady), member("m2", "", core.StateIdle, merged)},
			state: core.StateReady, lead: core.StateReady,
		},
		{
			name: "ready coordinator with a running member supervises",
			own:  core.StateReady, goal: "waiting", note: "m1 build",
			members: []memberView{member("m1", "builder", core.StateRunning), member("m2", "", core.StateReady)},
			state:   core.StateRunning, sub: core.SubStateSupervising, lead: core.StateRunning,
			reason: "supervising builder running", supervisedMemberID: []string{"m1"},
		},
		{
			name: "a member waiting on the user keeps the attention chip",
			own:  core.StateReady, members: []memberView{member("m1", "a", core.StateRunning), member("m2", "b", core.StateWaiting)},
			state: core.StateWaiting, sub: core.SubStateSupervising, lead: core.StateWaiting,
			reason: "needs you: b; supervising a running", supervisedMemberID: []string{"m1", "m2"},
		},
		{
			name: "goal waiting on an open PR is calm: waiting for merge",
			own:  core.StateReady, goal: "waiting",
			members: []memberView{member("m1", "a", core.StateReady, openPending)},
			state:   core.StateReady, sub: harnessproto.SubStateWaitingMerge, lead: "waiting for merge · 1 PR",
			reason: "a PR #12 ready to merge, CI running", supervisedMemberID: []string{"m1"},
		},
		{
			name:    "a red PR is the coordinator's attention, not the user's",
			own:     core.StateReady,
			members: []memberView{member("m1", "a", core.StateReady, openRed)},
			state:   core.StateRunning, sub: harnessproto.SubStatePRAttention, lead: "PR attention · 1 PR",
			reason: "a PR #13 CI red: test", supervisedMemberID: []string{"m1"},
		},
		{
			name:    "a stopped coordinator with a red PR is ready and says so",
			own:     core.StateIdle,
			members: []memberView{member("m1", "a", core.StateIdle, openRed)},
			state:   core.StateReady, sub: harnessproto.SubStatePRAttention, lead: "PR attention · 1 PR",
			reason: "coordinator stopped", supervisedMemberID: []string{"m1"},
		},
		{
			name: "a running coordinator is plain running",
			own:  core.StateRunning, members: []memberView{member("m1", "a", core.StateRunning)},
			state: core.StateRunning, lead: core.StateRunning, supervisedMemberID: []string{"m1"},
		},
		{
			name: "goal input needs the user",
			own:  core.StateReady, goal: "input", note: "which repo?",
			members: []memberView{member("m1", "a", core.StateRunning)},
			state:   core.StateWaiting, sub: core.SubStateSupervising, lead: core.StateWaiting,
			reason:             "needs you: coordinator; goal input: which repo?",
			supervisedMemberID: []string{"m1"},
		},
		{
			name: "a stopped coordinator with working members says wake-ups wait",
			own:  core.StateIdle, members: []memberView{member("m1", "a", core.StateRunning)},
			state: core.StateRunning, sub: core.SubStateSupervising, lead: core.StateRunning,
			reason: "coordinator stopped", supervisedMemberID: []string{"m1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := superviseCoordinator(tc.own, tc.goal, tc.note, tc.members, watching())
			if v.state != tc.state || v.subState != tc.sub || v.lead != tc.lead {
				t.Fatalf("state/sub/lead = %q/%q/%q, want %q/%q/%q", v.state, v.subState, v.lead, tc.state, tc.sub, tc.lead)
			}
			if tc.reason == "" && v.supervision.Reason != "" {
				t.Errorf("reason = %q, want empty", v.supervision.Reason)
			}
			if !strings.Contains(v.supervision.Reason, tc.reason) {
				t.Errorf("reason = %q, want it to contain %q", v.supervision.Reason, tc.reason)
			}
			var ids []string
			for _, m := range v.supervision.Members {
				ids = append(ids, m.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.supervisedMemberID, ",") {
				t.Errorf("supervised members = %v, want %v", ids, tc.supervisedMemberID)
			}
			if v.supervision.OwnState != tc.own || v.supervision.Goal != tc.goal {
				t.Errorf("supervision own/goal = %q/%q", v.supervision.OwnState, v.supervision.Goal)
			}
		})
	}
}

func TestSuperviseCoordinatorBoundsReason(t *testing.T) {
	var ms []memberView
	for _, id := range []string{"m1", "m2", "m3", "m4", "m5"} {
		ms = append(ms, member(id, id, core.StateRunning))
	}
	v := superviseCoordinator(core.StateReady, "", "", ms, watching())
	if !strings.Contains(v.supervision.Reason, "+2 more") || strings.Contains(v.supervision.Reason, "m4") {
		t.Fatalf("reason = %q", v.supervision.Reason)
	}
	long := strings.Repeat("é", 400)
	if got := superviseCoordinator(core.StateReady, "blocked", long, nil, watching()).supervision.GoalNote; len(got) > maxGoalNoteBytes+len("…") {
		t.Fatalf("goal note not bounded: %d bytes", len(got))
	}
}

// The coordinator row of `amux status --json`: with only an open member PR
// outstanding it is calm (waiting for merge, state ready), with its goal and
// the awaited members in `supervision`, the rollup in `rollup`, and the
// member row's PR in `enrichments`.
func TestPollCoordinatorSupervising(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("HOME", t.TempDir())
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []store.Session{
		{ID: "wg1", Agent: "claude", Mode: store.ModeTask, Scope: store.ScopeWork, Created: 1},
		{ID: "m1", Name: "builder", RootID: "wg1", Agent: "claude", Mode: store.ModeTask, Branch: "amux/wg1-m1", Created: 2},
	} {
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	w := NewWorkspace()
	w.SetGoalInfo(func(store.Session) (string, string) { return "waiting", "m1 PR #3 CI" })
	w.SetEnrichments(fakeProbe{"m1": {{Repo: "o/r", Number: 3, State: "open", Checks: "pending"}}})
	w.SetPRs(func(id string) []harnessproto.SessionPR {
		if id == "m1" {
			return []harnessproto.SessionPR{{Repo: "o/r", Number: 3, State: "open", Checks: "pending"}}
		}
		return nil
	})
	rows, err := w.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var root, m1 core.Session
	for _, r := range rows {
		switch r.ID {
		case "wg1":
			root = r
		case "m1":
			m1 = r
		}
	}
	// Neither session is live in this test, so the coordinator's own state is
	// idle; its goal waits on an open member PR (CI running, no review needed).
	if root.State != core.StateReady || root.SubState != harnessproto.SubStateWaitingMerge {
		t.Fatalf("root state/sub = %q/%q (status %q)", root.State, root.SubState, root.Status)
	}
	if !strings.HasPrefix(root.Status, "waiting for merge · 1 PR · 1 agent · goal waiting · builder PR #3 ready to merge, CI running") {
		t.Errorf("root status = %q", root.Status)
	}
	if root.Rollup == nil || root.Rollup.State != harnessproto.RollupWaitingMerge || root.Rollup.Count != 1 ||
		root.Rollup.Enrichments.PullRequest == nil || root.Rollup.Enrichments.PullRequest.CIPending != 1 {
		t.Fatalf("rollup = %+v", root.Rollup)
	}
	if m1.Enrichments == nil || len(*m1.Enrichments) != 1 || (*m1.Enrichments)[0].ID != "o/r#3" {
		t.Fatalf("member enrichments = %+v", m1.Enrichments)
	}
	if root.Supervision == nil || root.Supervision.Goal != "waiting" || root.Supervision.GoalNote != "m1 PR #3 CI" ||
		len(root.Supervision.Members) != 1 || root.Supervision.Members[0].PRs != "PR #3 CI…" {
		t.Fatalf("supervision = %+v", root.Supervision)
	}
	b, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	var shape map[string]any
	if err := json.Unmarshal(b, &shape); err != nil {
		t.Fatal(err)
	}
	if shape["subState"] != "waiting_merge" {
		t.Errorf("JSON subState = %v", shape["subState"])
	}
	if ro, _ := shape["rollup"].(map[string]any); ro["state"] != "waiting_merge" || ro["label"] != "waiting for merge · 1 PR" {
		t.Errorf("JSON rollup = %v", shape["rollup"])
	}
	sup, _ := shape["supervision"].(map[string]any)
	for _, key := range []string{"ownState", "goal", "goalNote", "members", "reason"} {
		if _, ok := sup[key]; !ok {
			t.Errorf("JSON supervision lacks %q: %s", key, b)
		}
	}
}
