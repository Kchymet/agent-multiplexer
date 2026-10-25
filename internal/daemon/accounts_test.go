package daemon

import (
	"amux/internal/accounts"
	"amux/internal/centralauth"
	"amux/internal/core"
	"context"
	"encoding/json"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func isolatedAccounts(t *testing.T) *Daemon {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	d := &Daemon{}
	t.Cleanup(func() { d.disconnectAccounts("", true) })
	return d
}
func profileRevision(t *testing.T, id string) uint64 {
	t.Helper()
	s, e := accounts.Read()
	if e != nil {
		t.Fatal(e)
	}
	return s.Profiles[id].Revision
}
func beginClaude(t *testing.T, d *Daemon, who string) hp.AccountRequest {
	t.Helper()
	r := hp.AccountRequest{Operation: hp.AccountLogin, ProfileID: "claude:central", Revision: profileRevision(t, "claude:central"), Initiator: who}
	out := d.accountCall(t.Context(), r, true)
	if !out.OK || out.Ceremony == nil {
		t.Fatalf("begin: %+v", out)
	}
	return hp.AccountRequest{ProfileID: r.ProfileID, AttemptID: out.Ceremony.AttemptID, Initiator: who}
}
func completeClaude(t *testing.T, d *Daemon, r hp.AccountRequest) {
	t.Helper()
	r.Operation = hp.AccountComplete
	r.Secret = "SYNTHETIC-PRIVATE-TOKEN"
	out := d.accountCall(t.Context(), r, true)
	if !out.OK {
		t.Fatalf("complete: %+v", out)
	}
}
func TestAccountCeremonySafeProjectionAndInitiator(t *testing.T) {
	d := isolatedAccounts(t)
	r := beginClaude(t, d, "browser-a")
	bad := r
	bad.Operation = hp.AccountPoll
	bad.Initiator = "browser-b"
	if out := d.accountCall(t.Context(), bad, true); out.OK {
		t.Fatal("another initiator polled ceremony")
	}
	completeClaude(t, d, r)
	out := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountStatus}, false)
	b, _ := json.Marshal(out)
	for _, secret := range []string{"SYNTHETIC-PRIVATE-TOKEN", "attemptId", "authUrl", "browser-a"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("safe projection leaked %s", secret)
		}
	}
	bad = hp.AccountRequest{Operation: hp.AccountStatus, Secret: "hidden"}
	if d.accountCall(t.Context(), bad, false).OK {
		t.Fatal("status accepted secret")
	}
}
func TestSuccessfulAttemptExpiryNeverBlocksProfile(t *testing.T) {
	d := isolatedAccounts(t)
	now := time.Now()
	d.accountNow = func() time.Time { return now }
	r := beginClaude(t, d, "local")
	completeClaude(t, d, r)
	s, _ := accounts.Read()
	before := s.Profiles[r.ProfileID]
	now = now.Add(codexLoginTimeout + time.Second)
	out := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountStatus}, false)
	if !out.OK {
		t.Fatal(out.Error)
	}
	s, _ = accounts.Read()
	after := s.Profiles[r.ProfileID]
	if after.State != "ready" || after.DeliveryEpoch != before.DeliveryEpoch {
		t.Fatalf("expiry disabled successful login: %+v", after)
	}
}
func TestPendingExpiryOnStatusFencesLateCompletion(t *testing.T) {
	d := isolatedAccounts(t)
	now := time.Now()
	d.accountNow = func() time.Time { return now }
	r := beginClaude(t, d, "local")
	now = now.Add(codexLoginTimeout + time.Second)
	d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountStatus}, false)
	s, _ := accounts.Read()
	if s.Profiles[r.ProfileID].State != "blocked" {
		t.Fatal("pending survived timeout")
	}
	r.Operation = hp.AccountComplete
	r.Secret = "LATE-TOKEN"
	if d.accountCall(t.Context(), r, true).OK {
		t.Fatal("late completion admitted")
	}
	if st, _ := centralauth.ClaudeStore().Status(); st.Configured {
		t.Fatal("late token persisted")
	}
}
func TestDisconnectRetiresUnknownAttemptAndLogoutFencesCompletion(t *testing.T) {
	d := isolatedAccounts(t)
	r := beginClaude(t, d, "browser")
	out := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountDisconnect, Initiator: "browser"}, true)
	if !out.OK {
		t.Fatal(out)
	}
	r.Operation = hp.AccountComplete
	r.Secret = "LATE-TOKEN"
	if d.accountCall(t.Context(), r, true).OK {
		t.Fatal("lost-response attempt survived disconnect")
	}
	r = beginClaude(t, d, "browser")
	out = d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountLogout, ProfileID: r.ProfileID, Revision: profileRevision(t, r.ProfileID)}, false)
	if !out.OK {
		t.Fatal(out)
	}
	r.Operation = hp.AccountComplete
	r.Secret = "LATE-TOKEN"
	if d.accountCall(t.Context(), r, true).OK {
		t.Fatal("late completion survived logout")
	}
	r = beginClaude(t, d, "browser")
	completeClaude(t, d, r)
	s, _ := accounts.Read()
	if s.Profiles[r.ProfileID].State != "ready" {
		t.Fatal("login after logout did not restore readiness")
	}
}
func TestClaudeCompleteLogoutRaceNeverResurrectsToken(t *testing.T) {
	d := isolatedAccounts(t)
	for range 12 {
		r := beginClaude(t, d, "local")
		r.Operation = hp.AccountComplete
		r.Secret = "RACING-TOKEN"
		revision := profileRevision(t, r.ProfileID)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); d.accountCall(context.Background(), r, true) }()
		go func() {
			defer wg.Done()
			d.accountCall(context.Background(), hp.AccountRequest{Operation: hp.AccountLogout, ProfileID: r.ProfileID, Revision: revision}, false)
		}()
		wg.Wait()
		s, _ := accounts.Read()
		if s.Profiles[r.ProfileID].State == "logged-out" {
			if _, e := centralauth.ClaudeStore().Load(); e == nil {
				t.Fatal("logout resurrected token")
			}
		}
	}
}
func TestAccountCorruptStoreBlocksModeAndLaunchBinding(t *testing.T) {
	d := isolatedAccounts(t)
	_ = d
	accounts.Bind("w", "codex")
	os.WriteFile(accounts.Path(), []byte("invalid"), 0600)
	if accounts.Mode("w", "codex") != "blocked" {
		t.Fatal("corrupt metadata fell back")
	}
	if accounts.Bind("w", "codex") == nil {
		t.Fatal("corrupt metadata admitted launch")
	}
}

