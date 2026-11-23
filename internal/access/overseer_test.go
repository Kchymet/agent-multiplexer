package access

import (
	"context"
	"sort"
	"testing"

	"amux/internal/core"
)

// approvingResolver is a fakeResolver that also answers ItemApprover.
type approvingResolver struct {
	fakeResolver
	acked map[string]bool
}

func (a approvingResolver) HostAcknowledged(_ context.Context, id string) (bool, error) {
	return a.acked[id], nil
}

func overseerFixture() approvingResolver {
	return approvingResolver{
		fakeResolver: fakeResolver{resources: map[string]Resource{
			"console":  {ID: "console", Role: "console"},
			"overseer": {ID: "overseer", Role: RoleOverseer},
			"wg1":      {ID: "wg1", Role: "coordinator", Scope: "work"},
			"a1":       {ID: "a1", RootID: "wg1", Repo: "api"},
			"wg2":      {ID: "wg2", Role: "coordinator", Scope: "work"},
			"gone":     {ID: "gone", RootID: "wg2", Archived: true},
			"api":      {ID: "api", Role: "repo", Scope: "repo", Repo: "api"},
		}},
		acked: map[string]bool{"it-acked": true},
	}
}

func overseerPrincipal() Principal { return Principal{Kind: SubjectSession, SubjectID: "overseer"} }

// TestOverseerRuleColumn pins exactly which classified operations carry the
// Overseer mark. Adjusting the overseer's powers means editing this list
// together with scope.go (and allowOverseer for the per-verb narrowing).
func TestOverseerRuleColumn(t *testing.T) {
	want := map[Route][]string{
		RouteAction: {
			core.ActionStart, core.ActionSteer, core.ActionSetArchived,
			core.ActionRaiseItem, core.ActionItemState,
			core.ActionRefresh, core.ActionNewWorkgroup,
		},
		RouteQuery: {
			core.QueryVersion, core.QueryCodexControl, core.QueryAgentSessions,
			core.QuerySessions, core.QuerySnapshot, core.QueryRepos,
			core.QueryRuntimeEvents, core.QueryWait, core.QueryItems, core.QueryQueue,
		},
	}
	for route, verbs := range rules {
		var got []string
		for verb, rule := range verbs {
			if rule.Overseer {
				got = append(got, verb)
			}
		}
		sort.Strings(got)
		exp := append([]string(nil), want[route]...)
		sort.Strings(exp)
		if len(got) != len(exp) {
			t.Errorf("%s: overseer rules = %v, want %v", route, got, exp)
			continue
		}
		for i := range got {
			if got[i] != exp[i] {
				t.Errorf("%s: overseer rules = %v, want %v", route, got, exp)
				break
			}
		}
	}
}

