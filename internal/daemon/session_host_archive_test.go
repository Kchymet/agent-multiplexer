package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/access"
	"amux/internal/amuxcfg"
	"amux/internal/core"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

// hostArchiveFixture: coordinator wg1 (member a1) and a foreign workgroup wg2
// (members b1, b2), plus a repo home. The rail snapshot is seeded directly so
// each test controls live activity.
func hostArchiveFixture(t *testing.T) (*Daemon, func(who, verb, id string, fields map[string]string) sessionrpc.DispatchResult) {
	t.Helper()
	wg1 := store.Session{ID: "wg1", Name: "one", Agent: "claude", Scope: store.ScopeWork}
	a1 := store.Session{ID: "a1", RootID: "wg1", Name: "a1", Agent: "claude"}
	wg2 := store.Session{ID: "wg2", Name: "two", Agent: "claude", Scope: store.ScopeWork}
	b1 := store.Session{ID: "b1", RootID: "wg2", Name: "b1", Agent: "claude"}
	b2 := store.Session{ID: "b2", RootID: "wg2", Name: "b2", Agent: "claude", Created: 12345}
	api := store.Session{ID: "api", Name: "api", Agent: "claude", Scope: store.ScopeRepo, Repo: "api"}
	d, runtime, principals := sessionRuntimeFixture(t, wg1, a1, wg2, b1, b2, api)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	setRail(d, map[string]string{"wg1": core.StateReady, "a1": core.StateIdle, "wg2": core.StateIdle,
		"b1": core.StateIdle, "b2": core.StateIdle, "api": core.StateIdle})
	n := 0
	call := func(who, verb, id string, fields map[string]string) sessionrpc.DispatchResult {
		t.Helper()
		n++
		route := access.RouteAction
		if verb == core.QueryHostSessions {
			route = access.RouteQuery
		}
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
	return d, call
}

func setRail(d *Daemon, states map[string]string) {
	roots := map[string]string{"a1": "wg1", "b1": "wg2", "b2": "wg2"}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessions = nil
	for id, state := range states {
		d.sessions = append(d.sessions, core.Session{ID: id, RootID: roots[id], IsRoot: roots[id] == "", State: state})
	}
}

func archived(t *testing.T, id string) bool {
	t.Helper()
	db, err := store.OpenReadOnly()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
	if err != nil || !ok {
		t.Fatalf("load %s: ok=%v err=%v", id, ok, err)
	}
	return s.Archived
}

var archiveTrue = map[string]string{"archived": "true"}

func TestHostArchiveDefaultDeniesForeignArchive(t *testing.T) {
	_, call := hostArchiveFixture(t)
	if got := call("wg1", core.ActionSetArchived, "b1", archiveTrue); got.Status != sessionrpc.StatusDenied {
		t.Fatalf("default foreign archive = %+v, want denied", got)
	}
	if archived(t, "b1") {
		t.Fatal("b1 archived under the default members scope")
	}
	got := call("wg1", core.QueryHostSessions, "", nil)
	var view core.HostSessionsView
	if got.Status != sessionrpc.StatusOK || json.Unmarshal(got.Body, &view) != nil || view.Enabled || len(view.Sessions) != 0 {
		t.Fatalf("default host-sessions = %+v (%s), want empty and disabled", got, got.Body)
	}
}

func TestHostArchiveAllowsIdleForeignSessionAndAudits(t *testing.T) {
	d, call := hostArchiveFixture(t)
	if err := amuxcfg.SetCoordinatorArchiveScope(amuxcfg.ArchiveScopeHost); err != nil {
		t.Fatal(err)
	}
	if got := call("wg1", core.ActionSetArchived, "b1", archiveTrue); got.Status != sessionrpc.StatusOK {
		t.Fatalf("host-scope foreign archive = %+v, want ok", got)
	}
	if !archived(t, "b1") {
		t.Fatal("b1 not archived")
	}
	b, err := os.ReadFile(HostArchiveAuditPath())
	if err != nil {
		t.Fatal(err)
	}
	var entry hostArchiveAudit
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &entry); err != nil {
		t.Fatalf("audit %q: %v", b, err)
	}
	if entry.Coordinator != "wg1" || entry.Target != "b1" || entry.TargetRoot != "wg2" || entry.At == "" {
		t.Fatalf("audit entry = %+v", entry)
	}
	// Restore, delete, kill, steer and rename on the foreign session stay denied.
	for _, tc := range []struct {
		verb, id string
		fields   map[string]string
	}{
		{core.ActionSetArchived, "b1", map[string]string{"archived": "false"}},
		{core.ActionDelete, "b2", nil},
		{core.ActionKill, "b2", nil},
		{core.ActionSteer, "b2", map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: "hi"}},
		{core.ActionRename, "b2", map[string]string{"name": "x"}},
		{core.ActionArchive, "b2", nil},
	} {
		if got := call("wg1", tc.verb, tc.id, tc.fields); got.Status == sessionrpc.StatusOK {
			t.Errorf("%s %s = %+v, want refused", tc.verb, tc.id, got)
		}
	}
	if archived(t, "b2") {
		t.Fatal("b2 changed by a refused operation")
	}
	// A whole idle foreign workgroup may be archived; it fences its members.
	setRail(d, map[string]string{"wg2": core.StateIdle, "b2": core.StateIdle, "api": core.StateIdle})
	if got := call("wg1", core.ActionSetArchived, "wg2", archiveTrue); got.Status != sessionrpc.StatusOK {
		t.Fatalf("host-scope foreign workgroup archive = %+v, want ok", got)
	}
	if !archived(t, "wg2") {
		t.Fatal("wg2 not archived")
	}
	// Repo homes are never targets; ordinary members never gain the scope.
	if got := call("wg1", core.ActionSetArchived, "api", archiveTrue); got.Status != sessionrpc.StatusDenied {
		t.Fatalf("repo home archive = %+v, want denied", got)
	}
	if got := call("a1", core.ActionSetArchived, "b2", archiveTrue); got.Status != sessionrpc.StatusDenied {
		t.Fatalf("member foreign archive = %+v, want denied", got)
	}
}

