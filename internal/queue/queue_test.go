package queue

import (
	"encoding/json"
	"strings"
	"testing"

	"amux/internal/core"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestEntryIDIsDeterministicAndDistinct(t *testing.T) {
	a := EntryID(KindQuestion, "s1", "gen1", "toolu_1")
	if a != EntryID(KindQuestion, "s1", "gen1", "toolu_1") {
		t.Fatal("entry id is not deterministic")
	}
	if !strings.HasPrefix(a, "q-") {
		t.Fatalf("question id %q lacks its kind prefix", a)
	}
	for _, other := range []string{
		EntryID(KindQuestion, "s1", "gen2", "toolu_1"), // a new runtime: a new question
		EntryID(KindQuestion, "s1", "gen1", "toolu_2"), // a new call
		EntryID(KindPermission, "s1", "gen1", "toolu_1"),
		EntryID(KindQuestion, "s1g", "en1", "toolu_1"), // parts are delimited
	} {
		if other == a {
			t.Errorf("distinct question shares id %q", a)
		}
	}
}

func TestParseQuestions(t *testing.T) {
	raw := json.RawMessage(`{"questions":[{"header":"Scope","question":"Which?","options":[{"label":"A","description":"first"},{"label":"B"}],"multiSelect":false},{"question":"  "}]}`)
	qs, ok := ParseQuestions(raw)
	if !ok || len(qs) != 1 || qs[0].Header != "Scope" || len(qs[0].Options) != 2 || qs[0].Options[0].Description != "first" {
		t.Fatalf("ParseQuestions = %+v, %v", qs, ok)
	}
	for _, bad := range []string{``, `{}`, `{"questions":[]}`, `[1]`} {
		if _, ok := ParseQuestions(json.RawMessage(bad)); ok {
			t.Errorf("ParseQuestions(%q) ok", bad)
		}
	}
}

func TestFormatAnswerMatchesThePhoneCard(t *testing.T) {
	one := []Question{{Question: "Which?", Options: []Option{{Label: "A"}, {Label: "B"}}}}
	if got := FormatAnswer(one, [][]string{{"B"}}, ""); got != "B" {
		t.Errorf("lone question = %q", got)
	}
	if got := FormatAnswer(one, nil, " free text "); got != "free text" {
		t.Errorf("free text = %q", got)
	}
	two := []Question{{Header: "Scope", Question: "Which?"}, {Question: "Colors?", MultiSelect: true}}
	if got := FormatAnswer(two, [][]string{{"A"}, {"red", "blue"}}, ""); got != "Scope: A\nColors?: red, blue" {
		t.Errorf("several questions = %q", got)
	}
}

func TestChoices(t *testing.T) {
	qs := []Question{
		{Question: "One", Options: []Option{{Label: "A"}, {Label: "B"}}},
		{Question: "Many", MultiSelect: true, Options: []Option{{Label: "x"}, {Label: "y"}}},
	}
	c := ParseChoices("A\nx | y")
	if len(c) != 2 || c[0][0] != "A" || len(c[1]) != 2 || c[1][1] != "y" {
		t.Fatalf("ParseChoices = %v", c)
	}
	if !ValidChoices(qs, c) {
		t.Error("valid choices refused")
	}
	for _, bad := range [][][]string{{{"C"}}, {{"A", "B"}}, {{"A"}, {"x"}, {"y"}}} {
		if ValidChoices(qs, bad) {
			t.Errorf("ValidChoices(%v) accepted", bad)
		}
	}
}

func TestCountAndSort(t *testing.T) {
	es := []Entry{
		{ID: "d-1", Kind: KindDecision, Created: 1},
		{ID: "q-2", Kind: KindQuestion, Created: 5},
		{ID: "p-3", Kind: KindPermission, Created: 9},
		{ID: "q-1", Kind: KindQuestion, Created: 2},
		{ID: "g-1", Kind: KindGoalInput, Dismissed: true},
	}
	Sort(es)
	var order []string
	for _, e := range es {
		order = append(order, e.ID)
	}
	if got := strings.Join(order, ","); got != "p-3,q-1,q-2,g-1,d-1" {
		t.Errorf("sorted = %s", got)
	}
	c := Count(es)
	if c.Total != 4 || c.ByKind[KindQuestion] != 2 || c.ByKind[KindGoalInput] != 0 {
		t.Errorf("Count = %+v", c)
	}
}

func TestSummarizeBuckets(t *testing.T) {
	open := harnessproto.SessionPRs{{Repo: "o/r", Number: 7, State: "open"}}
	merged := harnessproto.SessionPRs{{Repo: "o/r", Number: 6, State: "merged"}}
	rows := []core.Session{
		{ID: "needs", Title: "Needs", IsRoot: true, Role: "coordinator", State: core.StateWaiting,
			Supervision: &harnessproto.Supervision{NeedsYou: []string{"m1"}}},
		{ID: "m1", RootID: "needs", State: core.StateWaiting},
		{ID: "run", IsRoot: true, Role: "coordinator", State: core.StateReady},
		{ID: "m2", RootID: "run", State: core.StateRunning},
		{ID: "rev", IsRoot: true, Role: "coordinator", State: core.StateReady},
		{ID: "m3", RootID: "rev", State: core.StateReady, PRs: &open},
		{ID: "done", IsRoot: true, Role: "coordinator", State: core.StateIdle, Supervision: &harnessproto.Supervision{Goal: "complete"}},
		{ID: "m4", RootID: "done", State: core.StateIdle, PRs: &merged},
		{ID: "queued", IsRoot: true, Role: "coordinator", State: core.StateReady},
		{ID: "old", IsRoot: true, Role: "coordinator", Archived: true, Section: core.SectionArchived},
		{ID: "console", Role: "console"},
		{ID: "api", IsRoot: true, Role: "repo"},
	}
	s := Summarize(rows, []Entry{{ID: "q", Workgroup: "queued"}, {ID: "x", Workgroup: "run", Dismissed: true}})
	if s.NeedsYou != 2 || s.Running != 1 || s.Review != 1 || s.Done != 1 || len(s.Workgroups) != 5 {
		t.Fatalf("Summarize = %+v", s)
	}
	buckets := map[string]string{}
	for _, w := range s.Workgroups {
		buckets[w.ID] = w.Bucket
	}
	want := map[string]string{"needs": BucketNeedsYou, "run": BucketRunning, "rev": BucketReview, "done": BucketDone, "queued": BucketNeedsYou}
	for id, b := range want {
		if buckets[id] != b {
			t.Errorf("%s bucket = %q, want %q", id, buckets[id], b)
		}
	}
	for _, w := range s.Workgroups {
		if w.ID == "rev" && (len(w.OpenPRs) != 1 || w.OpenPRs[0] != "o/r#7") {
			t.Errorf("review open PRs = %v", w.OpenPRs)
		}
	}
}
