package main

import (
	"bytes"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/queue"
)

func TestQueueAnswerFields(t *testing.T) {
	f, err := queueAnswerFields("q-1", []string{"--choice", "A", "--choice=x|y"})
	if err != nil || f[core.QueueEntryField] != "q-1" || f[core.QueueChoiceField] != "A\nx|y" {
		t.Fatalf("choices = %v, %v", f, err)
	}
	f, err = queueAnswerFields("g-1", []string{"use", "us-east"})
	if err != nil || f[core.QueueTextField] != "use us-east" {
		t.Fatalf("text = %v, %v", f, err)
	}
	f, err = queueAnswerFields("p-1", []string{"--deny"})
	if err != nil || f[core.SteerDecision] != core.SteerDeny {
		t.Fatalf("deny = %v, %v", f, err)
	}
	f, err = queueAnswerFields("g-1", []string{"--", "--not-a-flag"})
	if err != nil || f[core.QueueTextField] != "--not-a-flag" {
		t.Fatalf("-- text = %v, %v", f, err)
	}
	for _, bad := range [][]string{{}, {"--bogus"}, {"--choice"}} {
		if _, err := queueAnswerFields("q-1", bad); err == nil {
			t.Errorf("queueAnswerFields(%v) accepted", bad)
		}
	}
}

func TestQueueAnswerFieldsAnswers(t *testing.T) {
	f, err := queueAnswerFields("q-1", []string{"--answers", `[{"choice":["a"]},{"other":"x"}]`})
	if err != nil || f[core.QueueAnswersField] != `[{"choice":["a"]},{"other":"x"}]` {
		t.Fatalf("answers = %v, %v", f, err)
	}
	for _, bad := range [][]string{{"--answers", "{"}, {"--answers"}, {"--answers=[]", "--choice", "a"}} {
		if _, err := queueAnswerFields("q-1", bad); err == nil {
			t.Errorf("queueAnswerFields(%v) accepted", bad)
		}
	}
}

func TestQueueAnswerInteractive(t *testing.T) {
	e := queue.Entry{ID: "q-1", Kind: queue.KindQuestion, Questions: []queue.Question{
		{Header: "Scope", Question: "Which parts should the change touch, given that the rollout has to cover every service the billing team owns?", MultiSelect: true,
			Options: []queue.Option{{Label: "api", Description: "the public API"}, {Label: "web"}, {Label: "cli", Preview: "amux inbox"}}},
		{Header: "Ship", Question: "Ship today?", Options: []queue.Option{{Label: "yes"}, {Label: "no"}}},
		{Question: "Anything else?"},
		{Question: "Skipped?", Options: []queue.Option{{Label: "a"}}},
	}}
	// Scope: options 3 and 1 plus words; Ship: an out-of-range number is
	// own text; Anything else: a line; the last is skipped.
	var out bytes.Buffer
	f, err := queueAnswerInteractive(e, strings.NewReader("3, 1 and docs\n7\nnothing more\n\n"), &out, 60)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"choice":["cli","api"],"other":"and docs"},{"other":"7"},{"other":"nothing more"},{}]`
	if f[core.QueueAnswersField] != want || f[core.QueueEntryField] != "q-1" {
		t.Errorf("fields = %v, want answers %s", f, want)
	}
	text := out.String()
	for _, w := range []string{"billing team owns?", "(choose any, or type your own)", "1. api", "the public API", "| amux inbox", "(type an answer)"} {
		if !strings.Contains(text, w) {
			t.Errorf("prompt missing %q:\n%s", w, text)
		}
	}
	for _, l := range strings.Split(text, "\n") {
		if len([]rune(l)) > 80 {
			t.Errorf("unwrapped line %q", l)
		}
	}
	// Nothing answered at all is refused.
	if _, err := queueAnswerInteractive(e, strings.NewReader("\n\n\n\n"), &out, 60); err == nil {
		t.Error("an empty interactive answer was accepted")
	}
	// A permission asks allow or deny until it gets one.
	f, err = queueAnswerInteractive(queue.Entry{ID: "p-1", Kind: queue.KindPermission, Title: "permission: Bash", Body: "rm -rf build"}, strings.NewReader("maybe\nd\n"), &out, 60)
	if err != nil || f[core.SteerDecision] != core.SteerDeny {
		t.Errorf("permission = %v, %v", f, err)
	}
	f, err = queueAnswerInteractive(queue.Entry{ID: "g-1", Kind: queue.KindGoalInput, Title: "Region?"}, strings.NewReader("eu-west\n"), &out, 60)
	if err != nil || f[core.QueueTextField] != "eu-west" {
		t.Errorf("goal = %v, %v", f, err)
	}
}

func TestRenderQueueShowsWholeText(t *testing.T) {
	long := "Should the settings page use tabs, one long scrolling page, or a wizard that walks through each section in order? Consider that most users only change one setting per visit."
	ov := queue.Overview{Entries: []queue.Entry{{ID: "q-1", Kind: queue.KindQuestion, Session: "m1", Title: long,
		Questions: []queue.Question{{Header: "Layout", Question: long, Options: []queue.Option{{Label: "Tabs", Description: strings.Repeat("a long description ", 12) + "end."}}}}}}}
	var b bytes.Buffer
	renderQueue(&b, ov)
	joined := strings.Join(strings.Fields(b.String()), " ")
	for _, want := range []string{"[Layout] " + long, "a long description end.", "(choose one, or type your own)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("render cut %q:\n%s", want, b.String())
		}
	}
}

func TestRenderQueueFlattensAgentText(t *testing.T) {
	ov := queue.Overview{
		Entries: []queue.Entry{
			{ID: "p-1", Kind: queue.KindPermission, Session: "m1", Workgroup: "wg1", Title: "permission: Bash", Body: "rm -rf build"},
			{ID: "q-1", Kind: queue.KindQuestion, Session: "m2", Title: "Which?\x1b[2J\nsecond line", Questions: []queue.Question{{Question: "Which?", Options: []queue.Option{{Label: "A"}}}}},
			{ID: "d-1", Kind: queue.KindDecision, Session: "wg1", Item: "it-00000001", Title: "Rewrite?", Suggest: "new workgroup"},
		},
		Counts:  queue.Counts{Total: 3},
		Inbox:   queue.InboxCounts{Open: 2},
		Summary: queue.Summary{NeedsYou: 1, Workgroups: []queue.WorkgroupSummary{{ID: "wg1", Bucket: queue.BucketNeedsYou}}},
	}
	var b bytes.Buffer
	renderQueue(&b, ov)
	out := b.String()
	for _, want := range []string{"1 need you", "2 open", "amux inbox answer p-1 --allow | --deny", "--choice <label>", "amux inbox ack it-00000001", "needs_you"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "Which?\nsecond") {
		t.Errorf("render passed agent control text through:\n%q", out)
	}
}
