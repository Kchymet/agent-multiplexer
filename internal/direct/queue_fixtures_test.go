package direct

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"amux/internal/core"
	"amux/internal/queue"
)

// The queue-v1 fixtures are the additive contract for the question queue, the
// workgroup summary, item triage and the overseer row (docs/direct-access.md
// "Question queue and overseer"). They live beside the other fixture sets,
// whose manifests are unchanged. Regenerate with
// UPDATE_DIRECT_FIXTURES=1 go test ./internal/direct -run TestQueueFixtures.
const queueFixtureDir = "../../docs/direct/fixtures/queue-v1"

func queueFixtureFiles(t *testing.T) map[string][]byte {
	t.Helper()
	js := func(v any) []byte { return append(mustIndent(t, v), '\n') }
	id := fixtureIdentity(t)
	g := Grant{Resource: ResourceScope{Kind: ResourceAllSessions}, Perms: []string{PermRead, PermSteer, PermApprove, PermManage, PermCreate}}
	rows := queueRows()
	v := queueView{entries: []QueueEntry{}, items: []InboxItem{}, rows: rows, byID: indexRows(rows)}
	for _, e := range queueEntries() {
		if sourceVisible(g, e.Session, e.Workgroup, v.byID) {
			v.raw = append(v.raw, e)
			v.entries = append(v.entries, projectEntry(e))
		}
	}
	for _, it := range queueItems() {
		if sourceVisible(g, it.Source, it.Workgroup, v.byID) {
			v.items = append(v.items, projectItem(it))
		}
	}
	sum := summarize(g, v)
	qc, oc := queueCaps(g)
	rc, ro := queueCaps(Grant{Resource: ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"771951"}}, Perms: []string{PermRead, PermSteer}})
	// queue-v1 is frozen: it describes a server before question prompts v2
	// (their capabilities live in questions-v2), so the copies other repos
	// pin stay valid.
	qc.Form, rc.Form = nil, nil
	overseerRow := project(g, rows[4], v.byID)
	return map[string][]byte{
		"queue-response.json":                 js(QueueResponse{ServerID: id.ServerID, Entries: v.entries, Items: v.items}),
		"queue-response-empty.json":           js(QueueResponse{ServerID: id.ServerID, Entries: []QueueEntry{}, Items: []InboxItem{}}),
		"summary-response.json":               js(SummaryResponse{ServerID: id.ServerID, Summary: sum}),
		"queue-watch-snapshot.json":           js(QueueWatchSnapshot{Type: "snapshot", Entries: v.entries, Items: v.items, Summary: sum}),
		"answer-request-question-choice.json": js(QueueAnswerRequest{IdemKey: "3b0f7a8e-0001", Choice: []string{"postgres"}}),
		"answer-request-question-text.json":   js(QueueAnswerRequest{IdemKey: "3b0f7a8e-0002", Answer: "Scope: api, web\nShip today?: yes"}),
		"answer-request-permission.json":      js(QueueAnswerRequest{IdemKey: "3b0f7a8e-0003", Decision: core.SteerAllow, RuntimeGeneration: "gen-7"}),
		"answer-request-goal-input.json":      js(QueueAnswerRequest{IdemKey: "3b0f7a8e-0004", Answer: "Start in eu-west."}),
		"answer-request-decision.json":        js(QueueAnswerRequest{IdemKey: "3b0f7a8e-0005", Answer: "Yes: rewrite it in a new workgroup."}),
		"answer-response.json":                js(ActionResponse{OK: true}),
		"item-state-request.json":             js(ItemStateRequest{State: TriageAck, IdemKey: "3b0f7a8e-0006"}),
		"item-state-response.json":            js(ItemStateResponse{OK: true, State: "acknowledged"}),
		"api-error-asker-gone.json":           js(APIError{Code: CodeAskerGone, Error: "the question is no longer open: it was answered elsewhere, replaced, or its session went away"}),
		"api-error-answered.json":             js(APIError{Code: CodeAnswered, Error: "this question was already answered"}),
		"api-error-overseer-denied.json":      js(APIError{Code: CodeDenied, Error: "steering the overseer needs an all-sessions client with steer, manage and create"}),
		"capabilities-queue-full.json": js(CapabilitiesResponse{ServerID: id.ServerID, Permissions: g.Perms, ResourceScope: g.Resource,
			ServerVerbs: ServerVerbs(), Verbs: sessionVerbs(g), CreateWorkgroup: true, Queue: qc, Overseer: oc}),
		"capabilities-queue-workgroup.json": js(CapabilitiesResponse{ServerID: id.ServerID, Permissions: []string{PermRead, PermSteer},
			ResourceScope: ResourceScope{Kind: ResourceWorkgroups, Workgroups: []string{"771951"}}, ServerVerbs: ServerVerbs(),
			Verbs: []string{VerbPrompt, VerbStop, VerbInterject}, Queue: rc, Overseer: ro}),
		"session-overseer.json": js(overseerRow),
		"audit-record.json": js(AuditRecord{At: 1795400000000, ClientID: "cl_8b1f", Label: "Pixel", Route: "queue.answer",
			Verb: queue.KindPermission, Target: qPermission, Text: "allow", Outcome: "ok"}),
	}
}

func TestQueueFixtures(t *testing.T) {
	want := queueFixtureFiles(t)
	want["SHA256SUMS"] = sums(want)
	if os.Getenv("UPDATE_DIRECT_FIXTURES") == "1" {
		if err := os.MkdirAll(queueFixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for n, b := range want {
			if err := os.WriteFile(filepath.Join(queueFixtureDir, n), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, err := os.ReadDir(queueFixtureDir)
	if err != nil {
		t.Fatalf("missing queue fixtures (regenerate with UPDATE_DIRECT_FIXTURES=1): %v", err)
	}
	if len(entries) != len(want) {
		t.Errorf("queue fixture dir has %d files, contract has %d", len(entries), len(want))
	}
	for n, b := range want {
		got, err := os.ReadFile(filepath.Join(queueFixtureDir, n))
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("%s drifted from the wire types: %v", n, err)
		}
	}
}
