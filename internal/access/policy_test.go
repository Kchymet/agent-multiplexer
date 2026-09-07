package access

import (
	"context"
	"errors"
	"strings"
	"testing"

	"amux/internal/core"
)

type fakeResolver struct {
	resources map[string]Resource
	provider  map[string]bool
	repos     map[string]bool
}

func (f fakeResolver) RepoGranted(_ context.Context, subject, repo string) (bool, error) {
	return f.repos[subject+":"+repo], nil
}

func (f fakeResolver) Lookup(_ context.Context, id string) (Resource, bool, error) {
	r, ok := f.resources[id]
	return r, ok, nil
}

func (f fakeResolver) ProviderAllows(_ context.Context, subject string, req Request) (bool, error) {
	return f.provider[subject+":"+req.Verb+":"+req.ID], nil
}

func TestPolicyRoleMatrix(t *testing.T) {
	resolver := fakeResolver{resources: map[string]Resource{
		"console": {ID: "console", Role: "console"},
		"wg1":     {ID: "wg1", Role: "coordinator", Scope: "work"},
		"a1":      {ID: "a1", RootID: "wg1", Repo: "api"},
		"a2":      {ID: "a2", RootID: "wg1", Repo: "web"},
		"wg2":     {ID: "wg2", Role: "coordinator", Scope: "work"},
		"b1":      {ID: "b1", RootID: "wg2", Repo: "api"},
		"api":     {ID: "api", Role: "repo", Scope: "repo", Repo: "api"},
		"one":     {ID: "one", RootID: "hidden", Scope: "repo", Repo: "api"},
		"hidden":  {ID: "hidden", Scope: "repo", Repo: "api"},
		"unknown": {ID: "unknown", Role: "future-admin"},
	}, repos: map[string]bool{"wg1:api": true}}
	p := Policy{Resolver: resolver}

	tests := []struct {
		name string
		who  Principal
		req  Request
		want bool
	}{
		{"host pane", Principal{Kind: SubjectHost}, Request{Route: RoutePane, Verb: core.ActionPaneOpen, ID: "b1"}, true},
		{"agent rename self", Principal{Kind: SubjectSession, SubjectID: "a1"}, Request{Route: RouteAction, Verb: core.ActionRename, ID: "a1"}, true},
		{"agent rename sibling", Principal{Kind: SubjectSession, SubjectID: "a1"}, Request{Route: RouteAction, Verb: core.ActionRename, ID: "a2"}, false},
		{"agent unarchive self", Principal{Kind: SubjectSession, SubjectID: "a1"}, Request{Route: RouteAction, Verb: core.ActionSetArchived, ID: "a1", Fields: map[string]string{"archived": "false"}}, false},
		{"coordinator add own", Principal{Kind: SubjectSession, SubjectID: "wg1"}, Request{Route: RouteAction, Verb: core.ActionAddAgent, ID: "wg1"}, true},
		{"coordinator add granted repo", Principal{Kind: SubjectSession, SubjectID: "wg1"}, Request{Route: RouteAction, Verb: core.ActionAddAgent, ID: "wg1", Fields: map[string]string{"repos": "api"}}, true},
		{"coordinator add ungranted repo", Principal{Kind: SubjectSession, SubjectID: "wg1"}, Request{Route: RouteAction, Verb: core.ActionAddAgent, ID: "wg1", Fields: map[string]string{"repos": "api,secret"}}, false},
		{"coordinator add forged field", Principal{Kind: SubjectSession, SubjectID: "wg1"}, Request{Route: RouteAction, Verb: core.ActionAddAgent, ID: "wg1", Fields: map[string]string{"role": "console"}}, false},
		{"coordinator add foreign", Principal{Kind: SubjectSession, SubjectID: "wg1"}, Request{Route: RouteAction, Verb: core.ActionAddAgent, ID: "wg2"}, false},
		{"coordinator steer member", Principal{Kind: SubjectSession, SubjectID: "wg1"}, Request{Route: RouteAction, Verb: core.ActionSteer, ID: "a2", Fields: map[string]string{core.SteerVerb: core.SteerPrompt}}, true},
		{"coordinator empty permission id", Principal{Kind: SubjectSession, SubjectID: "wg1"}, Request{Route: RouteAction, Verb: core.ActionSteer, ID: "a2", Fields: map[string]string{core.SteerVerb: core.SteerPermission}}, false},
		{"coordinator permission generation", Principal{Kind: SubjectSession, SubjectID: "wg1"}, Request{Route: RouteAction, Verb: core.ActionSteer, ID: "a2", Fields: map[string]string{core.SteerVerb: core.SteerPermission, core.SteerRequestID: "r1", RuntimeGenerationField: "turn-2"}}, true},
		{"coordinator foreign member", Principal{Kind: SubjectSession, SubjectID: "wg1"}, Request{Route: RouteAction, Verb: core.ActionStart, ID: "b1"}, false},
		{"coordinator archive root", Principal{Kind: SubjectSession, SubjectID: "wg1"}, Request{Route: RouteAction, Verb: core.ActionSetArchived, ID: "wg1", Fields: map[string]string{"archived": "true"}}, false},
		{"repo new own", Principal{Kind: SubjectSession, SubjectID: "api"}, Request{Route: RouteAction, Verb: core.ActionNewRepoAgent, ID: "api"}, true},
		{"repo steer own oneoff", Principal{Kind: SubjectSession, SubjectID: "api"}, Request{Route: RouteAction, Verb: core.ActionSteer, ID: "one", Fields: map[string]string{core.SteerVerb: core.SteerStop}}, true},
		{"console lifecycle", Principal{Kind: SubjectSession, SubjectID: "console"}, Request{Route: RouteAction, Verb: core.ActionNewWorkgroup}, true},
		{"console auth admin", Principal{Kind: SubjectSession, SubjectID: "console"}, Request{Route: RouteAction, Verb: core.ActionAuthReload}, false},
		{"unknown role denied", Principal{Kind: SubjectSession, SubjectID: "unknown"}, Request{Route: RouteAction, Verb: core.ActionRename, ID: "unknown"}, false},
		{"unknown future action denied", Principal{Kind: SubjectSession, SubjectID: "console"}, Request{Route: RouteAction, Verb: "credential-admin"}, false},
		{"restricted pane", Principal{Kind: SubjectSession, SubjectID: "wg1"}, Request{Route: RoutePane, Verb: core.ActionPaneOpen, ID: "a1"}, false},
		{"raw runtime path", Principal{Kind: SubjectSession, SubjectID: "wg1"}, Request{Route: RouteQuery, Verb: core.QueryRuntimePath, ID: "a1"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := p.Authorize(context.Background(), tc.who, tc.req)
			if got := err == nil; got != tc.want {
				t.Fatalf("Authorize() error = %v, success=%v want %v", err, got, tc.want)
			}
			if !tc.want && !errors.Is(err, ErrDenied) {
				t.Fatalf("denial = %v, want ErrDenied", err)
			}
		})
	}
}

