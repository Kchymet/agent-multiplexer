package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/agent"
	"amux/internal/claudegoal"
	"amux/internal/core"
	"amux/internal/items"
	"amux/internal/queue"
	"amux/internal/store"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func TestClaudeOpenQuestionsIn(t *testing.T) {
	dir := t.TempDir()
	write := func(lines ...string) string {
		p := filepath.Join(dir, "t.jsonl")
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	const (
		prompt   = `{"type":"user","message":{"role":"user","content":"pick one"}}`
		ask      = `{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"AskUserQuestion","input":{"questions":[{"header":"Pick","question":"Which?","options":[{"label":"A","description":"first"},{"label":"B"}]}]}}],"stop_reason":"tool_use"}}`
		answered = `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"User answered: A"}]}}`
		next     = `{"type":"user","message":{"role":"user","content":"never mind"}}`
	)
	open := claudeOpenQuestionsIn(write(prompt, ask))
	if len(open) != 1 || open[0].ToolUseID != "tu1" || len(open[0].Questions) != 1 ||
		open[0].Questions[0].Header != "Pick" || len(open[0].Questions[0].Options) != 2 {
		t.Fatalf("open questions = %+v", open)
	}
	if got := claudeOpenQuestionsIn(write(prompt, ask, answered)); len(got) != 0 {
		t.Fatalf("answered question still open: %+v", got)
	}
	if got := claudeOpenQuestionsIn(write(prompt, ask, next)); len(got) != 0 {
		t.Fatalf("superseded question still open: %+v", got)
	}
}

// fakeQueueDaemon is a daemon whose queue reads canned sources.
func fakeQueueDaemon(t *testing.T, rows []core.Session, perms map[string][]queuePermission, qs map[string][]openQuestion,
	goals map[string]claudegoal.State, inbox []items.Item) *Daemon {
	t.Helper()
	d := &Daemon{}
	d.queueSourcesFn = func() queueSources {
		return queueSources{
			rows: func() []core.Session { return rows },
			lookup: func(id string) (store.Session, bool, error) {
				for _, r := range rows {
					if r.ID == id {
						s := store.Session{ID: id, Agent: harnessproto.RuntimeClaude, Dir: "/x/" + id, RootID: r.RootID}
						if r.Role == store.RoleCoordinator {
							s.RootID, s.Scope = "", store.ScopeWork
						}
						return s, true, nil
					}
				}
				return store.Session{}, false, nil
			},
			questions: func(_ agent.Harness, s store.Session) []openQuestion { return qs[s.ID] },
			perms:     func(id string, _ store.Session) []queuePermission { return perms[id] },
			goal:      func(s store.Session) (claudegoal.State, error) { return goals[s.ID], nil },
			inbox:     func() ([]items.Item, error) { return inbox, nil },
		}
	}
	return d
}

func TestQueueDerivation(t *testing.T) {
	rows := []core.Session{
		{ID: "wg1", Title: "Billing", IsRoot: true, Role: store.RoleCoordinator, State: core.StateWaiting,
			Supervision: &harnessproto.Supervision{Goal: claudegoal.StatusInput, NeedsYou: []string{"coordinator", "m1"}}},
		{ID: "m1", Title: "api work", RootID: "wg1", State: core.StateWaiting},
		{ID: "m2", RootID: "wg1", State: core.StateRunning},
		{ID: "wg2", IsRoot: true, Role: store.RoleCoordinator, State: core.StateWaiting,
			Supervision: &harnessproto.Supervision{Goal: claudegoal.StatusInput}},
		{ID: "gone", RootID: "wg1", State: core.StateWaiting, Archived: true, Section: core.SectionArchived},
	}
	question := openQuestion{ToolUseID: "tu1", Questions: []queue.Question{{Question: "Which DB?", Options: []queue.Option{{Label: "sqlite"}, {Label: "pg"}}}}}
	d := fakeQueueDaemon(t, rows,
		map[string][]queuePermission{"m1": {{RequestID: "r1", Generation: "g7", Tool: "Bash", Action: "rm -rf build"}}},
		map[string][]openQuestion{"m1": {question}, "wg2": {{ToolUseID: "tu9", Questions: []queue.Question{{Question: "Archive this workgroup?"}}}}, "gone": {question}},
		map[string]claudegoal.State{"wg1": {Status: claudegoal.StatusInput, Note: "Which region?", Updated: 5}, "wg2": {Status: claudegoal.StatusInput, Note: "Archive this workgroup?"}},
		[]items.Item{
			{ID: "it-1", Kind: items.KindDecisionNeeded, Title: "Rewrite billing?", Source: "wg1", Workgroup: "wg1", State: items.StateOpen, Suggest: "new workgroup"},
			{ID: "it-2", Kind: items.KindRisk, Title: "Flaky CI", State: items.StateAcknowledged},
		})
	ov := d.queueOverview(nil)
	kinds := map[string]int{}
	for _, e := range ov.Entries {
		kinds[e.Kind]++
		if e.Session == "gone" {
			t.Errorf("archived session in the queue: %+v", e)
		}
	}
	// m1: a permission and a question; wg1: its goal ask; wg2: its question
	// only (its goal ask for the same thing collapses into it); one decision.
	if kinds[queue.KindPermission] != 1 || kinds[queue.KindQuestion] != 2 || kinds[queue.KindGoalInput] != 1 || kinds[queue.KindDecision] != 1 {
		t.Fatalf("entry kinds = %v (%+v)", kinds, ov.Entries)
	}
	if ov.Entries[0].Kind != queue.KindPermission || ov.Entries[0].RuntimeGeneration != "g7" || ov.Entries[0].Workgroup != "wg1" {
		t.Errorf("first entry = %+v, want m1's bound permission", ov.Entries[0])
	}
	if ov.Counts.Total != 5 || ov.Inbox.Open != 1 || ov.Inbox.Acknowledged != 1 {
		t.Errorf("counts = %+v inbox = %+v", ov.Counts, ov.Inbox)
	}
	if ov.Summary.NeedsYou != 2 {
		t.Errorf("summary = %+v", ov.Summary)
	}
	// Ids are stable across reads.
	again := d.queueOverview(nil)
	for i := range ov.Entries {
		if again.Entries[i].ID != ov.Entries[i].ID {
			t.Fatalf("entry ids changed between reads: %s vs %s", ov.Entries[i].ID, again.Entries[i].ID)
		}
	}
}

func TestQueueDismissAndStaleAnswers(t *testing.T) {
	rows := []core.Session{{ID: "m1", RootID: "wg1", State: core.StateWaiting}}
	qs := map[string][]openQuestion{"m1": {{ToolUseID: "tu1", Questions: []queue.Question{{Question: "Which?", Options: []queue.Option{{Label: "A"}}}}}}}
	d := fakeQueueDaemon(t, rows, nil, qs, nil, nil)
	ov := d.queueOverview(nil)
	if len(ov.Entries) != 1 {
		t.Fatalf("entries = %+v", ov.Entries)
	}
	id := ov.Entries[0].ID
	ctx := context.Background()

	// A choice that is not an option is refused before anything is delivered.
	res := d.handle(ctx, core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: id, core.QueueChoiceField: "Z"}})
	if res.OK {
		t.Fatal("an unknown choice was accepted")
	}

	// Dismissing hides a live question until it changes; --all still lists it.
	if res := d.handle(ctx, core.Action{Action: core.ActionQueueDismiss, Fields: map[string]string{core.QueueEntryField: id}}); !res.OK {
		t.Fatalf("dismiss = %+v", res)
	}
	if ov := d.queueOverview(nil); len(ov.Entries) != 0 {
		t.Fatalf("dismissed entry still listed: %+v", ov.Entries)
	}
	if ov := d.queueOverview(map[string]string{core.QueueAllField: "1"}); len(ov.Entries) != 1 || !ov.Entries[0].Dismissed {
		t.Fatalf("--all = %+v", ov.Entries)
	}

	// The question goes away (answered elsewhere): an answer is asker_gone…
	delete(qs, "m1")
	res = d.handle(ctx, core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: id, core.QueueChoiceField: "A"}})
	if res.OK || res.Code != codeAskerGone {
		t.Fatalf("stale answer = %+v, want %s", res, codeAskerGone)
	}
	// …unless amux itself just delivered an answer to it.
	d.queue.markAnswered(id, time.Now())
	res = d.handle(ctx, core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: id, core.QueueTextField: "x"}})
	if res.OK || res.Code != codeAnswered {
		t.Fatalf("second answer = %+v, want %s", res, codeAnswered)
	}
	if res := d.handle(ctx, core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{}}); res.OK {
		t.Fatal("an answer without an entry was accepted")
	}
}