func TestCreationReadinessChecksCoordinatorAndWorker(t *testing.T) {
	s := hp.AccountSnapshot{Defaults: map[string]string{"codex": "codex:central", "claude": "claude:central"}, Profiles: []hp.AccountProfile{{ID: "codex:central", State: "ready"}, {ID: "claude:central", State: "unconfigured"}}}
	a := core.Action{Action: core.ActionNewWorkgroup, Fields: map[string]string{"coordinator": "codex", "agent": "claude", "repos": "repo"}}
	if e := checkAccountReadiness(s, creationHarnesses(a)); e == nil {
		t.Fatal("unready worker ignored")
	}
	s.Profiles[1].State = "ready"
	s.Profiles[0].State = "blocked"
	if e := checkAccountReadiness(s, creationHarnesses(a)); e == nil {
		t.Fatal("unready coordinator ignored")
	}
	s.Profiles[0].State = "ready"
	if e := checkAccountReadiness(s, creationHarnesses(a)); e != nil {
		t.Fatal(e)
	}
}

func TestDifferentProfileDefaultsShareCAS(t *testing.T) {
	d := isolatedAccounts(t)
	before, _ := accounts.Read()
	a := hp.AccountRequest{Operation: hp.AccountDefault, ProfileID: "claude:host", Revision: before.Profiles["claude:host"].Revision, DefaultRevision: before.DefaultRevisions["claude"]}
	b := hp.AccountRequest{Operation: hp.AccountDefault, ProfileID: "claude:environment", Revision: before.Profiles["claude:environment"].Revision, DefaultRevision: a.DefaultRevision}
	if out := d.accountCall(t.Context(), a, false); !out.OK {
		t.Fatal(out)
	}
	if out := d.accountCall(t.Context(), b, false); out.OK || out.Error != "account_revision_changed" {
		t.Fatal(out)
	}
}

func TestCancelledClaudeReplacementRestoresReadyCredential(t *testing.T) {
	d := isolatedAccounts(t)
	first := beginClaude(t, d, "owner")
	completeClaude(t, d, first)
	prior, _ := centralauth.ClaudeStore().Load()
	r := beginClaude(t, d, "owner")
	r.Operation = hp.AccountCancel
	if out := d.accountCall(t.Context(), r, true); !out.OK {
		t.Fatal(out)
	}
	state, _ := accounts.Read()
	after, _ := centralauth.ClaudeStore().Load()
	if state.Profiles[r.ProfileID].State != "ready" || after.Generation != prior.Generation {
		t.Fatal("cancel lost prior ready credential")
	}
}

func TestClaudeLogoutReplacementCannotReuseOldBinding(t *testing.T) {
	d := isolatedAccounts(t)
	r := beginClaude(t, d, "owner")
	completeClaude(t, d, r)
	if err := accounts.Bind("old", "claude"); err != nil {
		t.Fatal(err)
	}
	b, p, err := accounts.Delivery("old", "claude")
	if err != nil {
		t.Fatal(err)
	}
	old, _ := centralauth.ClaudeStore().Load()
	if err = accounts.Accept("old", b.Revision, p.DeliveryEpoch, "", old.Generation); err != nil {
		t.Fatal(err)
	}
	if out := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountLogout, ProfileID: p.ID, Revision: profileRevision(t, p.ID)}, false); !out.OK {
		t.Fatal(out)
	}
	r = beginClaude(t, d, "owner")
	r.Operation = hp.AccountComplete
	r.Secret = "DIFFERENT-OPAQUE-ACCOUNT"
	if out := d.accountCall(t.Context(), r, true); !out.OK {
		t.Fatal(out)
	}
	fresh, _ := centralauth.ClaudeStore().Load()
	b, p, _ = accounts.Delivery("old", "claude")
	if fresh.Generation == old.Generation {
		t.Fatal("generation repeated")
	}
	if err = accounts.Accept("old", b.Revision, p.DeliveryEpoch, "", fresh.Generation); err == nil {
		t.Fatal("opaque replacement moved old workload")
	}
}

func TestDefaultMigrationPinsSyntheticConsoleBeforeChangingSource(t *testing.T) {
	d := isolatedAccounts(t)
	before, _ := accounts.Read()
	out := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountDefault, ProfileID: "claude:host", Revision: before.Profiles["claude:host"].Revision, DefaultRevision: before.DefaultRevisions["claude"]}, false)
	if !out.OK {
		t.Fatal(out)
	}
	after, _ := accounts.Read()
	if after.Bindings["console"].ProfileID != before.Defaults["claude"] || after.Defaults["claude"] != "claude:host" {
		t.Fatal("synthetic console changed with default", after.Bindings["console"])
	}
}
