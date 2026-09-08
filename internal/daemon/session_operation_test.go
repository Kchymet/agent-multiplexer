package daemon

import (
	"reflect"
	"strings"
	"testing"

	"amux/internal/access"
	"amux/internal/core"
)

func TestCanonicalSessionOperationClosedVocabulary(t *testing.T) {
	validPermission := map[string]string{
		core.SteerVerb:                core.SteerPermission,
		core.SteerDecision:            core.SteerAllow,
		core.SteerRequestID:           "permission-1",
		access.RuntimeGenerationField: "runtime-7",
	}
	tests := []struct {
		name string
		req  access.Request
		ok   bool
	}{
		{"query sessions", access.Request{Route: access.RouteQuery, Verb: core.QuerySessions}, true},
		{"unknown route", access.Request{Route: "future", Verb: core.QuerySessions}, false},
		{"unknown query", access.Request{Route: access.RouteQuery, Verb: "future"}, false},
		{"query fields", access.Request{Route: access.RouteQuery, Verb: core.QuerySessions, Fields: map[string]string{"all": "1"}}, false},
		{"runtime needs id", access.Request{Route: access.RouteQuery, Verb: core.QueryRuntimeRecord}, false},
		{"runtime recognized", access.Request{Route: access.RouteQuery, Verb: core.QueryRuntimeRecord, ID: "a1"}, true},
		{"runtime events cursor", access.Request{Route: access.RouteQuery, Verb: core.QueryRuntimeEvents, ID: "a1", Fields: map[string]string{core.RuntimeEventsCursorField: strings.Repeat("a", eventCursorEncodedLength)}}, true},
		{"runtime events after", access.Request{Route: access.RouteQuery, Verb: core.QueryRuntimeEvents, ID: "a1", Fields: map[string]string{core.RuntimeEventsAfterSequenceField: "7"}}, true},
		{"runtime events unknown field", access.Request{Route: access.RouteQuery, Verb: core.QueryRuntimeEvents, ID: "a1", Fields: map[string]string{"path": "/host/transcript"}}, false},
		{"runtime events ambiguous position", access.Request{Route: access.RouteQuery, Verb: core.QueryRuntimeEvents, ID: "a1", Fields: map[string]string{core.RuntimeEventsCursorField: strings.Repeat("a", eventCursorEncodedLength), core.RuntimeEventsAfterSequenceField: "7"}}, false},
		{"runtime events invalid sequence", access.Request{Route: access.RouteQuery, Verb: core.QueryRuntimeEvents, ID: "a1", Fields: map[string]string{core.RuntimeEventsAfterSequenceField: "-1"}}, false},
		{"start", access.Request{Route: access.RouteAction, Verb: core.ActionStart, ID: "a1"}, true},
		{"start missing id", access.Request{Route: access.RouteAction, Verb: core.ActionStart}, false},
		{"streaming tab", access.Request{Route: access.RouteAction, Verb: core.ActionStart, ID: "a1", Tab: 1}, false},
		{"unknown field", access.Request{Route: access.RouteAction, Verb: core.ActionAddAgent, ID: "wg1", Fields: map[string]string{"role": "console"}}, false},
		{"reserved console mode", access.Request{Route: access.RouteAction, Verb: core.ActionAddAgent, ID: "wg1", Fields: map[string]string{"mode": "console"}}, false},
		{"unknown creation mode", access.Request{Route: access.RouteAction, Verb: core.ActionNewRepoAgent, ID: "repo", Fields: map[string]string{"mode": "future"}}, false},
		{"interactive creation mode", access.Request{Route: access.RouteAction, Verb: core.ActionAddAgent, ID: "wg1", Fields: map[string]string{"mode": "interactive"}}, true},
		{"rename missing field", access.Request{Route: access.RouteAction, Verb: core.ActionRename, ID: "a1"}, false},
		{"move exact target", access.Request{Route: access.RouteAction, Verb: core.ActionMove, ID: "a1", Target: "wg2"}, true},
		{"target smuggling", access.Request{Route: access.RouteAction, Verb: core.ActionRename, ID: "a1", Target: "wg2", Fields: map[string]string{"name": "x"}}, false},
		{"archive boolean", access.Request{Route: access.RouteAction, Verb: core.ActionSetArchived, ID: "a1", Fields: map[string]string{"archived": "true"}}, true},
		{"restricted restore requires host regrant", access.Request{Route: access.RouteAction, Verb: core.ActionSetArchived, ID: "a1", Fields: map[string]string{"archived": "false"}}, false},
		{"ambiguous archive toggle is host only", access.Request{Route: access.RouteAction, Verb: core.ActionArchive, ID: "a1"}, false},
		{"archive invalid boolean", access.Request{Route: access.RouteAction, Verb: core.ActionSetArchived, ID: "a1", Fields: map[string]string{"archived": "yes"}}, false},
		{"permission", access.Request{Route: access.RouteAction, Verb: core.ActionSteer, ID: "a1", Fields: validPermission}, true},
		{"permission empty id", access.Request{Route: access.RouteAction, Verb: core.ActionSteer, ID: "a1", Fields: map[string]string{
			core.SteerVerb: core.SteerPermission, core.SteerDecision: core.SteerDeny,
			access.RuntimeGenerationField: "runtime-7",
		}}, false},
		{"permission empty generation", access.Request{Route: access.RouteAction, Verb: core.ActionSteer, ID: "a1", Fields: map[string]string{
			core.SteerVerb: core.SteerPermission, core.SteerDecision: core.SteerDeny,
			core.SteerRequestID: "permission-1",
		}}, false},
		{"permission unknown decision", access.Request{Route: access.RouteAction, Verb: core.ActionSteer, ID: "a1", Fields: map[string]string{
			core.SteerVerb: core.SteerPermission, core.SteerDecision: "later",
			core.SteerRequestID: "permission-1", access.RuntimeGenerationField: "runtime-7",
		}}, false},
		{"create role smuggling", access.Request{Route: access.RouteAction, Verb: core.ActionCreateWorkspace, Fields: map[string]string{"role": "console"}}, false},
		{"create invalid default", access.Request{Route: access.RouteAction, Verb: core.ActionCreateWorkspace, Fields: map[string]string{"defaultAgent": "true"}}, false},
		{"unknown action", access.Request{Route: access.RouteAction, Verb: "future", ID: "a1"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := canonicalSessionOperation(tc.req)
			if (err == nil) != tc.ok {
				t.Fatalf("canonicalSessionOperation(%+v) error = %v, ok=%v", tc.req, err, tc.ok)
			}
		})
	}
}