func TestHostArchiveRefusesActiveTargets(t *testing.T) {
	d, call := hostArchiveFixture(t)
	if err := amuxcfg.SetCoordinatorArchiveScope(amuxcfg.ArchiveScopeHost); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		states map[string]string
		id     string
		code   string
	}{
		{"running member", map[string]string{"b1": core.StateRunning, "b2": core.StateIdle, "wg2": core.StateRunning}, "b1", "archive_target_running"},
		{"pending permission", map[string]string{"b1": core.StateWaiting, "b2": core.StateIdle, "wg2": core.StateWaiting}, "b1", "archive_target_waiting"},
		{"workgroup with a running member", map[string]string{"b1": core.StateIdle, "b2": core.StateRunning, "wg2": core.StateIdle}, "wg2", "archive_target_running"},
		{"absent from the rail", map[string]string{"b2": core.StateIdle, "wg2": core.StateIdle}, "b1", "archive_target_state_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setRail(d, tc.states)
			got := call("wg1", core.ActionSetArchived, tc.id, archiveTrue)
			if got.Status != sessionrpc.StatusDenied || got.Code != tc.code {
				t.Fatalf("archive %s = %+v, want denied %s", tc.id, got, tc.code)
			}
			if archived(t, tc.id) {
				t.Fatalf("%s archived while active", tc.id)
			}
		})
	}

	// The staleness view reports the same verdicts and omits own workgroup.
	setRail(d, map[string]string{"wg1": core.StateReady, "a1": core.StateIdle, "wg2": core.StateRunning,
		"b1": core.StateRunning, "b2": core.StateIdle, "api": core.StateIdle})
	got := call("wg1", core.QueryHostSessions, "", nil)
	var view core.HostSessionsView
	if got.Status != sessionrpc.StatusOK || json.Unmarshal(got.Body, &view) != nil || !view.Enabled {
		t.Fatalf("host-sessions = %+v (%s)", got, got.Body)
	}
	rows := map[string]core.HostSessionRow{}
	for _, row := range view.Sessions {
		rows[row.ID] = row
	}
	if _, ok := rows["wg1"]; ok {
		t.Error("own workgroup listed in the host view")
	}
	if _, ok := rows["a1"]; ok {
		t.Error("own member listed in the host view")
	}
	if r := rows["b1"]; r.Archivable || r.Reason != "archive_target_running" || r.RootID != "wg2" || r.State != core.StateRunning {
		t.Errorf("b1 row = %+v", r)
	}
	if r := rows["b2"]; !r.Archivable || r.LastActivity != 12345 {
		t.Errorf("b2 row = %+v", r)
	}
	if r := rows["wg2"]; r.Archivable || r.Members != 2 || r.Role != store.RoleCoordinator {
		t.Errorf("wg2 row = %+v", r)
	}
	if r := rows["api"]; r.Archivable || r.Reason != "role_not_archivable" {
		t.Errorf("api row = %+v", r)
	}
	// Turning the setting back off revokes immediately.
	if err := amuxcfg.SetCoordinatorArchiveScope(""); err != nil {
		t.Fatal(err)
	}
	setRail(d, map[string]string{"wg2": core.StateIdle, "b1": core.StateIdle, "b2": core.StateIdle})
	if got := call("wg1", core.ActionSetArchived, "b2", archiveTrue); got.Status != sessionrpc.StatusDenied || got.Code != "access_denied" {
		t.Fatalf("archive after reset = %+v, want access_denied", got)
	}
}
