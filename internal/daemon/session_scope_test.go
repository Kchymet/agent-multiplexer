package daemon

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"testing"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

// TestOrdinaryAgentListsAreFilteredToSelf: list endpoints answer a member with
// its own scope instead of refusing it.
func TestOrdinaryAgentListsAreFilteredToSelf(t *testing.T) {
	db := policyFixture()
	resolver := fakeResolver(db)
	principal := access.Principal{Kind: access.SubjectSession, SubjectID: "a1"}

	rows, err := resolver.scopedSessionRows(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "wg1" || len(rows[0].Agents) != 1 || rows[0].Agents[0].ID != "a1" {
		t.Fatalf("member session rows = %+v, want only its own row under wg1", rows)
	}

	repos, err := resolver.scopedRepoRows(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(repos, []core.RepoRow{{Name: "api"}}) {
		t.Fatalf("member repo rows = %+v, want only its own repo", repos)
	}
}

func TestCoordinatorListsExcludeMovedOutMember(t *testing.T) {
	db := policyFixture()
	moved := db.sessions["a1"]
	moved.RootID = "wg2"
	db.sessions["a1"] = moved
	rows, err := fakeResolver(db).scopedSessionRows(context.Background(), access.Principal{Kind: access.SubjectSession, SubjectID: "wg1"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, row := range rows {
		ids = append(ids, row.ID)
		for _, agent := range row.Agents {
			ids = append(ids, agent.ID)
		}
	}
	sort.Strings(ids)
	if !reflect.DeepEqual(ids, []string{"old", "wg1"}) {
		t.Fatalf("coordinator visible ids = %v, want the root and its remaining member", ids)
	}
}

// TestSessionRPCScopeEndToEnd drives the production authorize+dispatch path
// with real credentials and store rows for each scope.
func TestSessionRPCScopeEndToEnd(t *testing.T) {
	wg1 := store.Session{ID: "wg1", Name: "one", Agent: "claude", Scope: store.ScopeWork, Dir: t.TempDir()}
	a1 := store.Session{ID: "a1", RootID: wg1.ID, Name: "a1", Agent: "claude", Dir: t.TempDir()}
	a2 := store.Session{ID: "a2", RootID: wg1.ID, Name: "a2", Agent: "claude", Dir: t.TempDir()}
	wg2 := store.Session{ID: "wg2", Name: "two", Agent: "claude", Scope: store.ScopeWork, Dir: t.TempDir()}
	b1 := store.Session{ID: "b1", RootID: wg2.ID, Name: "b1", Agent: "claude", Dir: t.TempDir()}
	_, runtime, principals := sessionRuntimeFixture(t, wg1, a1, a2, wg2, b1)

	n := 0
	call := func(who string, route access.Route, verb, id string, fields map[string]string) sessionrpc.DispatchResult {
		t.Helper()
		n++
		c := sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: route, Verb: verb, ID: id, Fields: fields}
		if err := runtime.authorize(context.Background(), principals[who], c); err != nil {
			return rpcDenied("access_denied")
		}
		result, err := runtime.dispatch(context.Background(), sessionrpc.DispatchRequest{
			Principal: principals[who], RequestID: fmt.Sprintf("%032x", n), Call: c,
		})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	rename := func(name string) map[string]string { return map[string]string{"name": name} }
	for _, tc := range []struct {
		name   string
		who    string
		route  access.Route
		verb   string
		id     string
		fields map[string]string
		want   sessionrpc.Status
	}{
		{"member status", "a1", access.RouteQuery, core.QuerySessions, "", nil, sessionrpc.StatusOK},
		{"member rename self", "a1", access.RouteAction, core.ActionRename, "a1", rename("self"), sessionrpc.StatusOK},
		{"member rename sibling", "a1", access.RouteAction, core.ActionRename, "a2", rename("x"), sessionrpc.StatusDenied},
		{"member rename coordinator", "a1", access.RouteAction, core.ActionRename, "wg1", rename("x"), sessionrpc.StatusDenied},
		{"member delete self", "a1", access.RouteAction, core.ActionDelete, "a1", nil, sessionrpc.StatusDenied},
		{"coordinator rename member", "wg1", access.RouteAction, core.ActionRename, "a2", rename("member"), sessionrpc.StatusOK},
		{"coordinator rename workgroup", "wg1", access.RouteAction, core.ActionRename, "wg1", rename("group"), sessionrpc.StatusOK},
		{"coordinator rename foreign", "wg1", access.RouteAction, core.ActionRename, "b1", rename("x"), sessionrpc.StatusDenied},
		{"coordinator rename foreign workgroup", "wg1", access.RouteAction, core.ActionRename, "wg2", rename("x"), sessionrpc.StatusDenied},
		{"coordinator delete member", "wg1", access.RouteAction, core.ActionDelete, "a2", nil, sessionrpc.StatusDenied},
		{"coordinator move member", "wg1", access.RouteAction, core.ActionMove, "a2", nil, sessionrpc.StatusDenied},
		{"coordinator archive foreign workgroup", "wg1", access.RouteAction, core.ActionSetArchived, "wg2", map[string]string{"archived": "true"}, sessionrpc.StatusDenied},
		{"coordinator grant repos", "wg1", access.RouteAction, core.ActionAgentSetRepos, "a2", map[string]string{"repos": "api"}, sessionrpc.StatusDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := call(tc.who, tc.route, tc.verb, tc.id, tc.fields); got.Status != tc.want {
				t.Fatalf("%s = %+v, want %s", tc.name, got, tc.want)
			}
		})
	}

	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for id, want := range map[string]string{"a1": "self", "a2": "member", "wg1": "group", "b1": "b1", "wg2": "two"} {
		s, ok, err := db.GetSession(id)
		if err != nil || !ok {
			t.Fatalf("load %s: ok=%v err=%v", id, ok, err)
		}
		if s.Display() != want {
			t.Errorf("%s display = %q, want %q", id, s.Display(), want)
		}
	}
}
