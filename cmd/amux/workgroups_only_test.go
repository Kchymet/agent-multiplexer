package main

import (
	"encoding/json"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/sessionrpc"
)

// `amux workgroup move` sends the move action and, with no target, nothing but
// the agent — the daemon makes the new workgroup and reports its id.
func TestWorkgroupMoveSendsMoveAction(t *testing.T) {
	body, _ := json.Marshal(core.Result{Type: "result", OK: true, NewID: "fresh"})
	rpc := &fakeRestrictedRPC{action: sessionrpc.Result{Status: sessionrpc.StatusOK, Body: body}}
	installRestrictedRPC(t, rpc)
	if err := cmdSession([]string{"move", "a1", "--new"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdSession([]string{"move", "a1", "wg2"}); err != nil {
		t.Fatal(err)
	}
	if len(rpc.actions) != 2 {
		t.Fatalf("actions: %+v", rpc.actions)
	}
	if a := rpc.actions[0]; a.Verb != core.ActionMove || a.ID != "a1" || a.Target != "" {
		t.Fatalf("--new move = %+v", a)
	}
	if a := rpc.actions[1]; a.Verb != core.ActionMove || a.ID != "a1" || a.Target != "wg2" {
		t.Fatalf("targeted move = %+v", a)
	}
}

// The retired one-off verb is no longer scriptable: `amux do new-repo-agent`
// fails locally with the vocabulary, which no longer lists it.
func TestDoRejectsRetiredOneOffVerb(t *testing.T) {
	err := cmdDo([]string{"new-repo-agent", "api"})
	if err == nil || !strings.Contains(err.Error(), "valid actions:") || !strings.Contains(err.Error(), "new-workgroup") {
		t.Fatalf("new-repo-agent = %v, want the vocabulary", err)
	}
	for _, line := range strings.Split(err.Error(), "\n") {
		if strings.Contains(line, "new-repo-agent") && !strings.Contains(line, "unknown action") {
			t.Fatalf("vocabulary still lists the retired verb: %q", line)
		}
	}
	for _, verb := range core.ControlActions() {
		if verb == "new-repo-agent" {
			t.Fatal("new-repo-agent is still a control action")
		}
	}
}

// `workgroup ls` shows each workgroup's repository ceiling, so a workgroup
// limited to one repo reads as such; a legacy coordinator with no ceiling
// shows none rather than a guess.
func TestGrantsTag(t *testing.T) {
	cases := []struct {
		row  core.WorkgroupRow
		want string
	}{
		{core.WorkgroupRow{Granted: true, Grants: []string{"api"}}, " · repos: api"},
		{core.WorkgroupRow{Granted: true, Grants: []string{"api", "web"}}, " · repos: api, web"},
		{core.WorkgroupRow{Granted: true}, " · repos: (none)"},
		{core.WorkgroupRow{}, ""},
	}
	for _, c := range cases {
		if got := grantsTag(c.row); got != c.want {
			t.Errorf("grantsTag(%+v) = %q, want %q", c.row, got, c.want)
		}
	}
}

// The user-facing help no longer speaks of one-off agents or of work-scoped vs
// repo-scoped workgroups: every container of agents is a workgroup.
func TestHelpHasNoOneOffVocabulary(t *testing.T) {
	out, _ := captureOutput(t, func() error { usage(); workgroupUsage(); doUsage(); return nil })
	for _, banned := range []string{"one-off", "work-scoped", "repo-scoped", "new-repo-agent"} {
		if strings.Contains(out, banned) {
			t.Errorf("help still says %q", banned)
		}
	}
	for _, want := range []string{"workgroup repo <r>", "limited to one tracked repo", "move an agent into another (or a new) workgroup"} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}