// TestOverseerAccessMatrix pins every cell of the overseer's column in
// docs/direct-console.md §2 against the policy.
func TestOverseerAccessMatrix(t *testing.T) {
	p := Policy{Resolver: overseerFixture()}
	steer := func(verb string) map[string]string { return map[string]string{core.SteerVerb: verb} }
	permission := map[string]string{core.SteerVerb: core.SteerPermission, core.SteerRequestID: "r1", RuntimeGenerationField: "g1", core.SteerDecision: "allow"}
	cases := []struct {
		name  string
		req   Request
		allow bool
	}{
		// Reads.
		{"sessions", Request{Route: RouteQuery, Verb: core.QuerySessions}, true},
		{"snapshot", Request{Route: RouteQuery, Verb: core.QuerySnapshot}, true},
		{"repos", Request{Route: RouteQuery, Verb: core.QueryRepos}, true},
		{"version", Request{Route: RouteQuery, Verb: core.QueryVersion}, true},
		{"agent sessions", Request{Route: RouteQuery, Verb: core.QueryAgentSessions}, true},
		{"queue", Request{Route: RouteQuery, Verb: core.QueryQueue}, true},
		{"events coordinator", Request{Route: RouteQuery, Verb: core.QueryRuntimeEvents, ID: "wg1"}, true},
		{"events member", Request{Route: RouteQuery, Verb: core.QueryRuntimeEvents, ID: "a1"}, true},
		{"events repo home", Request{Route: RouteQuery, Verb: core.QueryRuntimeEvents, ID: "api"}, true},
		{"events archived member", Request{Route: RouteQuery, Verb: core.QueryRuntimeEvents, ID: "gone"}, true},
		{"events console", Request{Route: RouteQuery, Verb: core.QueryRuntimeEvents, ID: "console"}, false},
		{"wait member", Request{Route: RouteQuery, Verb: core.QueryWait, ID: "a1"}, true},
		{"wait console", Request{Route: RouteQuery, Verb: core.QueryWait, ID: "console"}, false},
		{"items self", Request{Route: RouteQuery, Verb: core.QueryItems, ID: "overseer"}, true},
		{"items other", Request{Route: RouteQuery, Verb: core.QueryItems, ID: "wg1"}, false},
		{"runtime path", Request{Route: RouteQuery, Verb: core.QueryRuntimePath, ID: "a1"}, false},
		{"runtime record", Request{Route: RouteQuery, Verb: core.QueryRuntimeRecord, ID: "a1"}, false},
		{"accounts", Request{Route: RouteQuery, Verb: core.QueryAccounts}, false},
		{"account manage query", Request{Route: RouteQuery, Verb: core.QueryAccountManage}, false},
		{"account ceremony", Request{Route: RouteQuery, Verb: core.QueryAccountCeremony}, false},
		{"codex auth", Request{Route: RouteQuery, Verb: core.QueryCodexAuth}, false},
		{"resources", Request{Route: RouteQuery, Verb: core.QueryResources}, false},

		// Steering: coordinators only, never a permission answer.
		{"prompt coordinator", Request{Route: RouteAction, Verb: core.ActionSteer, ID: "wg1", Fields: steer(core.SteerPrompt)}, true},
		{"interject coordinator", Request{Route: RouteAction, Verb: core.ActionSteer, ID: "wg1", Fields: steer(core.SteerInterject)}, true},
		{"stop coordinator", Request{Route: RouteAction, Verb: core.ActionSteer, ID: "wg1", Fields: steer(core.SteerStop)}, true},
		{"goal coordinator", Request{Route: RouteAction, Verb: core.ActionSteer, ID: "wg1", Fields: steer(core.SteerGoal)}, false},
		{"permission coordinator", Request{Route: RouteAction, Verb: core.ActionSteer, ID: "wg1", Fields: permission}, false},
		{"permission member", Request{Route: RouteAction, Verb: core.ActionSteer, ID: "a1", Fields: permission}, false},
		{"prompt member", Request{Route: RouteAction, Verb: core.ActionSteer, ID: "a1", Fields: steer(core.SteerPrompt)}, false},
		{"prompt repo home", Request{Route: RouteAction, Verb: core.ActionSteer, ID: "api", Fields: steer(core.SteerPrompt)}, false},
		{"prompt console", Request{Route: RouteAction, Verb: core.ActionSteer, ID: "console", Fields: steer(core.SteerPrompt)}, false},
		{"prompt itself", Request{Route: RouteAction, Verb: core.ActionSteer, ID: "overseer", Fields: steer(core.SteerPrompt)}, false},

		// Start: a workgroup (its members), never one member or the console.
		{"start coordinator", Request{Route: RouteAction, Verb: core.ActionStart, ID: "wg1"}, true},
		{"start member", Request{Route: RouteAction, Verb: core.ActionStart, ID: "a1"}, false},
		{"start console", Request{Route: RouteAction, Verb: core.ActionStart, ID: "console"}, false},

		// Archive: explicit, unforced, a workgroup or a member.
		{"archive workgroup", Request{Route: RouteAction, Verb: core.ActionSetArchived, ID: "wg1", Fields: map[string]string{"archived": "true"}}, true},
		{"archive member", Request{Route: RouteAction, Verb: core.ActionSetArchived, ID: "a1", Fields: map[string]string{"archived": "true"}}, true},
		{"archive forced", Request{Route: RouteAction, Verb: core.ActionSetArchived, ID: "wg1", Fields: map[string]string{"archived": "true", "force": "true"}}, false},
		{"restore", Request{Route: RouteAction, Verb: core.ActionSetArchived, ID: "wg1", Fields: map[string]string{"archived": "false"}}, false},
		{"archive repo home", Request{Route: RouteAction, Verb: core.ActionSetArchived, ID: "api", Fields: map[string]string{"archived": "true"}}, false},
		{"archive console", Request{Route: RouteAction, Verb: core.ActionSetArchived, ID: "console", Fields: map[string]string{"archived": "true"}}, false},
		{"archive archived", Request{Route: RouteAction, Verb: core.ActionSetArchived, ID: "gone", Fields: map[string]string{"archived": "true"}}, false},
		{"archive toggle", Request{Route: RouteAction, Verb: core.ActionArchive, ID: "wg1"}, false},

		// Creation: only a workgroup from an item the user acknowledged.
		{"new workgroup from acked item", Request{Route: RouteAction, Verb: core.ActionNewWorkgroup, Fields: map[string]string{FromItemField: "it-acked", "name": "x"}}, true},
		{"new workgroup from unacked item", Request{Route: RouteAction, Verb: core.ActionNewWorkgroup, Fields: map[string]string{FromItemField: "it-open", "name": "x"}}, false},
		{"new workgroup without item", Request{Route: RouteAction, Verb: core.ActionNewWorkgroup, Fields: map[string]string{"name": "x"}}, false},
		{"add agent", Request{Route: RouteAction, Verb: core.ActionAddAgent, ID: "wg1", Fields: map[string]string{"repos": "api"}}, false},
		{"create workspace", Request{Route: RouteAction, Verb: core.ActionCreateWorkspace}, false},

		// Items: as itself (the global inbox owner); escalation is a coordinator's.
		{"raise item", Request{Route: RouteAction, Verb: core.ActionRaiseItem, ID: "overseer"}, true},
		{"item state", Request{Route: RouteAction, Verb: core.ActionItemState, ID: "overseer"}, true},
		{"item state as other", Request{Route: RouteAction, Verb: core.ActionItemState, ID: "wg1"}, false},
		{"escalate", Request{Route: RouteAction, Verb: core.ActionEscalateItem, ID: "overseer"}, false},

		// Never: answering the queue, admin and host operations.
		{"queue answer", Request{Route: RouteAction, Verb: core.ActionQueueAnswer}, false},
		{"queue dismiss", Request{Route: RouteAction, Verb: core.ActionQueueDismiss}, false},
		{"refresh", Request{Route: RouteAction, Verb: core.ActionRefresh}, true},
		{"rename", Request{Route: RouteAction, Verb: core.ActionRename, ID: "wg1"}, false},
		{"move", Request{Route: RouteAction, Verb: core.ActionMove, ID: "a1"}, false},
		{"delete", Request{Route: RouteAction, Verb: core.ActionDelete, ID: "wg1"}, false},
		{"kill", Request{Route: RouteAction, Verb: core.ActionKill, ID: "wg1"}, false},
		{"add repo", Request{Route: RouteAction, Verb: core.ActionAddRepo}, false},
		{"rm repo", Request{Route: RouteAction, Verb: core.ActionRmRepo}, false},
		{"agent set repos", Request{Route: RouteAction, Verb: core.ActionAgentSetRepos, ID: "a1"}, false},
		{"coordinator set repos", Request{Route: RouteAction, Verb: core.ActionCoordinatorSetRepos, ID: "wg1"}, false},
		{"restart", Request{Route: RouteAction, Verb: core.ActionRestart, ID: "wg1"}, false},
		{"auth reload", Request{Route: RouteAction, Verb: core.ActionAuthReload}, false},
		{"account manage", Request{Route: RouteAction, Verb: core.ActionAccountManage}, false},
		{"codex login", Request{Route: RouteAction, Verb: core.ActionCodexLogin}, false},
		{"subscribe", Request{Route: RouteAction, Verb: core.ActionSubscribe}, false},
		{"pane open", Request{Route: RoutePane, Verb: core.ActionPaneOpen, ID: "wg1"}, false},
	}
	for _, c := range cases {
		err := p.Authorize(context.Background(), overseerPrincipal(), c.req)
		if (err == nil) != c.allow {
			t.Errorf("%s: allowed=%v, want %v (err=%v)", c.name, err == nil, c.allow, err)
		}
	}
}

