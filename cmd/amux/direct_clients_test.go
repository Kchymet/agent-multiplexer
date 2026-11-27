package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"amux/internal/direct"
)

func TestDirectClientsUpdateCLI(t *testing.T) {
	var got []direct.AdminRequest
	admin := func(req direct.AdminRequest) (direct.AdminResponse, error) {
		got = append(got, req)
		res := direct.GrantUpdateResult{Changed: true, Narrowed: true, CutOperations: 2,
			OldPermissions: []string{"read", "steer", "create"}, OldResource: direct.ResourceScope{Kind: direct.ResourceAllSessions},
			Dropped: []string{"create"},
			Client: direct.ClientInfo{ClientID: "c_1", Label: "Pixel", Permissions: []string{"read", "steer"},
				ResourceScope: direct.ResourceScope{Kind: direct.ResourceWorkgroups, Workgroups: []string{"wg1"}}}}
		return direct.AdminResponse{OK: true, Update: &res}, nil
	}
	var out bytes.Buffer
	if err := runDirectClients([]string{"update", "c_1", "--workgroup", "wg1"}, &out, admin); err != nil {
		t.Fatal(err)
	}
	u := got[0].Update
	if got[0].Op != direct.OpUpdate || u.ClientID != "c_1" || u.Permissions != nil || u.Resource == nil ||
		u.Resource.Kind != direct.ResourceWorkgroups || !slices.Equal(u.Resource.Workgroups, []string{"wg1"}) || !strings.HasPrefix(got[0].Actor, "cli") {
		t.Fatalf("request: %+v %+v", got[0], u)
	}
	for _, want := range []string{"was read,steer,create all_sessions", "now read,steer workgroups:wg1", "dropped create", "2 open stream(s)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	// Flags first, id last; permissions only.
	got = nil
	out.Reset()
	if err := runDirectClients([]string{"update", "--permissions", "read, approve", "c_2"}, &out, admin); err != nil {
		t.Fatal(err)
	}
	if u := got[0].Update; u.ClientID != "c_2" || !slices.Equal(u.Permissions, []string{"read", "approve"}) || u.Resource != nil {
		t.Fatalf("request: %+v", u)
	}
	for _, bad := range [][]string{
		{"update"},
		{"update", "c_1"},
		{"update", "c_1", "--all-sessions", "--workgroup", "x"},
		{"update", "c_1", "c_2", "--all-sessions"},
	} {
		if err := runDirectClients(bad, &out, admin); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
	// An older server: a clear upgrade hint.
	old := func(direct.AdminRequest) (direct.AdminResponse, error) {
		return direct.AdminResponse{Error: `unknown admin operation "update"`}, errors.New(`unknown admin operation "update"`)
	}
	if err := runDirectClients([]string{"update", "c_1", "--all-sessions"}, &out, old); err == nil || !strings.Contains(err.Error(), "predates grant updates") {
		t.Fatalf("old server: %v", err)
	}
}

func TestDirectClientsListStaleCLI(t *testing.T) {
	var got direct.AdminRequest
	admin := func(req direct.AdminRequest) (direct.AdminResponse, error) {
		got = req
		return direct.AdminResponse{OK: true, ServerID: "srv", Clients: []direct.ClientInfo{{ClientID: "c_1", Label: "Pixel", ClientKind: "android",
			Permissions: []string{"read"}, ResourceScope: direct.ResourceScope{Kind: direct.ResourceAllSessions}, Stale: "superseded by c_2",
			UpdatedAt: 1_800_000_000, Changes: []direct.GrantChange{{Source: "host", By: "cli:me", OldPerms: []string{"read", "steer"},
				OldResource: direct.ResourceScope{Kind: direct.ResourceAllSessions}}}}}}, nil
	}
	var out bytes.Buffer
	if err := runDirectClients([]string{"list", "--stale", "--idle-days", "30"}, &out, admin); err != nil {
		t.Fatal(err)
	}
	if got.Op != direct.OpList || !got.Stale || got.StaleDays != 30 {
		t.Fatalf("request: %+v", got)
	}
	for _, want := range []string{"stale: superseded by c_2", "by host:cli:me (was read,steer all_sessions)", "amux device revoke"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output lacks %q:\n%s", want, out.String())
		}
	}
	if err := runDirectClients([]string{"list", "--idle-days", "3"}, &out, admin); err == nil {
		t.Fatal("--idle-days without --stale accepted")
	}
	out.Reset()
	if err := runDirectClients([]string{"list", "--json"}, &out, admin); err != nil || got.Stale || !strings.HasPrefix(out.String(), "[") {
		t.Fatalf("json list: %v %+v %s", err, got, out.String())
	}
}

func TestDirectEnrollReplaceFlag(t *testing.T) {
	h := newEnrollHarness(t)
	if err := runDirectEnroll([]string{"--label", "phone", "--all-sessions", "--replace"}, h.env); err != nil {
		t.Fatal(err)
	}
	if len(h.requests) != 1 || !h.requests[0].Replace || !strings.Contains(h.stderr.String(), `Replacing: once redeemed, earlier clients labelled "phone"`) {
		t.Fatalf("replace: %+v\n%s", h.requests, h.stderr.String())
	}
	h = newEnrollHarness(t)
	if err := runDirectEnroll([]string{"--label", "phone", "--all-sessions"}, h.env); err != nil || h.requests[0].Replace {
		t.Fatalf("replace without the flag: %v %+v", err, h.requests)
	}
}
