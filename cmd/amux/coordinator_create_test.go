package main

import (
	"strings"
	"testing"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/wsops"
)

// The CLI's creation fields must keep the coordinator's runtime separate from a
// member's: a `--model` chosen for a Claude worker has no meaning on the Codex
// coordinator, so it must never be forwarded as the coordinator's model.
func TestCreateFieldsKeepCoordinatorAndWorkerSeparate(t *testing.T) {
	repos, cfg := parseCreateFlags([]string{"acme/api", "--agent", "claude", "--model", "claude-opus-5", "--prompt", "ship it"})
	fields := createWorkspaceFields(repos, cfg)
	if _, ok := fields[wsops.FieldCoordinator]; ok {
		t.Fatalf("an unrequested coordinator runtime was sent: %q", fields[wsops.FieldCoordinator])
	}
	if _, ok := fields[wsops.FieldCoordinatorModel]; ok {
		t.Fatalf("a worker's model leaked to the coordinator: %q", fields[wsops.FieldCoordinatorModel])
	}
	if fields["agent"] != "claude" || fields["model"] != "claude-opus-5" {
		t.Fatalf("worker fields = %q/%q, want claude/claude-opus-5", fields["agent"], fields["model"])
	}
	if fields["prompt"] != "ship it" {
		t.Fatalf("task = %q, want the coordinator's task", fields["prompt"])
	}
	// Absent from the request, the daemon picks the default coordinator runtime.
	if got := wsops.CoordinatorKind(fields[wsops.FieldCoordinator]); got != agent.GoalRuntime {
		t.Fatalf("default coordinator = %q, want %q", got, agent.GoalRuntime)
	}
}

// An explicitly chosen coordinator runtime and model are forwarded as such.
func TestCreateFieldsCarryExplicitCoordinator(t *testing.T) {
	for _, args := range [][]string{
		{"acme/api", "--coordinator", "claude", "--coordinator-model", "claude-opus-5"},
		{"acme/api", "--coordinator=claude", "--coordinator-model=claude-opus-5"},
	} {
		repos, cfg := parseCreateFlags(args)
		fields := createWorkspaceFields(repos, cfg)
		if fields[wsops.FieldCoordinator] != "claude" || fields[wsops.FieldCoordinatorModel] != "claude-opus-5" {
			t.Fatalf("%v: coordinator fields = %q/%q", args, fields[wsops.FieldCoordinator], fields[wsops.FieldCoordinatorModel])
		}
		if got := wsops.CoordinatorKind(fields[wsops.FieldCoordinator]); got != "claude" {
			t.Fatalf("%v: resolved coordinator = %q", args, got)
		}
	}
}

// The interactive page cycles the coordinator runtime through the registry and
// always resolves to a real kind, starting from the goal runtime.
func TestPickCoordinatorKindCyclesRegistry(t *testing.T) {
	kinds := agent.Kinds()
	seen := map[string]bool{}
	current := ""
	for range kinds {
		current = pickCoordinatorKind(current)
		if !agent.Known(current) {
			t.Fatalf("picked an unknown coordinator kind %q", current)
		}
		seen[current] = true
	}
	if len(seen) != len(kinds) {
		t.Fatalf("cycled through %d kinds, want all %d", len(seen), len(kinds))
	}
	if got := wsops.CoordinatorKind(""); got != agent.GoalRuntime {
		t.Fatalf("the page's starting value resolves to %q, want %q", got, agent.GoalRuntime)
	}
}

// `amux do` must teach the coordinator fields and the goal verb, since they are
// how a scripted or remote caller reaches goal mode at all.
func TestDoHelpTeachesGoalVocabulary(t *testing.T) {
	for verb, want := range map[string][]string{
		core.ActionNewWorkgroup: {"coordinator"},
		core.ActionSteer:        {"goal", "status=", "token_budget"},
	} {
		help := actionGlosses[verb]
		for _, w := range want {
			if !strings.Contains(help, w) {
				t.Errorf("`amux do %s` help does not mention %q: %s", verb, w, help)
			}
		}
	}
}