// TestOverseerCreateNeedsApprover: a resolver that cannot vouch for a user
// acknowledgment denies every create-from-item.
func TestOverseerCreateNeedsApprover(t *testing.T) {
	p := Policy{Resolver: overseerFixture().fakeResolver}
	req := Request{Route: RouteAction, Verb: core.ActionNewWorkgroup, Fields: map[string]string{FromItemField: "it-acked"}}
	if err := p.Authorize(context.Background(), overseerPrincipal(), req); err == nil {
		t.Fatal("create-from-item allowed without an ItemApprover")
	}
}

// TestNobodyBelowTheHostSteersTheOverseer: coordinators, members and repo
// homes never reach the overseer (they raise items); the admin console may.
func TestNobodyBelowTheHostSteersTheOverseer(t *testing.T) {
	p := Policy{Resolver: overseerFixture()}
	req := Request{Route: RouteAction, Verb: core.ActionSteer, ID: "overseer", Fields: map[string]string{core.SteerVerb: core.SteerPrompt}}
	for _, who := range []string{"wg1", "a1", "api"} {
		if err := p.Authorize(context.Background(), Principal{Kind: SubjectSession, SubjectID: who}, req); err == nil {
			t.Errorf("%s may steer the overseer", who)
		}
	}
	if err := p.Authorize(context.Background(), Principal{Kind: SubjectSession, SubjectID: "console"}, req); err != nil {
		t.Errorf("the admin console may not steer the overseer: %v", err)
	}
}

