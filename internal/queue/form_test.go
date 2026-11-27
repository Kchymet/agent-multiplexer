package queue

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

const askInput = `{"questions":[
 {"header":"Layout","question":"Which layout?","multiSelect":false,"options":[
   {"label":"Tabs","description":"One tab per area","preview":"┌Tabs┐"},{"label":"Single page","description":"Long scroll"}]},
 {"header":"Extras","question":"Which extras?","multiSelect":true,"options":[
   {"label":"Search","preview":"ignored on multi"},{"label":"Dark mode"},{"label":"A, B"}]}],
 "metadata":{"source":"x"}}`

func TestClaudeForm(t *testing.T) {
	f, ok := ClaudeForm(json.RawMessage(askInput))
	if !ok {
		t.Fatal("not parsed")
	}
	if f.Version != harnessproto.FormVersion || f.Source != harnessproto.FormSourceClaudeAsk || !f.Cancel || !f.Reply {
		t.Fatalf("envelope %+v", f)
	}
	if len(f.Questions) != 2 || f.Questions[0].ID != "q0" || f.Questions[1].ID != "q1" {
		t.Fatalf("questions %+v", f.Questions)
	}
	q0, q1 := f.Questions[0], f.Questions[1]
	if !q0.Other || !q0.Notes || q0.Options[0].Preview != "┌Tabs┐" || q0.Options[0].Description != "One tab per area" {
		t.Fatalf("q0 %+v", q0)
	}
	if !q1.MultiSelect || q1.Options[0].Preview != "" {
		t.Fatalf("a multi-select question carries no previews: %+v", q1)
	}
	if _, ok := ClaudeForm(json.RawMessage(`{"questions":[]}`)); ok {
		t.Fatal("empty input parsed")
	}
	if _, ok := ClaudeForm(json.RawMessage(`not json`)); ok {
		t.Fatal("bad input parsed")
	}
}

func TestProjectLegacy(t *testing.T) {
	f, _ := ClaudeForm(json.RawMessage(askInput))
	e := Entry{Kind: KindQuestion, Title: "Which layout? (+1 more)", Form: &f}
	ProjectLegacy(&e)
	if len(e.Questions) != 2 || e.Questions[0].Options[0].Label != "Tabs" || !e.Questions[1].MultiSelect {
		t.Fatalf("legacy questions %+v", e.Questions)
	}
	if !strings.Contains(e.Body, "previews are hidden") {
		t.Fatalf("body %q", e.Body)
	}
	b, _ := json.Marshal(e.Questions)
	if strings.Contains(string(b), "preview") {
		t.Fatalf("legacy questions leak previews: %s", b)
	}

	s := f
	s.Questions = append([]harnessproto.FormQuestion(nil), f.Questions...)
	s.Questions[1].Secret = true
	e2 := Entry{Kind: KindQuestion, Title: "T", Form: &s, Questions: []Question{{Question: "stale"}}}
	ProjectLegacy(&e2)
	if e2.Questions != nil || e2.Title != LegacyOmittedTitle+"T" {
		t.Fatalf("secret form shown to legacy: %+v", e2)
	}
	ProjectLegacy(&e2)
	if e2.Title != LegacyOmittedTitle+"T" {
		t.Fatalf("title prefixed twice: %q", e2.Title)
	}
}

