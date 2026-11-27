package direct

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/items"
	"amux/internal/queue"
)

func wgScope(ids ...string) ResourceScope {
	return ResourceScope{Kind: ResourceWorkgroups, Workgroups: ids}
}

func decodeT[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %T: %v (%s)", v, err, w.Body)
	}
	return v
}

func entryIDs(r QueueResponse) []string {
	var out []string
	for _, e := range r.Entries {
		out = append(out, e.ID)
	}
	return out
}

func itemIDs(r QueueResponse) []string {
	var out []string
	for _, it := range r.Items {
		out = append(out, it.ID)
	}
	return out
}

func TestQueueIsProjectedToTheGrant(t *testing.T) {
	q := newQueueFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, q)

	all := enroll(t, a, []string{PermRead}, allSessions)
	w := do(s, "GET", PathQueue, all.AccessToken, "")
	if w.Code != 200 {
		t.Fatalf("queue: %d %s", w.Code, w.Body)
	}
	r := decodeT[QueueResponse](t, w)
	if got := entryIDs(r); !slices.Equal(got, []string{qPermission, qQuestion, qMulti, qGoal, qDecision, qOther}) {
		t.Errorf("all_sessions entries = %v", got)
	}
	if got := itemIDs(r); !slices.Equal(got, []string{itDecision, itRisk, itOther}) {
		t.Errorf("all_sessions items = %v", got)
	}
	if r.ServerID != a.ServerID() || r.Entries[0].State != EntryStateOpen || r.Entries[0].RuntimeGeneration != "gen-7" {
		t.Errorf("entry shape = %+v", r.Entries[0])
	}
	// Each entry and question says which control answers it.
	inputs := map[string]string{}
	for _, e := range r.Entries {
		inputs[e.ID] = e.Input
	}
	if inputs[qPermission] != queue.InputDecision || inputs[qMulti] != queue.InputQuestions || inputs[qGoal] != queue.InputText || inputs[qDecision] != queue.InputText {
		t.Errorf("entry inputs = %v", inputs)
	}
	multi := r.Entries[slices.IndexFunc(r.Entries, func(e QueueEntry) bool { return e.ID == qMulti })]
	if len(multi.Questions) != 2 || multi.Questions[0].Input != queue.InputMultiSelect || !multi.Questions[0].AllowOther ||
		multi.Questions[1].Input != queue.InputSingleSelect {
		t.Errorf("multi questions = %+v", multi.Questions)
	}

	wg := enroll(t, a, []string{PermRead}, wgScope("wg1"))
	r = decodeT[QueueResponse](t, do(s, "GET", PathQueue, wg.AccessToken, ""))
	if got := entryIDs(r); slices.Contains(got, qOther) || len(got) != 5 {
		t.Errorf("workgroup entries = %v", got)
	}
	if got := itemIDs(r); !slices.Equal(got, []string{itDecision, itRisk}) {
		t.Errorf("workgroup items = %v", got)
	}

	sum := decodeT[SummaryResponse](t, do(s, "GET", PathSummary, all.AccessToken, ""))
	if sum.Buckets != (SummaryBuckets{NeedsYou: 2, WaitingReview: 0, Running: 0, Done: 0}) || len(sum.Workgroups) != 2 {
		t.Errorf("all_sessions summary = %+v", sum)
	}
	wsum := decodeT[SummaryResponse](t, do(s, "GET", PathSummary, wg.AccessToken, ""))
	if len(wsum.Workgroups) != 1 || wsum.Workgroups[0].ID != "wg1" || wsum.Workgroups[0].Bucket != BucketNeedsYou || wsum.Buckets.NeedsYou != 1 {
		t.Errorf("workgroup summary = %+v", wsum)
	}

	if w := do(s, "GET", PathQueue+"?x=1", all.AccessToken, ""); w.Code != 400 {
		t.Errorf("query parameter accepted: %d", w.Code)
	}
	if w := do(s, "GET", PathQueue, "", ""); w.Code != 401 {
		t.Errorf("unauthenticated queue: %d", w.Code)
	}
}

func answer(body QueueAnswerRequest) string { b, _ := json.Marshal(body); return string(b) }