func TestCanonicalSessionOperationIsExactCopy(t *testing.T) {
	fields := map[string]string{"name": "renamed"}
	req := access.Request{Route: access.RouteAction, Verb: core.ActionRename, ID: "a1", Fields: fields}
	got, err := canonicalSessionOperation(req)
	if err != nil {
		t.Fatal(err)
	}
	want := core.Action{Action: core.ActionRename, ID: "a1", Fields: map[string]string{"name": "renamed"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("action = %+v, want %+v", got, want)
	}
	fields["name"] = "changed after authorization"
	if got.Fields["name"] != "renamed" {
		t.Fatalf("canonical action aliased caller fields: %+v", got)
	}
}

func TestCanonicalRuntimeEventQueryPreservesOnlyNormalizedPosition(t *testing.T) {
	fields := map[string]string{core.RuntimeEventsAfterSequenceField: "9"}
	req := access.Request{Route: access.RouteQuery, Verb: core.QueryRuntimeEvents, ID: "a1", Fields: fields}
	got, err := canonicalSessionOperation(req)
	if err != nil {
		t.Fatal(err)
	}
	want := core.Action{Action: core.ActionQuery, Query: core.QueryRuntimeEvents, ID: "a1", Fields: map[string]string{core.RuntimeEventsAfterSequenceField: "9"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("action = %+v, want %+v", got, want)
	}
	fields[core.RuntimeEventsAfterSequenceField] = "10"
	if got.Fields[core.RuntimeEventsAfterSequenceField] != "9" {
		t.Fatalf("canonical query aliased caller fields: %+v", got)
	}
}
