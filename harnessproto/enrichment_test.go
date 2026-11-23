package harnessproto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// specRow is a coordinator and a member row as session-enrichments-v1 puts
// them on the wire: the shape web and Android build against.
const specRow = `[
 {"id":"wg1","title":"wg","source":"workspace","kind":"claude","isRoot":true,"state":"ready","status":"waiting for merge · 1 PR · 1 agent",
  "cwd":"","startedAt":0,"canAttach":true,"canKill":true,"canResume":false,"subState":"waiting_merge",
  "rollup":{"state":"waiting_merge","count":1,"label":"waiting for merge · 1 PR","reason":"m1 PR #79 approved",
   "enrichments":{"pull_request":{"open":1,"draft":0,"attention":0,"waiting_review":0,"waiting_merge":1,"ci_pending":0,"merged":0,"closed":0}},
   "stale":false,"stale_reason":"","observed_at":"2026-11-22T18:03:11Z"}},
 {"id":"m1","title":"m1","source":"workspace","kind":"claude","rootId":"wg1","state":"ready","status":"ready · PR #79 CI✓ approved",
  "cwd":"","startedAt":0,"canAttach":true,"canKill":true,"canResume":false,
  "enrichments":[{"kind":"pull_request","id":"o/r#79","title":"chips","url":"https://github.com/o/r/pull/79","tone":"ok","label":"#79 ✓ approved",
   "observed_at":"2026-11-22T18:03:11Z","stale":false,
   "pull_request":{"repo":"o/r","number":79,"title":"chips","url":"https://github.com/o/r/pull/79","state":"open","head_branch":"amux/wg1-m1",
    "base_branch":"master","phase":"merge","attention":[],"ci":{"status":"green","failing_check":"","failing_checks":[]},
    "review":{"state":"approved","by":["octocat"],"requested":[]},"mergeable":{"conflict":false,"behind":false,"blocked_by_checks":false},
    "updated_at":"2026-11-22T17:58:40Z","merged_at":"","closed_at":""}},
   {"kind":"preview_url","id":"p1","title":"preview","url":"https://example.test","tone":"neutral","label":"preview","observed_at":"","stale":false,
    "preview_url":{"url":"https://example.test"}}]}
]`

// The spec rows decode losslessly: re-encoding gives the same JSON, with an
// unknown kind's payload dropped but its generic envelope kept.
func TestEnrichmentWireRoundTrip(t *testing.T) {
	var rows []Session
	if err := json.Unmarshal([]byte(specRow), &rows); err != nil {
		t.Fatal(err)
	}
	root, member := rows[0], rows[1]
	if root.SubState != SubStateWaitingMerge || root.Rollup == nil || root.Rollup.State != RollupWaitingMerge ||
		root.Rollup.Enrichments.PullRequest == nil || root.Rollup.Enrichments.PullRequest.WaitingMerge != 1 {
		t.Fatalf("root = %+v rollup = %+v", root, root.Rollup)
	}
	es := *member.Enrichments
	if len(es) != 2 || es[0].PullRequest == nil || es[0].PullRequest.Review.By[0] != "octocat" {
		t.Fatalf("member enrichments = %+v", es)
	}
	if es[1].Kind != "preview_url" || es[1].PullRequest != nil || es[1].Label != "preview" {
		t.Fatalf("unknown kind envelope = %+v", es[1])
	}
	b, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	_ = json.Unmarshal([]byte(strings.Replace(specRow, `,
    "preview_url":{"url":"https://example.test"}`, "", 1)), &want)
	_ = json.Unmarshal(b, &got)
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("round trip differs:\n got %s", b)
	}
}

// oldSession is a consumer that predates enrichments (state, subState, prs).
type oldSession struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	SubState string `json:"subState"`
}

// An old decoder ignores the new fields and still reads a ladder state.
func TestEnrichmentOldDecoder(t *testing.T) {
	var rows []oldSession
	dec := json.NewDecoder(strings.NewReader(specRow))
	if err := dec.Decode(&rows); err != nil {
		t.Fatal(err)
	}
	ladder := map[string]bool{StateIdle: true, StateReady: true, StateWaiting: true, StateRunning: true, StateUnknown: true}
	for _, r := range rows {
		if !ladder[r.State] {
			t.Errorf("row %s state %q is not on the attention ladder", r.ID, r.State)
		}
	}
}

// Every key nested under enrichments/rollup is snake_case, so the camelCase
// harnessproto wire, the snake_case direct API and the cloud projection share
// one decoder.
func TestEnrichmentKeysSnakeCase(t *testing.T) {
	for _, v := range []any{Enrichment{}, PullRequestEnrichment{}, PRCI{}, PRReviewSummary{}, PRMergeable{}, Rollup{}, RollupEnrichments{}, PullRequestCounts{}} {
		typ := reflect.TypeOf(v)
		for i := 0; i < typ.NumField(); i++ {
			tag := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
			if tag == "" || strings.ToLower(tag) != tag {
				t.Errorf("%s.%s json key %q is not snake_case", typ.Name(), typ.Field(i).Name, tag)
			}
		}
	}
	// The session keys themselves are single words.
	st := reflect.TypeOf(Session{})
	for _, name := range []string{"Enrichments", "Rollup"} {
		f, _ := st.FieldByName(name)
		if tag := f.Tag.Get("json"); tag != strings.ToLower(name)+",omitempty" {
			t.Errorf("Session.%s tag = %q", name, tag)
		}
	}
}

// A row without enrichments encodes exactly as before (omitempty).
func TestEnrichmentAdditive(t *testing.T) {
	b, _ := json.Marshal(Session{ID: "a", State: StateReady})
	for _, key := range []string{"enrichments", "rollup", "subState"} {
		if strings.Contains(string(b), `"`+key+`"`) {
			t.Errorf("empty row carries %q: %s", key, b)
		}
	}
}
