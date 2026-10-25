package daemon

import (
	"amux/internal/accounts"
	"amux/internal/core"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"testing"
)

func TestAccountOverviewPreservesLegacyDefaultsBindingsAndReadiness(t *testing.T) {
	d := isolatedAccounts(t)
	if err := accounts.Update(func(s *accounts.State) error {
		s.Defaults["claude"] = "claude:host"
		s.Defaults["codex"] = "codex:environment"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := accounts.Bind("existing", "claude"); err != nil {
		t.Fatal(err)
	}
	before, err := accounts.Read()
	if err != nil {
		t.Fatal(err)
	}
	out := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountStatus}, false)
	if !out.OK || out.Snapshot == nil || len(out.Snapshot.Profiles) != 2 {
		t.Fatalf("overview: %+v", out)
	}
	for _, p := range out.Snapshot.Profiles {
		if p.ID != p.Harness+":central" || p.Source != "central" {
			t.Fatalf("legacy config exposed: %+v", p)
		}
	}
	if out.Snapshot.Defaults["claude"] != "claude:host" || out.Snapshot.Defaults["codex"] != "codex:environment" {
		t.Fatal("legacy routing hidden")
	}
	internal, err := d.accountSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	harnesses := creationHarnesses(core.Action{Action: core.ActionNewWorkgroup, Fields: map[string]string{"coordinator": "codex", "agent": "claude", "repos": "repo"}})
	if len(harnesses) != 2 {
		t.Fatal("fixture must check coordinator and worker")
	}
	if err := checkAccountReadiness(internal, harnesses); err != nil {
		t.Fatal("internal readiness lost legacy defaults", err)
	}
	after, err := accounts.Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Bindings["existing"] != before.Bindings["existing"] || after.Defaults["claude"] != before.Defaults["claude"] || after.DefaultRevisions["claude"] != before.DefaultRevisions["claude"] {
		t.Fatal("view changed binding/default")
	}
	// Central selection is an explicit local CAS mutation and affects only new bindings.
	central := after.Profiles["claude:central"]
	result := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountDefault, ProfileID: central.ID, Revision: central.Revision, DefaultRevision: after.DefaultRevisions["claude"]}, false)
	if !result.OK {
		t.Fatal(result)
	}
	if err := accounts.Bind("new", "claude"); err != nil {
		t.Fatal(err)
	}
	after, err = accounts.Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Bindings["existing"] != before.Bindings["existing"] || after.Bindings["new"].ProfileID != "claude:central" {
		t.Fatal("local adoption rerouted existing workload")
	}
}

func TestRepoWorkgroupCreationChecksCoordinatorAndMemberAccounts(t *testing.T) {
	for _, action := range []string{core.ActionCreateWorkspace, core.ActionNewWorkgroup} {
		harnesses := creationHarnesses(core.Action{Action: action, Fields: map[string]string{
			"repos": "api", "defaultAgent": "1", "coordinator": "codex", "agent": "claude",
		}})
		if len(harnesses) != 2 || harnesses[0] != "codex" || harnesses[1] != "claude" {
			t.Fatalf("%s: %v", action, harnesses)
		}
	}
	if got := creationHarnesses(core.Action{Action: core.ActionNewRepoAgent}); len(got) != 0 {
		t.Fatalf("retired action still invokes account readiness: %v", got)
	}
}
