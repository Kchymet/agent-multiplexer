package direct

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"amux/internal/core"
	"amux/internal/queue"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// The questions-v2 fixtures are the cross-repo contract for question prompts
// v2 (docs/question-prompts.md): queue entries carrying a form (with their
// legacy projection), every answer shape, the conflict errors, the
// capabilities block, and the question_request / question_resolved runtime
// events. harness and harness-android copy them byte-exact (SHA256SUMS).
// Regenerate with UPDATE_DIRECT_FIXTURES=1 go test ./internal/direct -run TestQuestionFixtures.
const questionFixtureDir = "../../docs/direct/fixtures/questions-v2"

func fixtureStr(s string) *string { return &s }

// fixtureForms are the forms the fixtures exercise.
func fixtureForms() (claude, codex, secret hp.Form) {
	claude = hp.Form{
		Version: hp.FormVersion, Source: hp.FormSourceClaudeAsk, Cancel: true, Reply: true, Delivery: hp.DeliveryStructured,
		Questions: []hp.FormQuestion{
			{ID: "q0", Header: "Layout", Question: "Which layout should the settings page use?", Other: true, Notes: true,
				Options: []hp.FormOption{
					{Label: "Tabs (Recommended)", Description: "One tab per area", Preview: "┌ General │ Account │ Billing ┐\n│ …                          │"},
					{Label: "Single page", Description: "One long scroll", Preview: "General\n───────\nAccount\n───────"},
				}},
			{ID: "q1", Header: "Extras", Question: "Which extras do you want?", MultiSelect: true, Other: true, Notes: true,
				Options: []hp.FormOption{{Label: "Search", Description: "Filter settings"}, {Label: "Dark mode"}, {Label: "Export"}}},
		},
	}
	codex = hp.Form{
		Version: hp.FormVersion, Source: hp.FormSourceCodexUserInput, Delivery: hp.DeliveryStructured, Cancel: true,
		Questions: []hp.FormQuestion{
			{ID: "env", Header: "Env", Question: "Which environment?", Other: true,
				Options: []hp.FormOption{{Label: "staging", Description: "safe"}, {Label: "production"}}},
		},
	}
	secret = hp.Form{
		Version: hp.FormVersion, Source: hp.FormSourceCodexUserInput, Delivery: hp.DeliveryStructured, Cancel: true,
		Questions: []hp.FormQuestion{{ID: "token", Header: "Token", Question: "Paste the deploy token", Other: true, Secret: true}},
	}
	return claude, codex, secret
}

func questionFixtureEntries() []queue.Entry {
	claude, codex, secret := fixtureForms()
	pane := claude
	pane.Questions = claude.Questions[:1]
	pane.Delivery = hp.DeliveryPane
	claimed := codex
	claimed.Claim = &hp.FormClaim{By: "direct:cl_8b1f", At: 1795400005000}
	entries := []queue.Entry{
		{ID: "q-eeeeeeeeee", Kind: queue.KindQuestion, Session: "a1", SessionTitle: "api work", Workgroup: "wg1",
			Title: "Which layout should the settings page use? (+1 more)", ToolUseID: "toolu_7", Form: &claude},
		{ID: "q-ffffffffff", Kind: queue.KindQuestion, Session: "a2", SessionTitle: "deploy", Workgroup: "wg1",
			Title: "Which environment?", RequestID: "ui-3", Form: &claimed},
		{ID: "q-gggggggggg", Kind: queue.KindQuestion, Session: "a2", SessionTitle: "deploy", Workgroup: "wg1",
			Title: "Paste the deploy token", RequestID: "ui-4", Form: &secret},
		{ID: "q-hhhhhhhhhh", Kind: queue.KindQuestion, Session: "a1", SessionTitle: "api work", Workgroup: "wg1",
			Title: "Which layout should the settings page use?", ToolUseID: "toolu_8", Form: &pane},
	}
	for i := range entries {
		queue.ProjectLegacy(&entries[i])
	}
	return entries
}

