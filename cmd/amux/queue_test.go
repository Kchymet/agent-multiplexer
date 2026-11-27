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