func TestRepoHomeDoesNotOwnWorkgroupAgentSharingRepo(t *testing.T) {
	resolver := fakeResolver{resources: map[string]Resource{
		"api":    {ID: "api", Role: "repo", Scope: "repo", Repo: "api"},
		"hidden": {ID: "hidden", Scope: "repo", Repo: "api"},
		"one":    {ID: "one", RootID: "hidden", Repo: "api"},
		"wg1":    {ID: "wg1", Role: "coordinator", Scope: "work"},
		"a1":     {ID: "a1", RootID: "wg1", Repo: "api"},
	}}
	p := Policy{Resolver: resolver}
	who := Principal{Kind: SubjectSession, SubjectID: "api"}
	for _, tc := range []struct {
		id   string
		want bool
	}{{"one", true}, {"a1", false}} {
		err := p.Authorize(context.Background(), who, Request{Route: RouteAction, Verb: core.ActionStart, ID: tc.id})
		if (err == nil) != tc.want {
			t.Errorf("start %s: error=%v want success=%v", tc.id, err, tc.want)
		}
	}
	snap := core.Snapshot{Sessions: []core.Session{{ID: "api"}, {ID: "hidden"}, {ID: "one"}, {ID: "wg1"}, {ID: "a1"}}}
	out, err := p.FilterSnapshot(context.Background(), who, snap)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range out.Sessions {
		ids = append(ids, s.ID)
	}
	if got := strings.Join(ids, ","); got != "api,hidden,one" {
		t.Fatalf("visible repo sessions = %s", got)
	}
}

func TestFilterSnapshotUsesResolvedMembershipAndRemovesHostPaths(t *testing.T) {
	resolver := fakeResolver{resources: map[string]Resource{
		"wg1": {ID: "wg1", Role: "coordinator"},
		"a1":  {ID: "a1", RootID: "wg1"},
		"wg2": {ID: "wg2", Role: "coordinator"},
		"b1":  {ID: "b1", RootID: "wg2"},
	}}
	in := core.Snapshot{Type: "snapshot", Sessions: []core.Session{
		{ID: "wg1", Cwd: "/secret/wg1", Pid: 10},
		{ID: "a1", RootID: "wg1", Cwd: "/secret/a1", Pid: 11},
		{ID: "wg2", Cwd: "/secret/wg2", Pid: 12},
		{ID: "detached", Cwd: "/secret/user", Pid: 13},
	}}
	out, err := (Policy{Resolver: resolver}).FilterSnapshot(context.Background(), Principal{Kind: SubjectSession, SubjectID: "wg1"}, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sessions) != 2 || out.Sessions[0].ID != "wg1" || out.Sessions[1].ID != "a1" {
		t.Fatalf("filtered snapshot = %+v", out.Sessions)
	}
	for _, s := range out.Sessions {
		if s.Cwd != "" || s.Pid != 0 {
			t.Fatalf("restricted snapshot leaked host fields: %+v", s)
		}
	}
}

func TestArchivedPrincipalDeniedActionsAndSnapshots(t *testing.T) {
	resolver := fakeResolver{resources: map[string]Resource{
		"wg1": {ID: "wg1", Role: "coordinator", Archived: true},
		"a1":  {ID: "a1", RootID: "wg1"},
	}}
	p := Policy{Resolver: resolver}
	who := Principal{Kind: SubjectSession, SubjectID: "wg1"}
	if err := p.Authorize(context.Background(), who, Request{Route: RouteAction, Verb: core.ActionStart, ID: "a1"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("archived action error = %v", err)
	}
	if _, err := p.FilterSnapshot(context.Background(), who, core.Snapshot{Sessions: []core.Session{{ID: "a1"}}}); !errors.Is(err, ErrDenied) {
		t.Fatalf("archived snapshot error = %v", err)
	}
}
