package direct

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The accounts-v1 fixtures are the additive contract for machine account
// status. They live beside docs/direct/fixtures/v1 and workflow-v1, whose
// manifests are unchanged. Regenerate with
// UPDATE_DIRECT_FIXTURES=1 go test ./internal/direct -run TestAccountsFixtures.
const accountsFixtureDir = "../../docs/direct/fixtures/accounts-v1"

func accountsFixtureFiles(t *testing.T) map[string][]byte {
	t.Helper()
	js := func(v any) []byte { return append(mustIndent(t, v), '\n') }
	id := fixtureIdentity(t)
	yes, no := true, false
	return map[string][]byte{
		"accounts-response.json": js(projectAccounts(id.ServerID, fullSnapshot())),
		"capabilities-account-status-true.json": js(CapabilitiesResponse{ServerID: id.ServerID,
			Permissions: []string{PermRead, PermAccounts}, ResourceScope: ResourceScope{Kind: ResourceAllSessions},
			ServerVerbs: ServerVerbs(), Verbs: []string{}, AccountStatus: &yes}),
		"capabilities-account-status-false.json": js(CapabilitiesResponse{ServerID: id.ServerID,
			Permissions: []string{PermRead, PermAccounts}, ResourceScope: ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"771951"}},
			ServerVerbs: ServerVerbs(), Verbs: []string{}, AccountStatus: &no}),
		"session-account-binding.json": js(Session{ID: "e1deda", Title: "Backend implementation", RootID: "771951",
			Section: "workgroups", Runtime: "claude", Caps: SessionCaps{},
			AccountBinding: &SessionAccountBinding{ProfileID: "claude:central", Source: "central"}}),
		"api-error-accounts-denied.json": js(APIError{Code: CodeDenied, Error: "machine account status needs an all-sessions client"}),
	}
}

func TestAccountsFixtures(t *testing.T) {
	want := accountsFixtureFiles(t)
	want["SHA256SUMS"] = sums(want)
	if os.Getenv("UPDATE_DIRECT_FIXTURES") == "1" {
		if err := os.MkdirAll(accountsFixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for n, b := range want {
			if err := os.WriteFile(filepath.Join(accountsFixtureDir, n), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, err := os.ReadDir(accountsFixtureDir)
	if err != nil {
		t.Fatalf("missing accounts fixtures (regenerate with UPDATE_DIRECT_FIXTURES=1): %v", err)
	}
	if len(entries) != len(want) {
		t.Errorf("accounts fixture dir has %d files, contract has %d", len(entries), len(want))
	}
	for n, b := range want {
		got, err := os.ReadFile(filepath.Join(accountsFixtureDir, n))
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("%s drifted from the wire types: %v", n, err)
		}
	}
}