func TestQueueAnswerPermissionsAndFields(t *testing.T) {
	q := newQueueFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, q)
	steer := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	approve := enroll(t, a, []string{PermRead, PermApprove}, allSessions)
	path := func(id string) string { return PathQueue + "/" + id + "/answer" }

	// The permission an answer needs follows the entry kind, before any read.
	before := q.total()
	w := do(s, "POST", path(qPermission), steer.AccessToken, answer(QueueAnswerRequest{IdemKey: "k1", Decision: "allow", RuntimeGeneration: "gen-7"}))
	if w.Code != 403 || !strings.Contains(w.Header().Get("WWW-Authenticate"), `scope="approve"`) {
		t.Fatalf("permission answer without approve: %d %s", w.Code, w.Header())
	}
	w = do(s, "POST", path(qQuestion), approve.AccessToken, answer(QueueAnswerRequest{IdemKey: "k2", Answer: "x"}))
	if w.Code != 403 {
		t.Fatalf("question answer without steer: %d", w.Code)
	}
	if q.total() != before {
		t.Fatal("a denied answer reached the backend")
	}

	// Field rules per kind.
	for name, c := range map[string]struct {
		id   string
		body QueueAnswerRequest
	}{
		"permission without generation": {qPermission, QueueAnswerRequest{IdemKey: "b1", Decision: "allow"}},
		"permission with text":          {qPermission, QueueAnswerRequest{IdemKey: "b2", Decision: "allow", RuntimeGeneration: "gen-7", Answer: "ok"}},
		"permission bad decision":       {qPermission, QueueAnswerRequest{IdemKey: "b3", Decision: "always", RuntimeGeneration: "gen-7"}},
		"question both":                 {qQuestion, QueueAnswerRequest{IdemKey: "b4", Answer: "x", Choice: []string{"sqlite"}}},
		"question neither":              {qQuestion, QueueAnswerRequest{IdemKey: "b5"}},
		"goal with choice":              {qGoal, QueueAnswerRequest{IdemKey: "b6", Choice: []string{"a"}}},
		"decision empty":                {qDecision, QueueAnswerRequest{IdemKey: "b7", Answer: " "}},
	} {
		tok := steer.AccessToken
		if c.id == qPermission {
			tok = approve.AccessToken
		}
		if w := do(s, "POST", path(c.id), tok, answer(c.body)); w.Code != 400 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if w := do(s, "POST", path(qQuestion), steer.AccessToken, `{"idem_key":"u1","answer":"x","extra":1}`); w.Code != 400 {
		t.Errorf("unknown field accepted: %d", w.Code)
	}
	if w := do(s, "POST", path("q-!!"), steer.AccessToken, answer(QueueAnswerRequest{IdemKey: "u2", Answer: "x"})); w.Code != 404 {
		t.Errorf("malformed id: %d", w.Code)
	}
	// choice applies only to a single-question entry; multi-select only.
	if w := do(s, "POST", path(qMulti), steer.AccessToken, answer(QueueAnswerRequest{IdemKey: "c1", Choice: []string{"api"}})); w.Code != 400 {
		t.Errorf("choice on a multi-question entry: %d", w.Code)
	}
	if w := do(s, "POST", path(qQuestion), steer.AccessToken, answer(QueueAnswerRequest{IdemKey: "c2", Choice: []string{"sqlite", "postgres"}})); w.Code != 400 {
		t.Errorf("several labels on a single-select question: %d", w.Code)
	}

	// A stale generation is asker_gone before anything is sent.
	if w := do(s, "POST", path(qPermission), approve.AccessToken, answer(QueueAnswerRequest{IdemKey: "g1", Decision: "allow", RuntimeGeneration: "gen-6"})); w.Code != 409 || apiCode(t, w) != CodeAskerGone {
		t.Errorf("stale generation: %d %s", w.Code, w.Body)
	}

	// Good answers map to the daemon's re-checked host verb.
	w = do(s, "POST", path(qPermission), approve.AccessToken, answer(QueueAnswerRequest{IdemKey: "ok1", Decision: "deny", RuntimeGeneration: "gen-7"}))
	if w.Code != 200 {
		t.Fatalf("permission answer: %d %s", w.Code, w.Body)
	}
	act := <-q.applied
	if act.Action != core.ActionQueueAnswer || act.Fields[core.QueueEntryField] != qPermission || act.Fields[core.SteerDecision] != "deny" {
		t.Errorf("permission answer action = %+v", act)
	}
	if w := do(s, "POST", path(qQuestion), steer.AccessToken, answer(QueueAnswerRequest{IdemKey: "ok2", Choice: []string{"postgres"}, RuntimeGeneration: "anything"})); w.Code != 200 {
		t.Fatalf("question choice: %d %s", w.Code, w.Body)
	}
	if act := <-q.applied; act.Fields[core.QueueChoiceField] != "postgres" {
		t.Errorf("choice action = %+v", act)
	}
	if w := do(s, "POST", path(qMulti), steer.AccessToken, answer(QueueAnswerRequest{IdemKey: "ok3", Answer: "Scope: api, web\nShip: yes"})); w.Code != 200 {
		t.Fatalf("multi-question text: %d %s", w.Code, w.Body)
	}
	if act := <-q.applied; act.Fields[core.QueueTextField] != "Scope: api, web\nShip: yes" {
		t.Errorf("text action = %+v", act)
	}
	// answers: one control per question, Other text alongside choices.
	for name, body := range map[string]QueueAnswerRequest{
		"answers with answer":     {IdemKey: "a1", Answers: []QueueQuestionAnswer{{Choice: []string{"api"}}}, Answer: "x"},
		"answers with choice":     {IdemKey: "a2", Answers: []QueueQuestionAnswer{{Choice: []string{"api"}}}, Choice: []string{"api"}},
		"unknown label":           {IdemKey: "a3", Answers: []QueueQuestionAnswer{{Choice: []string{"cli"}}}},
		"too many answers":        {IdemKey: "a4", Answers: []QueueQuestionAnswer{{Other: "a"}, {Other: "b"}, {Other: "c"}}},
		"single choice and other": {IdemKey: "a5", Answers: []QueueQuestionAnswer{{}, {Choice: []string{"yes"}, Other: "later"}}},
		"nothing answered":        {IdemKey: "a6", Answers: []QueueQuestionAnswer{{}, {}}},
	} {
		if w := do(s, "POST", path(qMulti), steer.AccessToken, answer(body)); w.Code != 400 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if w := do(s, "POST", path(qGoal), steer.AccessToken, answer(QueueAnswerRequest{IdemKey: "a7", Answers: []QueueQuestionAnswer{{Other: "x"}}})); w.Code != 400 {
		t.Errorf("answers on a goal entry: %d", w.Code)
	}
	w = do(s, "POST", path(qMulti), steer.AccessToken, answer(QueueAnswerRequest{IdemKey: "ok4", Answers: []QueueQuestionAnswer{
		{Choice: []string{"web", "api"}, Other: "docs"}, {Other: "after review"},
	}}))
	if w.Code != 200 {
		t.Fatalf("answers: %d %s", w.Code, w.Body)
	}
	if act := <-q.applied; act.Fields[core.QueueAnswersField] != `[{"choice":["web","api"],"other":"docs"},{"other":"after review"}]` || act.Fields[core.QueueTextField] != "" {
		t.Errorf("answers action = %+v", act)
	}

	// Replaying a key answers the recorded result without re-running it.
	n := q.applies.Load()
	if w := do(s, "POST", path(qMulti), steer.AccessToken, answer(QueueAnswerRequest{IdemKey: "ok3", Answer: "Scope: api, web\nShip: yes"})); w.Code != 200 || q.applies.Load() != n {
		t.Errorf("replay: %d applies %d->%d", w.Code, n, q.applies.Load())
	}

	// The daemon's own re-check: answered and asker_gone are 409, and replay.
	q.rejectCode = CodeAnswered
	if w := do(s, "POST", path(qGoal), steer.AccessToken, answer(QueueAnswerRequest{IdemKey: "r1", Answer: "eu-west"})); w.Code != 409 || apiCode(t, w) != CodeAnswered {
		t.Errorf("answered: %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", path(qGoal), steer.AccessToken, answer(QueueAnswerRequest{IdemKey: "r1", Answer: "eu-west"})); w.Code != 409 || apiCode(t, w) != CodeAnswered {
		t.Errorf("answered replay: %d %s", w.Code, w.Body)
	}
	q.rejectCode = CodeAskerGone
	if w := do(s, "POST", path("g-zzzzzzzzzz"), steer.AccessToken, answer(QueueAnswerRequest{IdemKey: "r2", Answer: "x"})); w.Code != 409 || apiCode(t, w) != CodeAskerGone {
		t.Errorf("gone entry: %d %s", w.Code, w.Body)
	}

	// Every answer was audited, admitted first.
	recs, err := a.AuditRecords("", 0)
	if err != nil || len(recs) < 4 || recs[0].Outcome != "admitted" || recs[0].Route != "queue.answer" || recs[0].Label != "phone" {
		t.Fatalf("audit = %+v, %v", recs, err)
	}
}

func TestWorkgroupGrantCannotAnswerOutsideItsScope(t *testing.T) {
	q := newQueueFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, q)
	wg := enroll(t, a, []string{PermRead, PermSteer}, wgScope("wg1"))
	before := q.applies.Load()
	if w := do(s, "POST", PathQueue+"/"+qOther+"/answer", wg.AccessToken, answer(QueueAnswerRequest{IdemKey: "o1", Answer: "x"})); w.Code != 404 {
		t.Errorf("out-of-scope entry: %d %s", w.Code, w.Body)
	}
	if w := do(s, "POST", PathQueue+"/"+qConsole+"/answer", wg.AccessToken, answer(QueueAnswerRequest{IdemKey: "o2", Answer: "x"})); w.Code != 404 {
		t.Errorf("console entry: %d %s", w.Code, w.Body)
	}
	if q.applies.Load() != before {
		t.Fatal("an out-of-scope answer reached the daemon")
	}
	if w := do(s, "POST", PathQueue+"/"+qGoal+"/answer", wg.AccessToken, answer(QueueAnswerRequest{IdemKey: "o3", Answer: "eu"})); w.Code != 200 {
		t.Errorf("in-scope entry: %d %s", w.Code, w.Body)
	}
}

func itemState(state, key string) string {
	b, _ := json.Marshal(ItemStateRequest{State: state, IdemKey: key})
	return string(b)
}

func TestItemTriage(t *testing.T) {
	q := newQueueFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, q)
	steer := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	creator := enroll(t, a, []string{PermRead, PermSteer, PermCreate}, allSessions)
	wgCreator := enroll(t, a, []string{PermRead, PermSteer, PermCreate}, wgScope("wg1"))
	reader := enroll(t, a, []string{PermRead}, allSessions)
	path := func(id string) string { return PathItems + "/" + id + "/state" }

	if w := do(s, "POST", path(itRisk), reader.AccessToken, itemState("dismiss", "t0")); w.Code != 403 {
		t.Errorf("triage without steer: %d", w.Code)
	}
	// ack is the user's go-ahead for the overseer: steer + create + all_sessions.
	for _, tok := range []string{steer.AccessToken, wgCreator.AccessToken} {
		if w := do(s, "POST", path(itDecision), tok, itemState("ack", "t1")); w.Code != 403 || !strings.Contains(w.Header().Get("WWW-Authenticate"), `scope="create"`) {
			t.Errorf("ack without create+all_sessions: %d %s", w.Code, w.Header())
		}
	}
	w := do(s, "POST", path(itDecision), creator.AccessToken, itemState("ack", "t2"))
	if w.Code != 200 || decodeT[ItemStateResponse](t, w) != (ItemStateResponse{OK: true, State: items.StateAcknowledged}) {
		t.Fatalf("ack: %d %s", w.Code, w.Body)
	}
	act := <-q.applied
	if act.Action != core.ActionItemState || act.ID != items.GlobalInbox() || act.Fields[core.ItemIDField] != itDecision ||
		act.Fields[core.ItemStateField] != "ack" || !strings.Contains(act.Fields[core.ItemNoteField], "phone") {
		t.Errorf("ack action = %+v", act)
	}
	// "acknowledge" is accepted for ack; dismiss/done/reopen need only steer.
	if w := do(s, "POST", path(itRisk), creator.AccessToken, itemState("acknowledge", "t3")); w.Code != 200 {
		t.Errorf("acknowledge alias: %d %s", w.Code, w.Body)
	}
	<-q.applied
	if w := do(s, "POST", path(itRisk), steer.AccessToken, itemState("dismiss", "t4")); w.Code != 200 || decodeT[ItemStateResponse](t, w).State != items.StateDismissed {
		t.Errorf("dismiss: %d %s", w.Code, w.Body)
	}
	<-q.applied
	if w := do(s, "POST", path("it-0000000e"), steer.AccessToken, itemState("reopen", "t5")); w.Code != 200 || decodeT[ItemStateResponse](t, w).State != items.StateOpen {
		t.Errorf("reopen: %d %s", w.Code, w.Body)
	}
	<-q.applied
	before := q.applies.Load()
	for name, c := range map[string][2]string{
		"bad state":       {itRisk, "bogus"},
		"console item":    {itConsole, "done"},
		"unknown item":    {"it-000000ff", "done"},
		"malformed id":    {"nope", "done"},
		"other workgroup": {itOther, "done"},
	} {
		tok := steer.AccessToken
		if name == "other workgroup" {
			tok = wgCreator.AccessToken
		}
		want := 404
		if name == "bad state" {
			want = 400
		}
		if w := do(s, "POST", path(c[0]), tok, itemState(c[1], "x-"+name)); w.Code != want {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if q.applies.Load() != before {
		t.Fatal("a refused triage reached the daemon")
	}
}

func sessionIDs(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var r SessionsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range r.Sessions {
		out = append(out, s.ID)
	}
	return out
}

func TestOverseerInventoryIsOptIn(t *testing.T) {
	q := newQueueFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, q)
	all := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	wg := enroll(t, a, []string{PermRead, PermSteer}, wgScope("wg1"))

	if got := sessionIDs(t, do(s, "GET", PathSession, all.AccessToken, "")); slices.Contains(got, OverseerID) {
		t.Errorf("overseer without include: %v", got)
	}
	if got := sessionIDs(t, do(s, "GET", PathSession+"?include=overseer", all.AccessToken, "")); !slices.Contains(got, OverseerID) || slices.Contains(got, ConsoleID) {
		t.Errorf("include=overseer for all_sessions: %v", got)
	}
	if got := sessionIDs(t, do(s, "GET", PathSession+"?include=overseer", wg.AccessToken, "")); slices.Contains(got, OverseerID) {
		t.Errorf("overseer for a workgroup grant: %v", got)
	}
	// An older client's inventory is byte-identical to a host without the overseer.
	plain := newFake(slices.DeleteFunc(queueRows(), func(r core.Session) bool { return r.ID == OverseerID })...)
	s2 := newTestServer(t, a, plain)
	if got, want := do(s, "GET", PathSession, all.AccessToken, "").Body.Bytes(), do(s2, "GET", PathSession, all.AccessToken, "").Body.Bytes(); !bytes.Equal(got, want) {
		t.Errorf("old-client inventory changed:\n%s\n%s", got, want)
	}
	// The overseer's history is readable by an all_sessions grant only.
	if w := do(s, "GET", PathSession+"/overseer/history", all.AccessToken, ""); w.Code != 200 {
		t.Errorf("overseer history: %d", w.Code)
	}
	if w := do(s, "GET", PathSession+"/overseer/history", wg.AccessToken, ""); w.Code != 404 {
		t.Errorf("overseer history for a workgroup grant: %d", w.Code)
	}
}

func TestOverseerSteerNeedsEverythingItCouldDo(t *testing.T) {
	q := newQueueFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, q)
	steer := enroll(t, a, []string{PermRead, PermSteer}, allSessions)
	most := enroll(t, a, []string{PermRead, PermSteer, PermManage}, allSessions)
	full := enroll(t, a, []string{PermRead, PermSteer, PermManage, PermCreate}, allSessions)
	path := PathSession + "/overseer/actions"
	before := q.applies.Load()
	for _, tok := range []string{steer.AccessToken, most.AccessToken} {
		w := do(s, "POST", path, tok, actionBody(VerbPrompt, "archive everything", "o-"+tok[:12]))
		if w.Code != 403 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "steer manage create") {
			t.Errorf("partial grant steering the overseer: %d %s", w.Code, w.Header())
		}
	}
	if w := do(s, "POST", path, full.AccessToken, actionBody(VerbArchive, "", "o-archive")); w.Code != 409 || apiCode(t, w) != CodeUnsupportedVerb {
		t.Errorf("archive on the overseer: %d %s", w.Code, w.Body)
	}
	if q.applies.Load() != before {
		t.Fatal("a refused overseer steer reached the daemon")
	}
	w := do(s, "POST", path, full.AccessToken, actionBody(VerbPrompt, "summarize the day", "o-ok"))
	if w.Code != 200 {
		t.Fatalf("full grant steering the overseer: %d %s", w.Code, w.Body)
	}
	if act := <-q.applied; act.ID != OverseerID || act.Fields[core.SteerText] != "summarize the day" {
		t.Errorf("overseer steer action = %+v", act)
	}
	recs, _ := a.AuditRecords("", 0)
	if len(recs) != 2 || recs[0].Route != "overseer.steer" || recs[0].Outcome != "admitted" || recs[1].Outcome != "ok" || recs[1].Text != "summarize the day" {
		t.Errorf("overseer audit = %+v", recs)
	}
	// Rows advertise the gate.
	var r SessionsResponse
	_ = json.Unmarshal(do(s, "GET", PathSession+"?include=overseer", steer.AccessToken, "").Body.Bytes(), &r)
	for _, row := range r.Sessions {
		if row.ID == OverseerID && (row.Caps.Prompt || row.Caps.Stop) {
			t.Errorf("overseer row caps for a steer-only grant: %+v", row.Caps)
		}
	}
}