// TestQueueReadIsConsoleAndOverseerOnly: only the admin console and the
// overseer read the queue; nobody below the host answers it.
func TestQueueReadIsConsoleAndOverseerOnly(t *testing.T) {
	p := Policy{Resolver: overseerFixture()}
	read := Request{Route: RouteQuery, Verb: core.QueryQueue}
	answer := Request{Route: RouteAction, Verb: core.ActionQueueAnswer}
	for who, canRead := range map[string]bool{"console": true, "overseer": true, "wg1": false, "a1": false, "api": false} {
		principal := Principal{Kind: SubjectSession, SubjectID: who}
		if err := p.Authorize(context.Background(), principal, read); (err == nil) != canRead {
			t.Errorf("%s queue read allowed=%v, want %v", who, err == nil, canRead)
		}
		if err := p.Authorize(context.Background(), principal, answer); err == nil {
			t.Errorf("%s may answer the queue", who)
		}
	}
}

func TestOverseerWorkgroupScope(t *testing.T) {
	o := Resource{ID: "overseer", Role: RoleOverseer}
	for _, target := range []Resource{{ID: "wg1", Role: "coordinator"}, {ID: "a1", RootID: "wg1"}, {ID: "api", Role: "repo"}, o} {
		if !InWorkgroupScope(o, target) {
			t.Errorf("overseer scope excludes %+v", target)
		}
	}
	if InWorkgroupScope(o, Resource{ID: "console", Role: "console"}) {
		t.Error("overseer scope includes the console")
	}
	if !InWorkgroupScope(Resource{ID: "console", Role: "console"}, o) {
		t.Error("the admin console's scope excludes the overseer")
	}
}
