package nativetui

import (
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/queue"
)

func queueFrame(t *testing.T, ov queue.Overview) frameMsg {
	t.Helper()
	b, err := json.Marshal(ov)
	if err != nil {
		t.Fatal(err)
	}
	return frameMsg{daemon.Frame{Data: &core.Data{Type: core.FrameData, Query: core.QueryQueue, OK: true, Rows: b}}}
}

func testOverview() queue.Overview {
	return queue.Overview{
		Entries: []queue.Entry{
			{ID: "p-1", Kind: queue.KindPermission, Session: "m1", Workgroup: "wg1", Title: "permission: Bash", Body: "rm -rf build"},
			{ID: "q-1", Kind: queue.KindQuestion, Session: "m2", Workgroup: "wg1", Title: "Which DB?\x1b[2J",
				Questions: []queue.Question{{Question: "Which DB?", Options: []queue.Option{{Label: "sqlite"}, {Label: "pg", Description: "server"}}}}},
			{ID: "d-1", Kind: queue.KindDecision, Session: "wg1", Item: "it-00000001", Title: "Rewrite billing?", Suggest: "new workgroup"},
		},
		Counts:  queue.Counts{Total: 3},
		Inbox:   queue.InboxCounts{Open: 2, Acknowledged: 1},
		Summary: queue.Summary{NeedsYou: 1, Running: 2},
	}
}

func TestQueuePanel(t *testing.T) {
	m := resourcesModel()
	m.Update(queueFrame(t, testOverview()))
	if m.qp.ov == nil || len(m.qp.ov.Entries) != 3 {
		t.Fatalf("queue not applied: %+v", m.qp)
	}
	if line := ansi.Strip(m.queueSummaryLine()); !strings.Contains(line, "3 questions") || !strings.Contains(line, "2 inbox") {
		t.Errorf("summary line = %q", line)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("i")})
	if !m.qp.open {
		t.Fatal("i did not open the queue panel")
	}
	out := ansi.Strip(m.renderQueue())
	for _, want := range []string{"Questions for you", "1 need you", "permission", "rm -rf build", "allow", "Rewrite billing?"} {
		if !strings.Contains(out, want) {
			t.Errorf("panel missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(m.renderQueue(), "\x1b[2J") {
		t.Error("panel passed an agent's control sequence through")
	}

	// Move to the question: its options are listed; r opens a reply form
	// that carries the entry id.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if e, _ := m.qp.selectedEntry(); e.ID != "q-1" {
		t.Fatalf("selected %q, want q-1", e.ID)
	}
	if out := ansi.Strip(m.renderQueue()); !strings.Contains(out, "2. pg") || !strings.Contains(out, "1-9") {
		t.Errorf("question options not shown:\n%s", out)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	if m.qp.answer == nil || m.qp.answer.entry.ID != "q-1" {
		t.Fatalf("r opened %+v", m.qp.answer)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	if f, err := m.qp.answer.fields(); err != nil || f[core.QueueAnswersField] != `[{"choice":["pg"]}]` {
		t.Fatalf("answer fields = %v, %v", f, err)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc}) // back to the list

	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.qp.open {
		t.Fatal("esc did not close the queue panel")
	}
}