func questionFixtureFiles(t *testing.T) map[string][]byte {
	t.Helper()
	js := func(v any) []byte { return append(mustIndent(t, v), '\n') }
	id := fixtureIdentity(t)
	var entries []QueueEntry
	for _, e := range questionFixtureEntries() {
		entries = append(entries, projectEntry(e))
	}
	g := Grant{Resource: ResourceScope{Kind: ResourceAllSessions}, Perms: []string{PermRead, PermSteer, PermApprove, PermManage, PermCreate}}
	qc, oc := queueCaps(g)
	claude, _, _ := fixtureForms()
	event := func(typ, item string, payload any) hp.RuntimeEvent {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return hp.RuntimeEvent{Type: typ, ItemID: item, Direction: hp.DirOut, Payload: b}
	}
	answer := func(a hp.FormAnswer) QueueAnswerRequest {
		return QueueAnswerRequest{IdemKey: "5c1e9d2a-" + a.Action, Form: &a}
	}
	full := hp.FormAnswer{Version: hp.FormVersion, Action: hp.FormActionAnswer, Answers: []hp.QuestionAnswer{
		{ID: "q0", Selected: []int{0}, Note: "keep the icons"},
		{ID: "q1", Selected: []int{0, 1}, Other: fixtureStr("keyboard shortcuts")},
	}}
	other := hp.FormAnswer{Version: hp.FormVersion, Action: hp.FormActionAnswer, Answers: []hp.QuestionAnswer{
		{ID: "q0", Other: fixtureStr("a two-column grid")},
		{ID: "q1", Selected: []int{2}},
	}}
	cancel := hp.FormAnswer{Version: hp.FormVersion, Action: hp.FormActionCancel}
	reply := hp.FormAnswer{Version: hp.FormVersion, Action: hp.FormActionCancel, Message: "Let's discuss the layout first."}
	return map[string][]byte{
		"queue-response.json": js(QueueResponse{ServerID: id.ServerID, Entries: entries, Items: []InboxItem{}}),
		"capabilities-queue-form.json": js(CapabilitiesResponse{ServerID: id.ServerID, Permissions: g.Perms, ResourceScope: g.Resource,
			ServerVerbs: ServerVerbs(), Verbs: sessionVerbs(g), CreateWorkgroup: true, Queue: qc, Overseer: oc}),
		"session-caps-question.json":        js(SessionCaps{Prompt: true, Interject: true, Stop: true, Question: true}),
		"answer-request-form-full.json":     js(answer(full)),
		"answer-request-form-other.json":    js(QueueAnswerRequest{IdemKey: "5c1e9d2a-other", Form: &other}),
		"answer-request-form-cancel.json":   js(answer(cancel)),
		"answer-request-form-reply.json":    js(QueueAnswerRequest{IdemKey: "5c1e9d2a-reply", Form: &reply}),
		"answer-response.json":              js(ActionResponse{OK: true}),
		"release-request.json":              js(QueueReleaseRequest{IdemKey: "5c1e9d2a-release"}),
		"api-error-answered.json":           js(APIError{Code: CodeAnswered, Error: queueConflict(CodeAnswered)}),
		"api-error-needs-newer-client.json": js(APIError{Code: CodeNeedsNewerClient, Error: queueConflict(CodeNeedsNewerClient)}),
		"api-error-delivery-failed.json":    js(APIError{Code: CodeDeliveryFailed, Error: queueConflict(CodeDeliveryFailed)}),
		"api-error-answer-in-pane.json":     js(APIError{Code: CodeAnswerInPane, Error: queueConflict(CodeAnswerInPane)}),
		"event-question-request.json": js(event(hp.TypeQuestionRequest, "q-eeeeeeeeee",
			hp.QuestionRequestPayload{RequestID: "q-eeeeeeeeee", RuntimeGeneration: "gen-7", Form: claude})),
		"event-question-resolved.json": js(event(hp.TypeQuestionResolved, "q-eeeeeeeeee",
			hp.QuestionResolvedPayload{RequestID: "q-eeeeeeeeee", RuntimeGeneration: "gen-7", Outcome: hp.QuestionAnswered, Via: hp.ViaPane})),
	}
}

