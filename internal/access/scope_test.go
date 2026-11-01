package access

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"amux/internal/core"
)

// coreOperationConstants parses internal/core for every Action*/Query* string
// constant, so a newly added operation fails this package's tests until it is
// given an explicit authorization scope.
func coreOperationConstants(t *testing.T) (actions, queries map[string]string) {
	t.Helper()
	dir := filepath.Join("..", "core")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	actions, queries = map[string]string{}, map[string]string{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, ident := range value.Names {
					if i >= len(value.Values) {
						continue
					}
					lit, ok := value.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					verb, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					switch {
					case strings.HasPrefix(ident.Name, "Action"):
						actions[ident.Name] = verb
					case strings.HasPrefix(ident.Name, "Query"):
						queries[ident.Name] = verb
					}
				}
			}
		}
	}
	if len(actions) == 0 || len(queries) == 0 {
		t.Fatalf("parsed no core operations (actions=%d queries=%d)", len(actions), len(queries))
	}
	return actions, queries
}

func TestEveryCoreOperationIsClassified(t *testing.T) {
	actions, queries := coreOperationConstants(t)
	for name, verb := range actions {
		route := RouteAction
		if strings.HasPrefix(verb, "pane.") {
			route = RoutePane
		}
		if _, ok := RuleFor(route, verb); !ok {
			t.Errorf("core.%s (%q) has no %s authorization scope in access.rules", name, verb, route)
		}
	}
	for name, verb := range queries {
		if _, ok := RuleFor(RouteQuery, verb); !ok {
			t.Errorf("core.%s (%q) has no query authorization scope in access.rules", name, verb)
		}
	}
	for _, verb := range core.ControlActions() {
		if _, ok := RuleFor(RouteAction, verb); !ok {
			t.Errorf("control action %q is unclassified", verb)
		}
	}
}

func TestUnclassifiedOperationsAreDeniedToEveryRole(t *testing.T) {
	resolver := fakeResolver{resources: map[string]Resource{
		"console": {ID: "console", Role: "console"},
		"wg1":     {ID: "wg1", Role: "coordinator", Scope: "work"},
		"a1":      {ID: "a1", RootID: "wg1"},
	}}
	p := Policy{Resolver: resolver}
	for _, who := range []string{"console", "wg1", "a1"} {
		for _, req := range []Request{
			{Route: RouteAction, Verb: "credential-admin", ID: who},
			{Route: RouteQuery, Verb: "future-dump", ID: who},
			{Route: "future-route", Verb: core.ActionRename, ID: who},
		} {
			if err := p.Authorize(context.Background(), Principal{Kind: SubjectSession, SubjectID: who}, req); !errors.Is(err, ErrDenied) {
				t.Errorf("%s %s %s: err=%v, want ErrDenied", who, req.Route, req.Verb, err)
			}
		}
	}
}

