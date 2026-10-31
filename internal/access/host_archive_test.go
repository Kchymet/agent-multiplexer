package access

import (
	"context"
	"testing"

	"amux/internal/core"
)

func TestHostScopeArchivePolicy(t *testing.T) {
	resolver := fakeResolver{resources: map[string]Resource{
		"console": {ID: "console", Role: "console"},
		"wg1":     {ID: "wg1", Role: "coordinator", Scope: "work"},
		"a1":      {ID: "a1", RootID: "wg1"},
		"wg2":     {ID: "wg2", Role: "coordinator", Scope: "work"},
		"b1":      {ID: "b1", RootID: "wg2"},
		"b2":      {ID: "b2", RootID: "wg2", Archived: true},
		"api":     {ID: "api", Role: "repo", Scope: "repo", Repo: "api"},
		"hidden":  {ID: "hidden", Scope: "repo", Repo: "api"},
	}}
	archive := func(id string) Request {
		return Request{Route: RouteAction, Verb: core.ActionSetArchived, ID: id, Fields: map[string]string{"archived": "true"}}
	}
	restore := func(id string) Request {
		return Request{Route: RouteAction, Verb: core.ActionSetArchived, ID: id, Fields: map[string]string{"archived": "false"}}
	}
	coordinator := Principal{Kind: SubjectSession, SubjectID: "wg1"}
	member := Principal{Kind: SubjectSession, SubjectID: "a1"}
	repoHome := Principal{Kind: SubjectSession, SubjectID: "api"}

	tests := []struct {
		name          string
		who           Principal
		req           Request
		members, host bool
	}{
		{"foreign member archive", coordinator, archive("b1"), false, true},
		{"foreign workgroup archive", coordinator, archive("wg2"), false, true},
		{"own member archive unchanged", coordinator, archive("a1"), true, true},
		{"foreign restore", coordinator, restore("b1"), false, false},
		{"foreign already archived", coordinator, archive("b2"), false, false},
		{"foreign archive extra field", coordinator, Request{Route: RouteAction, Verb: core.ActionSetArchived, ID: "b1", Fields: map[string]string{"archived": "true", "x": "1"}}, false, false},
		{"foreign archive toggle", coordinator, Request{Route: RouteAction, Verb: core.ActionArchive, ID: "b1"}, false, false},
		{"foreign delete", coordinator, Request{Route: RouteAction, Verb: core.ActionDelete, ID: "b1"}, false, false},
		{"foreign kill", coordinator, Request{Route: RouteAction, Verb: core.ActionKill, ID: "b1"}, false, false},
		{"foreign steer", coordinator, Request{Route: RouteAction, Verb: core.ActionSteer, ID: "b1", Fields: map[string]string{core.SteerVerb: core.SteerPrompt}}, false, false},
		{"foreign start", coordinator, Request{Route: RouteAction, Verb: core.ActionStart, ID: "b1"}, false, false},
		{"foreign rename", coordinator, Request{Route: RouteAction, Verb: core.ActionRename, ID: "b1", Fields: map[string]string{"name": "x"}}, false, false},
		{"foreign events", coordinator, Request{Route: RouteQuery, Verb: core.QueryRuntimeEvents, ID: "b1"}, false, false},
		{"console archive", coordinator, archive("console"), false, false},
		{"repo home archive", coordinator, archive("api"), false, false},
		{"legacy hidden root archive", coordinator, archive("hidden"), false, false},
		{"member foreign archive", member, archive("b1"), false, false},
		{"repo home foreign archive", repoHome, archive("b1"), false, false},
	}
	for _, scope := range []struct {
		name string
		host func(context.Context) bool
	}{
		{"members", nil},
		{"members-explicit", func(context.Context) bool { return false }},
		{"host", func(context.Context) bool { return true }},
	} {
		p := Policy{Resolver: resolver, HostArchive: scope.host}
		for _, tt := range tests {
			want := tt.members
			if scope.name == "host" {
				want = tt.host
			}
			err := p.Authorize(context.Background(), tt.who, tt.req)
			if got := err == nil; got != want {
				t.Errorf("%s/%s: allowed=%v want %v (err %v)", scope.name, tt.name, got, want, err)
			}
		}
	}
}
