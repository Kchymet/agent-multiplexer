package daemon

import (
	"amux/internal/accounts"
	"amux/internal/centralauth"
	"amux/internal/store"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Expected to fail on affcf94: migration seeds the source but leaves the
// credential generation unknown even when the pre-feature token is available.
func TestAccountMigrationPinsPreFeatureClaudeCredential(t *testing.T) {
	d := isolatedAccounts(t)
	secret, err := centralauth.NewSecret("PRE-FEATURE-SYNTHETIC-TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	original, err := centralauth.ClaudeStore().Save(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	err = db.PutSession(store.Session{ID: "old", Agent: "claude", Dir: t.TempDir()})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	before, err := accounts.Read()
	if err != nil {
		t.Fatal(err)
	}
	// Force the current implementation's migration seam before replacement.
	out := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountDefault, ProfileID: "claude:central", Revision: before.Profiles["claude:central"].Revision, DefaultRevision: before.DefaultRevisions["claude"]}, false)
	if !out.OK {
		t.Fatal(out)
	}
	r := beginClaude(t, d, "owner")
	completeClaude(t, d, r)
	fresh, err := centralauth.ClaudeStore().Load()
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Generation == original.Generation {
		t.Fatal("fixture did not replace credential")
	}
	b, pf, err := accounts.Delivery("old", "claude")
	if err != nil {
		return
	} // Explicit blocked migration is safe.
	if err = accounts.Accept("old", b.Revision, pf.DeliveryEpoch, "", fresh.Generation); err == nil {
		t.Fatal("pre-feature workload silently accepted replacement opaque credential")
	}
}

func TestUnreadableMigrationAllowsOnlyExplicitRebindAfterLogin(t *testing.T) {
	d := isolatedAccounts(t)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	err = db.PutSession(store.Session{ID: "old", Agent: "claude", Dir: t.TempDir()})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	dir := centralauth.ClaudeStore().Dir
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "credential.json"), []byte("corrupt legacy credential"), 0600); err != nil {
		t.Fatal(err)
	}
	r := beginClaude(t, d, "owner")
	completeClaude(t, d, r)
	if _, _, err = accounts.Delivery("old", "claude"); err == nil {
		t.Fatal("old workload accepted replacement")
	}
	out := d.accountCall(t.Context(), hp.AccountRequest{Operation: hp.AccountRebind, ProfileID: "claude:central", Revision: profileRevision(t, "claude:central"), WorkloadID: "old"}, false)
	if !out.OK {
		t.Fatal(out)
	}
	b, p, err := accounts.Delivery("old", "claude")
	if err != nil {
		t.Fatal(err)
	}
	credential, err := centralauth.ClaudeStore().Load()
	if err != nil {
		t.Fatal(err)
	}
	if err = accounts.Accept("old", b.Revision, p.DeliveryEpoch, "", credential.Generation); err != nil {
		t.Fatal(err)
	}
}
