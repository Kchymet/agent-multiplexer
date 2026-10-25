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

// Expected to fail on 33bd444: unreadable legacy credentials must not become
// permission to silently adopt a replacement opaque account.
func TestAccountMigrationWithUnreadableCredentialCannotAdoptReplacement(t *testing.T) {
	d := isolatedAccounts(t)
	secret, err := centralauth.NewSecret("PRE-FEATURE-SYNTHETIC-TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	original, err := centralauth.ClaudeStore().Save(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(centralauth.ClaudeStore().Dir, "credential.json"), []byte("corrupt"), 0600); err != nil {
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

// A binding deliberately created for a new workload before its first login has
// no prior credential identity to preserve. This is distinct from adopting an
// already stored pre-feature workload with unreadable credential evidence.
func TestNewAccountBindingAwaitingInitialLoginCanReceiveCredential(t *testing.T) {
	d := isolatedAccounts(t)
	if err := accounts.Bind("new-awaiting-login", "claude"); err != nil {
		t.Fatal(err)
	}
	r := beginClaude(t, d, "owner")
	completeClaude(t, d, r)
	credential, err := centralauth.ClaudeStore().Load()
	if err != nil {
		t.Fatal(err)
	}
	b, p, err := accounts.Delivery("new-awaiting-login", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if err := accounts.Accept("new-awaiting-login", b.Revision, p.DeliveryEpoch, "", credential.Generation); err != nil {
		t.Fatal("new workload could not receive initial credential", err)
	}
}