func TestScopeMatrix(t *testing.T) {
	resolver := fakeResolver{resources: map[string]Resource{
		"wg1":   {ID: "wg1", Role: "coordinator", Scope: "work"},
		"a1":    {ID: "a1", RootID: "wg1", Repo: "api"},
		"a2":    {ID: "a2", RootID: "wg1", Repo: "web"},
		"old":   {ID: "old", RootID: "wg1", Archived: true},
		"moved": {ID: "moved", RootID: "wg2"}, // was a wg1 member; RootID is current
		"wg2":   {ID: "wg2", Role: "coordinator", Scope: "work"},
		"b1":    {ID: "b1", RootID: "wg2"},
		"api":   {ID: "api", Role: "repo", Scope: "repo", Repo: "api"},
		"loose": {ID: "loose"}, // a non-coordinator root
	}, repos: map[string]bool{"wg1:api": true}}
	p := Policy{Resolver: resolver}
	session := func(id string) Principal { return Principal{Kind: SubjectSession, SubjectID: id} }
	action := func(verb, id string, fields map[string]string) Request {
		return Request{Route: RouteAction, Verb: verb, ID: id, Fields: fields}
	}
	query := func(verb, id string) Request { return Request{Route: RouteQuery, Verb: verb, ID: id} }
	steer := func(verb string) map[string]string { return map[string]string{core.SteerVerb: verb} }
	permission := map[string]string{core.SteerVerb: core.SteerPermission, core.SteerRequestID: "r1", RuntimeGenerationField: "g1"}
	archive := map[string]string{"archived": "true"}
	restore := map[string]string{"archived": "false"}
	rename := map[string]string{"name": "n"}

	type tc struct {
		name string
		who  Principal
		req  Request
		want bool
	}
	cases := []tc{
		// Self scope: an ordinary member acting on itself.
		{"self status sessions", session("a1"), query(core.QuerySessions, ""), true},
		{"self status snapshot", session("a1"), query(core.QuerySnapshot, ""), true},
		{"self repos", session("a1"), query(core.QueryRepos, ""), true},
		{"self events", session("a1"), query(core.QueryRuntimeEvents, "a1"), true},
		{"self rename", session("a1"), action(core.ActionRename, "a1", rename), true},
		{"self start", session("a1"), action(core.ActionStart, "a1", nil), true},
		{"self steer prompt", session("a1"), action(core.ActionSteer, "a1", steer(core.SteerPrompt)), true},
		{"self steer interject", session("a1"), action(core.ActionSteer, "a1", steer(core.SteerInterject)), true},
		{"self steer stop", session("a1"), action(core.ActionSteer, "a1", steer(core.SteerStop)), true},
		{"self done", session("a1"), action(core.ActionSetArchived, "a1", archive), true},
		{"self answers own permission", session("a1"), action(core.ActionSteer, "a1", permission), false},
		{"self goal steer", session("a1"), action(core.ActionSteer, "a1", steer(core.SteerGoal)), false},
		{"self restore", session("a1"), action(core.ActionSetArchived, "a1", restore), false},
		{"self add-agent", session("a1"), action(core.ActionAddAgent, "a1", nil), false},
		{"self sibling events", session("a1"), query(core.QueryRuntimeEvents, "a2"), false},
		{"self sibling rename", session("a1"), action(core.ActionRename, "a2", rename), false},
		{"self sibling steer", session("a1"), action(core.ActionSteer, "a2", steer(core.SteerStop)), false},
		{"self coordinator rename", session("a1"), action(core.ActionRename, "wg1", rename), false},
		{"self coordinator events", session("a1"), query(core.QueryRuntimeEvents, "wg1"), false},
		{"self foreign start", session("a1"), action(core.ActionStart, "b1", nil), false},
		{"self missing target", session("a1"), action(core.ActionRename, "", rename), false},
		{"self unknown target", session("a1"), action(core.ActionRename, "ghost", rename), false},
		{"non-coordinator root no archive", session("loose"), action(core.ActionSetArchived, "loose", archive), false},
		{"repo home self rename", session("api"), action(core.ActionRename, "api", rename), true},
		{"repo home self archive", session("api"), action(core.ActionSetArchived, "api", archive), false},
		{"repo home add-agent", session("api"), action(core.ActionAddAgent, "api", nil), false},

		// Coordinator inside its workgroup: itself and current direct members.
		{"coord status", session("wg1"), query(core.QuerySessions, ""), true},
		{"coord own events", session("wg1"), query(core.QueryRuntimeEvents, "wg1"), true},
		{"coord member events", session("wg1"), query(core.QueryRuntimeEvents, "a1"), true},
		{"coord archived member events", session("wg1"), query(core.QueryRuntimeEvents, "old"), true},
		{"coord rename self", session("wg1"), action(core.ActionRename, "wg1", rename), true},
		{"coord rename member", session("wg1"), action(core.ActionRename, "a1", rename), true},
		{"coord start workgroup", session("wg1"), action(core.ActionStart, "wg1", nil), true},
		{"coord start member", session("wg1"), action(core.ActionStart, "a2", nil), true},
		{"coord steer member", session("wg1"), action(core.ActionSteer, "a1", steer(core.SteerInterject)), true},
		{"coord stop member", session("wg1"), action(core.ActionSteer, "a1", steer(core.SteerStop)), true},
		{"coord answer member permission", session("wg1"), action(core.ActionSteer, "a1", permission), true},
		{"coord archive member", session("wg1"), action(core.ActionSetArchived, "a1", archive), true},
		{"coord add-agent own", session("wg1"), action(core.ActionAddAgent, "wg1", map[string]string{"repos": "api"}), true},
		{"coord answer own permission", session("wg1"), action(core.ActionSteer, "wg1", permission), false},
		{"coord archive own workgroup", session("wg1"), action(core.ActionSetArchived, "wg1", archive), true},
		{"coord restore own workgroup", session("wg1"), action(core.ActionSetArchived, "wg1", restore), false},
		{"coord archive foreign workgroup", session("wg1"), action(core.ActionSetArchived, "wg2", archive), false},
		{"coord restore member", session("wg1"), action(core.ActionSetArchived, "old", restore), false},
		{"coord act on archived member", session("wg1"), action(core.ActionStart, "old", nil), false},
		{"coord rename archived member", session("wg1"), action(core.ActionRename, "old", rename), false},
		{"coord add-agent ungranted repo", session("wg1"), action(core.ActionAddAgent, "wg1", map[string]string{"repos": "web"}), false},

		// Coordinator outside its workgroup.
		{"coord foreign events", session("wg1"), query(core.QueryRuntimeEvents, "b1"), false},
		{"coord foreign rename", session("wg1"), action(core.ActionRename, "b1", rename), false},
		{"coord foreign steer", session("wg1"), action(core.ActionSteer, "b1", steer(core.SteerStop)), false},
		{"coord foreign workgroup start", session("wg1"), action(core.ActionStart, "wg2", nil), false},
		{"coord foreign add-agent", session("wg1"), action(core.ActionAddAgent, "wg2", nil), false},
		{"coord moved-out member events", session("wg1"), query(core.QueryRuntimeEvents, "moved"), false},
		{"coord moved-out member steer", session("wg1"), action(core.ActionSteer, "moved", steer(core.SteerPrompt)), false},
		{"coord non-member root", session("wg1"), action(core.ActionRename, "loose", rename), false},
		{"coord repo home", session("wg1"), action(core.ActionRename, "api", rename), false},
		{"new coordinator owns moved member", session("wg2"), action(core.ActionRename, "moved", rename), true},
	}
	// Host-only operations: denied to members and coordinators even on targets
	// in their own scope, and always allowed for the host.
	hostOnly := []Request{
		action(core.ActionSetArchived, "wg1", restore),
		action(core.ActionSetArchived, "old", restore),
		action(core.ActionArchive, "a1", nil),
		action(core.ActionDelete, "a1", nil),
		action(core.ActionKill, "a1", nil),
		action(core.ActionMove, "a1", nil),
		action(core.ActionAddRepo, "", map[string]string{"source": "x"}),
		action(core.ActionRmRepo, "api", nil),
		action(core.ActionAgentSetRepos, "a1", map[string]string{"repos": "api"}),
		action(core.ActionCoordinatorSetRepos, "wg1", map[string]string{"repos": "api"}),
		action(core.ActionNewWorkgroup, "", nil),
		action(core.ActionCreateWorkspace, "", nil),
		action(core.ActionRestart, "a1", nil),
		action(core.ActionAuthReload, "", nil),
		action(core.ActionAccountManage, "", nil),
		action(core.ActionCodexLogin, "", nil),
		action(core.ActionRefresh, "", nil),
		{Route: RoutePane, Verb: core.ActionPaneOpen, ID: "a1"},
		query(core.QueryRuntimePath, "a1"),
		query(core.QueryRuntimeRecord, "a1"),
		query(core.QueryAccounts, ""),
		query(core.QueryCodexAuth, ""),
		query(core.QueryResources, ""),
	}
	for _, req := range hostOnly {
		label := string(req.Route) + " " + req.Verb + " " + req.ID
		cases = append(cases,
			tc{"host-only member " + label, session("a1"), req, false},
			tc{"host-only coordinator " + label, session("wg1"), req, false},
			tc{"host-only host " + label, Principal{Kind: SubjectHost}, req, true},
		)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := p.Authorize(context.Background(), c.who, c.req)
			if got := err == nil; got != c.want {
				t.Fatalf("Authorize() error = %v, success=%v want %v", err, got, c.want)
			}
			if !c.want && !errors.Is(err, ErrDenied) {
				t.Fatalf("denial = %v, want ErrDenied", err)
			}
		})
	}
}

func TestIdentityComesFromPrincipalNotRequest(t *testing.T) {
	resolver := fakeResolver{resources: map[string]Resource{
		"wg1": {ID: "wg1", Role: "coordinator", Scope: "work"},
		"a1":  {ID: "a1", RootID: "wg1"},
		"a2":  {ID: "a2", RootID: "wg1"},
	}}
	p := Policy{Resolver: resolver}
	// Caller-supplied identity claims in fields are never consulted: a member
	// claiming to be its coordinator still gets self scope only.
	req := Request{Route: RouteAction, Verb: core.ActionRename, ID: "a2", Fields: map[string]string{
		"name": "x", "subject": "wg1", "caller": "wg1", "role": "coordinator",
	}}
	if err := p.Authorize(context.Background(), Principal{Kind: SubjectSession, SubjectID: "a1"}, req); !errors.Is(err, ErrDenied) {
		t.Fatalf("forged identity fields: err=%v, want ErrDenied", err)
	}
}