func TestQueuePermissionNeedsDecisionAndGeneration(t *testing.T) {
	rows := []core.Session{{ID: "m1", RootID: "wg1", State: core.StateWaiting}}
	perms := map[string][]queuePermission{"m1": {{RequestID: "r1"}}} // no live runtime to bind
	d := fakeQueueDaemon(t, rows, perms, nil, nil, nil)
	id := d.queueOverview(nil).Entries[0].ID
	ctx := context.Background()
	if res := d.handle(ctx, core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: id, core.QueueTextField: "yes"}}); res.OK {
		t.Fatal("a permission answered without allow/deny")
	}
	res := d.handle(ctx, core.Action{Action: core.ActionQueueAnswer, Fields: map[string]string{core.QueueEntryField: id, core.SteerDecision: core.SteerAllow}})
	if res.OK || res.Code != codeAskerGone {
		t.Fatalf("unbound permission answer = %+v, want %s", res, codeAskerGone)
	}
}

func TestOverseerGoalUpdate(t *testing.T) {
	cases := []struct {
		prev, cur string
		wake      bool
	}{
		{"", claudegoal.StatusInput, false}, // first sight: a baseline
		{claudegoal.StatusActive, claudegoal.StatusInput, true},
		{claudegoal.StatusActive, claudegoal.StatusBlocked, true},
		{claudegoal.StatusWaiting, claudegoal.StatusComplete, true},
		{claudegoal.StatusInput, claudegoal.StatusInput, false},
		{claudegoal.StatusInput, claudegoal.StatusActive, false},
	}
	for _, c := range cases {
		what, wake := overseerGoalUpdate(c.prev, c.cur, "note")
		if wake != c.wake {
			t.Errorf("%s -> %s: wake=%v (%q), want %v", c.prev, c.cur, wake, what, c.wake)
		}
	}
}

func TestHostAcknowledgedResolver(t *testing.T) {
	acked := items.Item{ID: "it-00000001", State: items.StateAcknowledged, History: []items.Event{{Action: items.EventAcknowledged, Host: true}}}
	bySession := items.Item{ID: "it-00000002", State: items.StateAcknowledged, History: []items.Event{{Action: items.EventAcknowledged, By: "overseer"}}}
	r := &daemonAccessResolver{itemLookup: func(id string) (items.Item, bool, error) {
		switch id {
		case acked.ID:
			return acked, true, nil
		case bySession.ID:
			return bySession, true, nil
		}
		return items.Item{}, false, nil
	}}
	for id, want := range map[string]bool{acked.ID: true, bySession.ID: false, "it-00000003": false, "../etc": false} {
		got, err := r.HostAcknowledged(context.Background(), id)
		if err != nil || got != want {
			t.Errorf("HostAcknowledged(%q) = %v, %v; want %v", id, got, err, want)
		}
	}
}