// TestConsoleNeverExposedAnywhere is the permanent console-exclusion suite:
// whatever the grant, the admin console and what it raised never appear on,
// or are reachable through, any direct route.
func TestConsoleNeverExposedAnywhere(t *testing.T) {
	q := newQueueFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, q)
	tok := enroll(t, a, []string{PermRead, PermSteer, PermRespond, PermApprove, PermManage, PermCreate, PermAccounts}, allSessions)
	for _, path := range []string{PathSession, PathSession + "?include=overseer", PathQueue, PathSummary} {
		w := do(s, "GET", path, tok.AccessToken, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		if b := w.Body.String(); strings.Contains(b, `"`+ConsoleID+`"`) || strings.Contains(b, qConsole) || strings.Contains(b, itConsole) {
			t.Errorf("%s exposes the console: %s", path, b)
		}
	}
	before := q.applies.Load() + q.histories.Load() + q.streams.Load()
	for _, req := range [][3]string{
		{"GET", PathSession + "/console/history", ""},
		{"GET", PathSession + "/console/stream", ""},
		{"POST", PathSession + "/console/actions", actionBody(VerbPrompt, "rm -rf", "c1")},
		{"POST", PathSession + "/console/actions", actionBody(VerbStart, "", "c2")},
		{"POST", PathQueue + "/" + qConsole + "/answer", answer(QueueAnswerRequest{IdemKey: "c3", Answer: "yes"})},
		{"POST", PathItems + "/" + itConsole + "/state", itemState("done", "c4")},
	} {
		if w := do(s, req[0], req[1], tok.AccessToken, req[2]); w.Code != 404 {
			t.Errorf("%s %s: %d", req[0], req[1], w.Code)
		}
	}
	if q.applies.Load()+q.histories.Load()+q.streams.Load() != before {
		t.Fatal("a console request reached the backend")
	}
	for _, scope := range []ResourceScope{wgScope(ConsoleID), wgScope(OverseerID)} {
		if _, _, err := a.NewCode("x", []string{PermRead}, scope, 0); err == nil {
			t.Errorf("--workgroup %v accepted", scope.Workgroups)
		}
	}
	// Cloud ceilings keep refusing it too.
	if err := validateCloudCeiling(t, a, wgScope(ConsoleID)); err == nil {
		t.Error("cloud ceiling admitted the console")
	}
}