func TestLegacyAnswer(t *testing.T) {
	f, _ := ClaudeForm(json.RawMessage(askInput))
	one := f
	one.Questions = f.Questions[:1]

	a, err := LegacyAnswer(f, [][]string{{"Single page"}, {"Search", "A, B"}}, "")
	if err != nil || a.Action != harnessproto.FormActionAnswer || a.Answers[0].Selected[0] != 1 || len(a.Answers[1].Selected) != 2 || a.Answers[1].Selected[1] != 2 {
		t.Fatalf("labels: %+v %v", a, err)
	}
	a, err = LegacyAnswer(one, nil, "  a grid  ")
	if err != nil || a.Answers[0].Other == nil || *a.Answers[0].Other != "a grid" {
		t.Fatalf("one-question text: %+v %v", a, err)
	}
	a, err = LegacyAnswer(f, nil, "Layout: tabs\nExtras: none")
	if err != nil || a.Action != harnessproto.FormActionCancel || a.Message != "Layout: tabs\nExtras: none" {
		t.Fatalf("multi-question text becomes a reply: %+v %v", a, err)
	}
	for name, c := range map[string]struct {
		choices [][]string
		text    string
	}{
		"mixed":         {[][]string{{"Tabs"}}, "x"},
		"partial":       {[][]string{{"Tabs"}}, ""},
		"empty slot":    {[][]string{{"Tabs"}, {}}, ""},
		"unknown label": {[][]string{{"Grid"}, {"Search"}}, ""},
		"nothing":       {nil, ""},
	} {
		if _, err := LegacyAnswer(f, c.choices, c.text); !errors.Is(err, ErrLegacyShape) {
			t.Errorf("%s: want ErrLegacyShape, got %v", name, err)
		}
	}
	if _, err := LegacyAnswer(f, [][]string{{"Tabs", "Single page"}, {"Search"}}, ""); !errors.Is(err, harnessproto.ErrInvalidAnswer) {
		t.Errorf("two labels on a single-select: %v", err)
	}
	s := f
	s.Questions = append([]harnessproto.FormQuestion(nil), f.Questions...)
	s.Questions[0].Secret = true
	if _, err := LegacyAnswer(s, [][]string{{"Tabs"}, {"Search"}}, ""); !errors.Is(err, ErrNeedsNewerClient) {
		t.Errorf("secret: %v", err)
	}
	codex := harnessproto.Form{Version: 2, Source: harnessproto.FormSourceCodexUserInput, Questions: []harnessproto.FormQuestion{
		{ID: "env", Question: "Which env?", Options: []harnessproto.FormOption{{Label: "prod"}, {Label: "dev"}}},
		{ID: "why", Question: "Why?", Other: true},
	}}
	if _, err := LegacyAnswer(codex, nil, "because"); !errors.Is(err, ErrLegacyShape) {
		t.Errorf("a reply-less form refuses a multi-question text: %v", err)
	}
	c1 := codex
	c1.Questions = codex.Questions[:1]
	if _, err := LegacyAnswer(c1, nil, "staging"); !errors.Is(err, ErrLegacyShape) {
		t.Errorf("a question without Other refuses text: %v", err)
	}
}

func TestMatchReply(t *testing.T) {
	f, _ := ClaudeForm(json.RawMessage(askInput))
	one := f
	one.Questions = f.Questions[:1]
	multi := f
	multi.Questions = f.Questions[1:]

	if a, ok := MatchReply(one, "Single page"); !ok || a.Answers[0].Selected[0] != 1 {
		t.Fatalf("lone label: %+v %v", a, ok)
	}
	if a, ok := MatchReply(multi, "Search, Dark mode"); !ok || len(a.Answers[0].Selected) != 2 {
		t.Fatalf("multi labels: %+v %v", a, ok)
	}
	if a, ok := MatchReply(multi, "A, B"); !ok || len(a.Answers[0].Selected) != 1 || a.Answers[0].Selected[0] != 2 {
		t.Fatalf("label holding a comma: %+v %v", a, ok)
	}
	if a, ok := MatchReply(f, "Layout: Tabs\nExtras: Search, A, B"); !ok || len(a.Answers[1].Selected) != 2 {
		t.Fatalf("questionReply lines: %+v %v", a, ok)
	}
	// FormatAnswer's own rendering round-trips.
	text := FormatAnswer(LegacyQuestions(f), [][]string{{"Tabs"}, {"Dark mode"}}, "")
	if _, ok := MatchReply(f, text); !ok {
		t.Fatalf("FormatAnswer rendering %q not recognized", text)
	}
	for _, s := range []string{"tabs", "Single page please", "Tabs, Single page", "Layout: Tabs", "Extras: Search\nLayout: Tabs", ""} {
		if _, ok := MatchReply(f, s); ok && s != "" {
			t.Errorf("%q matched", s)
		}
	}
	if _, ok := MatchReply(one, "Tabs, Single page"); ok {
		t.Error("two labels on a single-select matched")
	}
}
