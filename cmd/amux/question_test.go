package main

import (
	"bytes"
	"strings"
	"testing"

	"amux/internal/queue"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func cliForm() harnessproto.Form {
	return harnessproto.Form{Version: 2, Source: harnessproto.FormSourceClaudeAsk, Cancel: true, Reply: true, Delivery: harnessproto.DeliveryStructured,
		Questions: []harnessproto.FormQuestion{
			{ID: "q0", Header: "Layout", Question: "Which layout?", Other: true, Notes: true,
				Options: []harnessproto.FormOption{{Label: "Tabs", Preview: "[tabs]\x1b[31m"}, {Label: "2"}}},
			{ID: "q1", Header: "Extras", Question: "Which extras?", MultiSelect: true, Other: true,
				Options: []harnessproto.FormOption{{Label: "Search"}, {Label: "Dark mode"}, {Label: "Export"}}},
		}}
}

func TestParseQuestionAnswer(t *testing.T) {
	f := cliForm()
	a, err := parseQuestionAnswer(f, []string{"--q", "Layout", "--select", "Tabs", "--note", "keep icons",
		"--q", "2", "--select", "2", "--select=Export", "--other", "-"}, strings.NewReader("keyboard map\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Answers) != 2 || a.Answers[0].Selected[0] != 0 || a.Answers[0].Note != "keep icons" ||
		len(a.Answers[1].Selected) != 2 || a.Answers[1].Selected[0] != 1 || *a.Answers[1].Other != "keyboard map" {
		t.Fatalf("answer = %+v", a)
	}
	// An exact label beats an option number: "2" is the label of option 2.
	a, err = parseQuestionAnswer(f, []string{"--q", "q0", "--select", "2", "--q", "Extras", "--select", "1"}, nil)
	if err != nil || a.Answers[0].Selected[0] != 1 {
		t.Fatalf("label vs number: %+v %v", a, err)
	}
	one := f
	one.Questions = f.Questions[:1]
	if a, err := parseQuestionAnswer(one, []string{"--other", "a grid"}, nil); err != nil || *a.Answers[0].Other != "a grid" {
		t.Fatalf("one-question form without --q: %+v %v", a, err)
	}
	for name, args := range map[string][]string{
		"no --q on a two-question form": {"--select", "Tabs"},
		"unknown option":                {"--q", "1", "--select", "Grid", "--q", "2", "--select", "1"},
		"unknown question":              {"--q", "9", "--select", "1"},
		"unanswered question":           {"--q", "1", "--select", "1"},
		"two on single-select":          {"--q", "1", "--select", "1", "--select", "2", "--q", "2", "--select", "1"},
		"note where none is taken":      {"--q", "1", "--select", "1", "--q", "2", "--select", "1", "--note", "x"},
		"unknown flag":                  {"--bogus"},
		"missing value":                 {"--q"},
	} {
		if _, err := parseQuestionAnswer(f, args, nil); err == nil || !isUsageError(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestQuestionExitCodes(t *testing.T) {
	for code, want := range map[string]int{"answered": 3, "asker_gone": 4, "delivery_failed": 5, "answer_in_pane": 5, "needs_newer_client": 6, "": 1} {
		if got := questionExitCode(code); got != want {
			t.Errorf("%q -> %d, want %d", code, got, want)
		}
	}
}

func TestRenderQuestion(t *testing.T) {
	f := cliForm()
	e := queue.Entry{ID: "q-aaaaaaaaaa", Session: "m1", SessionTitle: "api\nwork", Form: &f}
	var short, full bytes.Buffer
	renderQuestion(&short, e, false)
	renderQuestion(&full, e, true)
	if strings.Contains(short.String(), "[tabs]") || !strings.Contains(full.String(), "│ [tabs][31m") {
		t.Fatalf("previews: short %q full %q", short.String(), full.String())
	}
	if strings.Contains(full.String(), "\x1b") || !strings.Contains(short.String(), "api work") {
		t.Fatalf("untrusted text not flattened: %q", full.String())
	}
	if !strings.Contains(short.String(), "2. [Extras] Which extras?  (pick any)") || !strings.Contains(short.String(), "answers go straight to the agent") {
		t.Fatalf("render = %s", short.String())
	}
}
