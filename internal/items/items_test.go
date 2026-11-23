package items

import (
	"strings"
	"testing"
)

func TestRaiseValidateNormalizes(t *testing.T) {
	got, err := Raise{
		Kind: " follow_up ", Title: "  Add docs  ", Detail: "line one\n\tline two ",
		PRs:   []string{"Kchymet/agent-multiplexer#12", " repo#3 ", "repo#3", ""},
		Links: []string{"https://example.com/x?a=1,2"}, Suggest: "do it",
	}.Validate()
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindFollowUp || got.Title != "Add docs" || got.Detail != "line one\n\tline two" {
		t.Fatalf("normalized = %+v", got)
	}
	if strings.Join(got.PRs, " ") != "Kchymet/agent-multiplexer#12 repo#3" {
		t.Fatalf("prs = %q", got.PRs)
	}
}

func TestRaiseValidateRejects(t *testing.T) {
	long := strings.Repeat("x", MaxTitleRunes+1)
	many := make([]string, MaxRefs+1)
	for i := range many {
		many[i] = "r#" + string(rune('1'+i%9)) + strings.Repeat("0", i)
	}
	cases := map[string]Raise{
		"unknown kind":      {Kind: "todo", Title: "x"},
		"empty title":       {Kind: KindRisk, Title: "   "},
		"multiline title":   {Kind: KindRisk, Title: "a\nb"},
		"control character": {Kind: KindRisk, Title: "a\x1b[31mb"},
		"long title":        {Kind: KindRisk, Title: long},
		"bad pr":            {Kind: KindRisk, Title: "x", PRs: []string{"not a pr"}},
		"pr zero":           {Kind: KindRisk, Title: "x", PRs: []string{"repo#0"}},
		"bad link scheme":   {Kind: KindRisk, Title: "x", Links: []string{"javascript:alert(1)"}},
		"link no host":      {Kind: KindRisk, Title: "x", Links: []string{"https://"}},
		"too many prs":      {Kind: KindRisk, Title: "x", PRs: many},
		"detail control":    {Kind: KindRisk, Title: "x", Detail: "a\rb"},
	}
	for name, r := range cases {
		if _, err := r.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, r)
		}
	}
}

func TestDedupeKey(t *testing.T) {
	if a, b := DedupeKey("Add  the Docs."), DedupeKey("add the docs"); a != b {
		t.Fatalf("keys differ: %q vs %q", a, b)
	}
	if DedupeKey("Add docs") == DedupeKey("Add tests") {
		t.Fatal("distinct titles share a key")
	}
}

func TestTransitions(t *testing.T) {
	cases := []struct {
		from, to string
		ok       bool
	}{
		{StateOpen, StateAcknowledged, true},
		{StateAcknowledged, StateAcknowledged, false},
		{StateOpen, StateDone, true},
		{StateAcknowledged, StateDismissed, true},
		{StateDone, StateDismissed, false},
		{StateDone, StateOpen, true},
		{StateDismissed, StateOpen, true},
		{StateAcknowledged, StateOpen, true},
		{StateOpen, StateOpen, false},
		{StateOpen, "bogus", false},
	}
	for _, c := range cases {
		if got := CanTransition(c.from, c.to); got != c.ok {
			t.Errorf("CanTransition(%s, %s) = %v, want %v", c.from, c.to, got, c.ok)
		}
	}
	for _, v := range Verbs() {
		if _, ok := TargetState(v); !ok {
			t.Errorf("verb %q has no target state", v)
		}
	}
}

func TestHistoryKeepsRaiseAndNewest(t *testing.T) {
	var it Item
	it.AddEvent(Event{Action: EventRaised, At: 0})
	for i := 1; i <= MaxHistory+5; i++ {
		it.AddEvent(Event{Action: EventUpdated, At: int64(i)})
	}
	if len(it.History) != MaxHistory || it.History[0].Action != EventRaised || it.History[len(it.History)-1].At != MaxHistory+5 {
		t.Fatalf("history = %d entries, first %+v, last %+v", len(it.History), it.History[0], it.History[len(it.History)-1])
	}
}

func TestIDs(t *testing.T) {
	id := NewID()
	if !ValidID(id) {
		t.Fatalf("NewID %q is not valid", id)
	}
	for _, bad := range []string{"", "it-", "it-XYZ12345", "it-1234567", "xx-12345678", "it-ABCDEF12"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
}
