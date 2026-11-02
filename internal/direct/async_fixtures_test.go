package direct

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The async-create-v1 fixtures are the additive contract for asynchronous
// creation (docs/direct-async-create.md). They live beside, never inside, the
// v1 and workflow-v1 sets. Regenerate with
// UPDATE_DIRECT_FIXTURES=1 go test ./internal/direct -run TestAsyncCreateFixtures.
const asyncFixtureDir = "../../docs/direct/fixtures/async-create-v1"

func asyncFixtureFiles(t *testing.T) map[string][]byte {
	t.Helper()
	js := func(v any) []byte { return append(mustIndent(t, v), '\n') }
	const id = "crt_q2b1T0xZbR3n9mWc7yVd8A"
	return map[string][]byte{
		"create-request-new-workgroup-async.json": js(ActionRequest{Verb: VerbNewWorkgroup, IdemKey: "b1f0c6a2-11", Async: true,
			Fields: map[string]string{"name": "Docs", "prompt": "Refresh the guides.", "repos": "harness", "agent": "claude"}}),
		"action-request-add-agent-async.json": js(ActionRequest{Verb: VerbAddAgent, IdemKey: "b1f0c6a2-12", Async: true,
			Fields: map[string]string{"agent": "codex", "mode": "task", "repos": "agent-multiplexer", "prompt": "Fix the flaky test."}}),
		"creation-status-queued.json":    js(CreationStatus{CreationID: id, Verb: VerbAddAgent, Phase: CreatePhaseQueued, Version: 1}),
		"creation-status-preparing.json": js(CreationStatus{CreationID: id, Verb: VerbAddAgent, Phase: CreatePhasePreparing, Version: 2, ElapsedMS: 41000}),
		"creation-status-ready.json": js(CreationStatus{CreationID: id, Verb: VerbAddAgent, Phase: CreatePhaseReady, Version: 3,
			NewID: "4c2a91", RootID: "771951", ElapsedMS: 93000}),
		"creation-status-failed-rejected.json": js(CreationStatus{CreationID: id, Verb: VerbAddAgent, Phase: CreatePhaseFailed, Version: 3,
			Code: CodeRejected, Error: "the daemon rejected the action", ElapsedMS: 12000}),
		"creation-status-failed-uncertain.json": js(CreationStatus{CreationID: id, Verb: VerbAddAgent, Phase: CreatePhaseFailed, Version: 3,
			Code: CodeIdemAmbiguous, Uncertain: true, ElapsedMS: 600000,
			Error: "the creation's outcome is unknown; the daemon may have created it. It will not run again with this idem_key; check the session list before retrying with a new one."}),
		"capabilities-response-async.json": js(CapabilitiesResponse{ServerID: fixtureIdentity(t).ServerID,
			Permissions: strs(PermRead, PermCreate), ResourceScope: ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"771951"}},
			ServerVerbs: ServerVerbs(), Verbs: strs(VerbAddAgent), AsyncCreate: true,
			Catalog: &CreationCatalog{Harnesses: []CatalogHarness{{Name: "codex", Version: "0.9.0"}}, IdentityModes: []string{"machine"},
				Modes: []string{"task", "interactive"}, Workgroups: []CatalogWorkgroup{{ID: "771951", Repos: []string{"agent-multiplexer"}}}}}),
		"api-error-rate-limited.json": js(APIError{Code: CodeRateLimited, Error: "too many creations are in progress; nothing was recorded, retry later"}),
	}
}

func TestAsyncCreateFixtures(t *testing.T) {
	want := asyncFixtureFiles(t)
	want["SHA256SUMS"] = sums(want)
	if os.Getenv("UPDATE_DIRECT_FIXTURES") == "1" {
		if err := os.MkdirAll(asyncFixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for n, b := range want {
			if err := os.WriteFile(filepath.Join(asyncFixtureDir, n), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, err := os.ReadDir(asyncFixtureDir)
	if err != nil {
		t.Fatalf("missing async fixtures (regenerate with UPDATE_DIRECT_FIXTURES=1): %v", err)
	}
	if len(entries) != len(want) {
		t.Errorf("async fixture dir has %d files, contract has %d", len(entries), len(want))
	}
	for n, b := range want {
		got, err := os.ReadFile(filepath.Join(asyncFixtureDir, n))
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("%s drifted from the wire types: %v", n, err)
		}
		if bytes.HasPrefix([]byte(n), []byte("creation-status-")) && bytes.Contains(b, []byte(`"ok"`)) {
			t.Errorf("%s carries an ok field", n)
		}
	}
	for n, verify := range map[string]func(ActionRequest) error{
		"create-request-new-workgroup-async.json": func(r ActionRequest) error { _, err := createActionFor(r); return err },
		"action-request-add-agent-async.json":     func(r ActionRequest) error { _, err := actionFor("771951", r); return err },
	} {
		var r ActionRequest
		if err := jsonStrict(want[n], &r); err != nil || !r.Async {
			t.Fatalf("%s: %v async=%v", n, err, r.Async)
		}
		if err := verify(r); err != nil {
			t.Fatalf("%s does not validate: %v", n, err)
		}
	}
	if !validCreationID("crt_q2b1T0xZbR3n9mWc7yVd8A") {
		t.Fatal("fixture creation id is not well formed")
	}
}