func TestQuestionFixtures(t *testing.T) {
	want := questionFixtureFiles(t)
	want["SHA256SUMS"] = sums(want)
	if os.Getenv("UPDATE_DIRECT_FIXTURES") == "1" {
		if err := os.MkdirAll(questionFixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for n, b := range want {
			if err := os.WriteFile(filepath.Join(questionFixtureDir, n), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	entries, err := os.ReadDir(questionFixtureDir)
	if err != nil {
		t.Fatalf("missing question fixtures (regenerate with UPDATE_DIRECT_FIXTURES=1): %v", err)
	}
	if len(entries) != len(want) {
		t.Errorf("question fixture dir has %d files, contract has %d", len(entries), len(want))
	}
	for n, b := range want {
		got, err := os.ReadFile(filepath.Join(questionFixtureDir, n))
		if err != nil || !bytes.Equal(got, b) {
			t.Errorf("%s drifted from the wire types: %v", n, err)
		}
	}
}

// TestQuestionFixturesRoundTrip decodes every answer fixture strictly, as the
// server does, and validates it against its form.
func TestQuestionFixturesRoundTrip(t *testing.T) {
	claude, _, _ := fixtureForms()
	for _, n := range []string{"answer-request-form-full.json", "answer-request-form-other.json", "answer-request-form-cancel.json", "answer-request-form-reply.json"} {
		b, err := os.ReadFile(filepath.Join(questionFixtureDir, n))
		if err != nil {
			t.Fatal(err)
		}
		var req QueueAnswerRequest
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || req.Form == nil {
			t.Fatalf("%s: %v", n, err)
		}
		if err := hp.ValidateAnswer(claude, *req.Form); err != nil {
			t.Errorf("%s: %v", n, err)
		}
		e := queue.Entry{Kind: queue.KindQuestion, Form: &claude}
		if _, err := answerFields(queue.KindQuestion, req, &e); err != nil {
			t.Errorf("%s: answerFields: %v", n, err)
		}
	}
	// The legacy projection of the fixtures: the secret form is omitted, the
	// preview form keeps its labels.
	es := questionFixtureEntries()
	if es[2].Questions != nil || es[2].Title[:len(queue.LegacyOmittedTitle)] != queue.LegacyOmittedTitle {
		t.Errorf("secret entry leaked to legacy fields: %+v", es[2])
	}
	if len(es[0].Questions) != 2 || es[0].Questions[0].Options[0].Label != "Tabs (Recommended)" {
		t.Errorf("legacy projection %+v", es[0].Questions)
	}
}

// TestAnswerFieldsForm pins the direct field rules for a form answer.
func TestAnswerFieldsForm(t *testing.T) {
	claude, _, _ := fixtureForms()
	e := queue.Entry{Kind: queue.KindQuestion, Form: &claude}
	good := hp.FormAnswer{Version: hp.FormVersion, Action: hp.FormActionCancel}
	f, err := answerFields(queue.KindQuestion, QueueAnswerRequest{IdemKey: "k", Form: &good}, &e)
	if err != nil || f["form"] == "" {
		t.Fatalf("form answer fields = %v, %v", f, err)
	}
	if _, err := answerFields(queue.KindQuestion, QueueAnswerRequest{IdemKey: "k", Form: &good, Answer: "x"}, &e); err == nil {
		t.Error("form plus answer accepted")
	}
	bad := hp.FormAnswer{Version: hp.FormVersion, Action: hp.FormActionAnswer}
	if _, err := answerFields(queue.KindQuestion, QueueAnswerRequest{IdemKey: "k", Form: &bad}, &e); err == nil {
		t.Error("an invalid form answer was accepted")
	}
	if _, err := answerFields(queue.KindQuestion, QueueAnswerRequest{IdemKey: "k", Form: &good}, &queue.Entry{Kind: queue.KindQuestion}); err == nil {
		t.Error("a form answer to an entry without a form was accepted")
	}
	if got := formAuditText(hp.FormAnswer{Version: 2, Action: hp.FormActionAnswer, Answers: []hp.QuestionAnswer{{ID: "token", Other: fixtureStr("s3cr3t")}}}); bytes.Contains([]byte(got), []byte("s3cr3t")) {
		t.Errorf("audit text leaks free text: %q", got)
	}
}

// TestQueueReleaseRoute: release is a question verb on the answer path —
// steer permission, strict body, a fresh in-scope read, then the daemon's
// host verb.
func TestQueueReleaseRoute(t *testing.T) {
	q := newQueueFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, q)
	steer := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	approve := enroll(t, a, []string{PermRead, PermApprove}, allSessions)
	path := func(id string) string { return PathQueue + "/" + id + "/release" }
	if w := do(s, "POST", path(qQuestion), approve.AccessToken, `{"idem_key":"r0"}`); w.Code != 403 {
		t.Fatalf("release without steer: %d", w.Code)
	}
	if w := do(s, "POST", path(qPermission), steer.AccessToken, `{"idem_key":"r1"}`); w.Code != 404 {
		t.Fatalf("release of a permission: %d", w.Code)
	}
	if w := do(s, "POST", path(qQuestion), steer.AccessToken, `{"idem_key":"r2","x":1}`); w.Code != 400 {
		t.Fatalf("unknown field: %d", w.Code)
	}
	if w := do(s, "POST", path("q-zzzzzzzzzz"), steer.AccessToken, `{"idem_key":"r3"}`); w.Code != 409 || apiCode(t, w) != CodeAskerGone {
		t.Fatalf("release of a gone question: %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", path(qQuestion), steer.AccessToken, `{"idem_key":"r4"}`); w.Code != 200 {
		t.Fatalf("release: %d %s", w.Code, w.Body)
	}
	if act := <-q.applied; act.Action != core.ActionQueueRelease || act.Fields[core.QueueEntryField] != qQuestion {
		t.Fatalf("release action = %+v", act)
	}
}
