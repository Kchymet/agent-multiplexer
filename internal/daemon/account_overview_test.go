package daemon

import (
	"amux/internal/accounts"
	"amux/internal/core"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"reflect"
	"testing"
)

func TestAccountOverviewIsCentralOnlyAndRetiresLegacyDefaults(t *testing.T) {
	d := isolatedAccounts(t)
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
	if out.Snapshot.Defaults["claude"] != "claude:central" || out.Snapshot.Defaults["codex"] != "codex:central" {
		t.Fatalf("defaults: %+v", out.Snapshot.Defaults)
	}
	after, err := accounts.Read()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Bindings["existing"], before.Bindings["existing"]) || after.DefaultRevisions["claude"] != before.DefaultRevisions["claude"] {
		t.Fatal("view changed binding/default")
	}
	// A legacy source can no longer be selected; central remains selectable.
	host := after.Profiles["claude:host"]
	result := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountDefault, ProfileID: host.ID, Revision: host.Revision, DefaultRevision: after.DefaultRevisions["claude"]}, false)
	if result.OK || result.Error != "account_legacy_source_retired" {
		t.Fatalf("legacy default accepted: %+v", result)
	}
	central := after.Profiles["claude:central"]
	if result := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountDefault, ProfileID: central.ID, Revision: central.Revision, DefaultRevision: after.DefaultRevisions["claude"]}, false); !result.OK {
		t.Fatal(result)
	}
	if got, _ := accounts.Read(); got.Defaults["claude"] != "claude:central" {
		t.Fatal("central default lost")
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
	if got := creationHarnesses(core.Action{Action: "new-repo-agent"}); len(got) != 0 {
		t.Fatalf("retired action still invokes account readiness: %v", got)
	}
}