func TestQueueCapabilities(t *testing.T) {
	q := newQueueFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	s := newTestServer(t, a, q)
	full := enroll(t, a, []string{PermRead, PermSteer, PermApprove, PermManage, PermCreate}, allSessions)
	c := decodeT[CapabilitiesResponse](t, do(s, "GET", PathCapabilities, full.AccessToken, ""))
	if c.Queue == nil || !c.Queue.Read || !slices.Equal(c.Queue.AnswerKinds, []string{"question", "permission", "goal_input", "decision_needed"}) ||
		!c.Queue.Triage || !c.Queue.Acknowledge || c.Overseer == nil || !c.Overseer.Read || !c.Overseer.Steer {
		t.Errorf("full caps = %+v %+v", c.Queue, c.Overseer)
	}
	wg := enroll(t, a, []string{PermRead}, wgScope("wg1"))
	c = decodeT[CapabilitiesResponse](t, do(s, "GET", PathCapabilities, wg.AccessToken, ""))
	if c.Queue == nil || len(c.Queue.AnswerKinds) != 0 || c.Queue.Triage || c.Queue.Acknowledge || c.Overseer.Read || c.Overseer.Steer {
		t.Errorf("read-only workgroup caps = %+v %+v", c.Queue, c.Overseer)
	}
	// A backend without the queue (an older host) advertises neither.
	s2 := newTestServer(t, a, newFake(testRows()...))
	if c := decodeT[CapabilitiesResponse](t, do(s2, "GET", PathCapabilities, full.AccessToken, "")); c.Queue != nil || c.Overseer != nil {
		t.Errorf("caps without a queue backend = %+v %+v", c.Queue, c.Overseer)
	}
}

