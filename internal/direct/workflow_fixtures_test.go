package direct

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// The workflow-v1 fixtures are the additive contract for the verbs beyond S1's
// prompt and stop, root creation and capabilities. They live beside, never
// inside, docs/direct/fixtures/v1 (whose manifest web and Android pin).
// Regenerate with UPDATE_DIRECT_FIXTURES=1 go test ./internal/direct -run TestWorkflowFixtures.
const workflowFixtureDir = "../../docs/direct/fixtures/workflow-v1"

func workflowFixtureFiles(t *testing.T) map[string][]byte {
	t.Helper()
	js := func(v any) []byte { return append(mustIndent(t, v), '\n') }
	req := func(verb string, fields map[string]string, key string) []byte {
		if fields == nil {
			fields = map[string]string{}
		}
		return js(ActionRequest{Verb: verb, Fields: fields, IdemKey: key})
	}
	id := fixtureIdentity(t)
	return map[string][]byte{
		"capabilities-response.json": js(CapabilitiesResponse{
			ServerID: id.ServerID, Permissions: strs(PermRead, PermSteer, PermRespond, PermApprove, PermManage, PermCreate),
			ResourceScope: ResourceScope{Kind: ResourceAllSessions}, ServerVerbs: ServerVerbs(),
			Verbs: strs(VerbPrompt, VerbStop, VerbInterject, VerbGoal, VerbPermission, VerbStart, VerbRename,
				VerbArchive, VerbUnarchive, VerbAddAgent),
			CreateWorkgroup: true,
			Catalog: &CreationCatalog{
				Harnesses: []CatalogHarness{{Name: "claude", Version: "2.1.0", Models: []string{"claude-opus-5-5", "claude-sonnet-5-5"}},
					{Name: "codex", Version: "0.9.0", Models: []string{"gpt-6", "gpt-6-mini"}}},
				IdentityModes: []string{hp.IdentityMachine}, Modes: []string{"task", "interactive"},
				Repos:      []string{"agent-multiplexer", "harness"},
				Workgroups: []CatalogWorkgroup{{ID: "771951", Repos: []string{"agent-multiplexer"}}},
			},
		}),
		"capabilities-response-steer.json": js(CapabilitiesResponse{
			ServerID: id.ServerID, Permissions: strs(PermRead, PermSteer),
			ResourceScope: ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"771951"}}, ServerVerbs: ServerVerbs(),
			Verbs: strs(VerbPrompt, VerbStop),
		}),
		"session-caps-full.json": js(Session{ID: "e1deda", Title: "Backend implementation", RootID: "771951",
			Section: hp.SectionWorkgroups, State: hp.StateWaiting, Runtime: "codex", ControlMode: hp.ControlModeStructured,
			Caps: SessionCaps{Prompt: true, Stop: true, Interject: true, Permission: true, Start: true, Rename: true, Archive: true}}),
		"action-request-interject.json":  req(VerbInterject, map[string]string{"text": "Skip the flaky test."}, "b1f0c6a2-1"),
		"action-request-permission.json": req(VerbPermission, map[string]string{"request_id": "call_7", "runtime_generation": "rg_01J9", "decision": "allow"}, "b1f0c6a2-2"),
		"action-request-goal.json":       req(VerbGoal, map[string]string{"status": "paused"}, "b1f0c6a2-3"),
		"action-request-start.json":      req(VerbStart, nil, "b1f0c6a2-4"),
		"action-request-rename.json":     req(VerbRename, map[string]string{"name": "API work"}, "b1f0c6a2-5"),
		"action-request-archive.json":    req(VerbArchive, nil, "b1f0c6a2-6"),
		"action-request-unarchive.json":  req(VerbUnarchive, nil, "b1f0c6a2-7"),
		"action-request-add-agent.json":  req(VerbAddAgent, map[string]string{"agent": "codex", "mode": "task", "repos": "agent-multiplexer", "prompt": "Fix the flaky test."}, "b1f0c6a2-8"),
		"create-request-new-workgroup.json": req(VerbNewWorkgroup, map[string]string{"name": "Docs", "prompt": "Refresh the guides.",
			"repos": "harness", "agent": "claude", "coordinator": "codex", "identity_mode": hp.IdentityMachine}, "b1f0c6a2-9"),
		"create-request-new-workgroup-empty.json": req(VerbNewWorkgroupEmpty, map[string]string{"name": "Triage", "repos": ""}, "b1f0c6a2-10"),
		"action-response-created.json":            js(ActionResponse{OK: true, NewID: "4c2a91", RootID: "771951"}),
		"action-response-archive.json":            js(ActionResponse{OK: true, Affected: []string{"771951", "e1deda"}}),
		"api-error-denied.json":                   js(APIError{Code: CodeDenied, Error: "this client may not perform that operation"}),
		"api-error-not-in-catalog.json": js(APIError{Code: CodeNotInCatalog,
			Error: "a harness or repository is not in this machine's creation catalog for this target"}),
	}
}

func strs(v ...string) []string { return v }

func TestWorkflowFixtures(t *testing.T) {
	want := workflowFixtureFiles(t)
	want["SHA256SUMS"] = sums(want)
	if os.Getenv("UPDATE_DIRECT_FIXTURES") == "1" {
		if err := os.MkdirAll(workflowFixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for n, b := range want {
			if err := os.WriteFile(filepath.Join(workflowFixtureDir, n), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, err := os.ReadDir(workflowFixtureDir)
	if err != nil {
		t.Fatalf("missing workflow fixtures (regenerate with UPDATE_DIRECT_FIXTURES=1): %v", err)
	}
	if len(entries) != len(want) {
		t.Errorf("workflow fixture dir has %d files, contract has %d", len(entries), len(want))
	}
	for n, b := range want {
		got, err := os.ReadFile(filepath.Join(workflowFixtureDir, n))
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("%s drifted from the wire types: %v", n, err)
		}
	}
	// Every request fixture decodes and validates exactly as the server would.
	for n, b := range want {
		if !bytes.HasPrefix([]byte(n), []byte("action-request-")) && !bytes.HasPrefix([]byte(n), []byte("create-request-")) {
			continue
		}
		var r ActionRequest
		if err := jsonStrict(b, &r); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		var verr error
		if bytes.HasPrefix([]byte(n), []byte("create-")) {
			_, verr = createActionFor(r)
		} else {
			_, verr = actionFor("e1deda", r)
		}
		if verr != nil {
			t.Fatalf("%s does not validate: %v", n, verr)
		}
	}
}

func jsonStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
