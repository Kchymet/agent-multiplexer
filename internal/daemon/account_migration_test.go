package daemon

import (
	"amux/internal/accounts"
	"amux/internal/centralauth"
	"amux/internal/store"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func putClaudeSession(t *testing.T, id string) {
	t.Helper()
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PutSession(store.Session{ID: id, Agent: "claude", Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

func acceptClaude(id string, l accounts.Launch, generation uint64) error {
	b, p, err := accounts.Delivery(id, "claude")
	if err != nil {
		return err
	}
	return accounts.Accept(id, b.Revision, p.DeliveryEpoch, "", generation, l)
}

// A pre-feature central credential stays usable for the running process, and
// a replacement login serves the workload's next launch without any rebind.
func TestPreFeatureClaudeCredentialFollowsReplacementAtNextLaunch(t *testing.T) {
	d := isolatedAccounts(t)
	secret, err := centralauth.NewSecret("PRE-FEATURE-SYNTHETIC-TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	original, err := centralauth.ClaudeStore().Save(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	putClaudeSession(t, "old")
	running, err := accounts.BeginLaunch("old", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if err := acceptClaude("old", running, original.Generation); err != nil {
		t.Fatal("pre-feature credential not delivered", err)
	}
	completeClaude(t, d, beginClaude(t, d, "owner"))
	fresh, err := centralauth.ClaudeStore().Load()
	if err != nil || fresh.Generation == original.Generation {
		t.Fatal("fixture did not replace credential", err)
	}
	if err := acceptClaude("old", running, fresh.Generation); !errors.Is(err, accounts.ErrSuperseded) {
		t.Fatal("running process adopted the replacement", err)
	}
	next, _ := accounts.BeginLaunch("old", "claude")
	if err := acceptClaude("old", next, fresh.Generation); err != nil {
		t.Fatal("next launch refused the replacement", err)
	}
}

// A corrupt pre-feature record never blocks the repairing login; nothing is
// delivered from it, and after login the same workload's next launch delivers.
func TestCorruptClaudeRecordAllowsRepairLogin(t *testing.T) {
	d := isolatedAccounts(t)
	dir := centralauth.ClaudeStore().Dir
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credential.json"), []byte("corrupt legacy credential"), 0600); err != nil {
		t.Fatal(err)
	}
	putClaudeSession(t, "old")
	if _, err := centralauth.ClaudeStore().Load(); err == nil {
		t.Fatal("fixture is readable")
	}
	completeClaude(t, d, beginClaude(t, d, "owner"))
	credential, err := centralauth.ClaudeStore().Load()
	if err != nil {
		t.Fatal(err)
	}
	launch, _ := accounts.BeginLaunch("old", "claude")
	if err := acceptClaude("old", launch, credential.Generation); err != nil {
		t.Fatal("repaired login not delivered to the next launch", err)
	}
}

// A workload created before the first login receives it without rebind.
func TestWorkloadBeforeFirstLoginReceivesIt(t *testing.T) {
	d := isolatedAccounts(t)
	putClaudeSession(t, "new")
	launch, _ := accounts.BeginLaunch("new", "claude")
	completeClaude(t, d, beginClaude(t, d, "owner"))
	credential, err := centralauth.ClaudeStore().Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := acceptClaude("new", launch, credential.Generation); err != nil {
		t.Fatal("waiting launch could not receive the first login", err)
	}
}
