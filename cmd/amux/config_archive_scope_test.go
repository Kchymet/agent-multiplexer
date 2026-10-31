package main

import (
	"os"
	"strings"
	"testing"

	"amux/internal/amuxcfg"
	"amux/internal/core"
)

func TestConfigCoordinatorArchiveScopeHost(t *testing.T) {
	sandboxCLI(t)
	out, err := captureOutput(t, func() error { return cmdConfig([]string{"ls"}) })
	if err != nil || !strings.Contains(out, "coordinator.archive_scope members  (default;") || !strings.Contains(out, "host-only setting") {
		t.Fatalf("ls: %q, %v", out, err)
	}
	if _, err := captureOutput(t, func() error { return cmdConfig([]string{"set", "coordinator.archive_scope", "host"}) }); err != nil {
		t.Fatal(err)
	}
	if out, err := captureOutput(t, func() error { return cmdConfig([]string{"get", "coordinator.archive_scope"}) }); err != nil || strings.TrimSpace(out) != "host" {
		t.Fatalf("get: %q, %v", out, err)
	}
	if _, err := captureOutput(t, func() error { return cmdConfig([]string{"set", "coordinator.archive_scope", "all"}) }); err == nil {
		t.Fatal("invalid value accepted")
	}
	if _, err := captureOutput(t, func() error { return cmdConfig([]string{"unset", "coordinator.archive_scope"}) }); err != nil {
		t.Fatal(err)
	}
	if amuxcfg.HostArchiveScope() {
		t.Fatal("unset left host scope enabled")
	}
}

// TestConfigCoordinatorArchiveScopeRefusedInAgent: an agent session can never
// flip (or read) the host-only switch, and nothing is written.
func TestConfigCoordinatorArchiveScopeRefusedInAgent(t *testing.T) {
	sandboxCLI(t)
	t.Setenv("AMUX_WORKGROUP", "wg1")
	for _, args := range [][]string{
		{"set", "coordinator.archive_scope", "host"},
		{"unset", "coordinator.archive_scope"},
		{"get", "coordinator.archive_scope"},
	} {
		_, err := captureOutput(t, func() error { return cmdConfig(args) })
		if err == nil || !strings.Contains(err.Error(), "host-only setting") {
			t.Fatalf("%v from an agent = %v, want host-only refusal", args, err)
		}
	}
	if _, err := os.Stat(core.ConfigPath()); !os.IsNotExist(err) {
		t.Fatalf("config file written from an agent session: %v", err)
	}
}

func TestRestrictedArchiveDirectionNeverRestores(t *testing.T) {
	got := restrictedArchiveDirection(core.Action{Action: core.ActionArchive, ID: "b1"})
	if got.Action != core.ActionSetArchived || got.Fields["archived"] != "true" || got.ID != "b1" {
		t.Fatalf("archive toggle from an agent = %+v, want explicit archive", got)
	}
	other := core.Action{Action: core.ActionRename, ID: "b1", Fields: map[string]string{"name": "x"}}
	if got := restrictedArchiveDirection(other); got.Action != core.ActionRename {
		t.Fatalf("non-archive action rewritten: %+v", got)
	}
}