func TestQueueWatchPushesChanges(t *testing.T) {
	q := newQueueFake()
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	srv := newTestServer(t, a, q)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	tok := enroll(t, a, []string{PermRead}, wgScope("wg1"))
	conn := dialWatchPath(t, ts, PathQueueWatch)
	if err := conn.WriteJSON(WatchAuth{Type: "auth", AccessToken: tok.AccessToken}); err != nil {
		t.Fatal(err)
	}
	var welcome WatchWelcome
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := conn.ReadJSON(&welcome); err != nil || welcome.Type != "welcome" {
		t.Fatalf("welcome = %+v, %v", welcome, err)
	}
	var snap QueueWatchSnapshot
	if err := conn.ReadJSON(&snap); err != nil || snap.Type != "snapshot" || len(snap.Entries) != 5 || len(snap.Items) != 2 || len(snap.Summary.Workgroups) != 1 {
		t.Fatalf("first snapshot = %+v, %v", snap, err)
	}
	q.qmu.Lock()
	q.overview.Entries = q.overview.Entries[1:]
	q.qmu.Unlock()
	if err := conn.ReadJSON(&snap); err != nil || len(snap.Entries) != 4 {
		t.Fatalf("changed snapshot = %+v, %v", snap, err)
	}
}

func TestAuditAdminOp(t *testing.T) {
	a := openTestAuthority(t, t.TempDir(), Lifetimes{}, nil)
	for i, c := range []string{"cl-a", "cl-b", "cl-a"} {
		if err := a.Audit(AuditRecord{ClientID: c, Route: "queue.answer", Verb: "question", Target: "q-x", Outcome: "ok", Text: strings.Repeat("x", i)}); err != nil {
			t.Fatal(err)
		}
	}
	resp := Administer(a, nil, AdminRequest{Op: OpAudit, ClientID: "cl-a"})
	if !resp.OK || len(resp.Audit) != 2 || resp.Audit[1].Text != "xx" {
		t.Fatalf("audit op = %+v", resp)
	}
	if err := a.Audit(AuditRecord{ClientID: "cl-a", Text: strings.Repeat("é", MaxAuditTextBytes)}); err != nil {
		t.Fatal(err)
	}
	recs, _ := a.AuditRecords("cl-a", 1)
	if len(recs) != 1 || len(recs[0].Text) > MaxAuditTextBytes+len("…") {
		t.Errorf("capped text = %d bytes", len(recs[0].Text))
	}
}
