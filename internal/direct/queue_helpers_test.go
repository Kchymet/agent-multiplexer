package direct

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/items"
	"amux/internal/queue"

	"github.com/gorilla/websocket"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// queueFake is a fakeBackend that also serves the question queue. It is a
// separate type so every other test's fake keeps predating the queue.
type queueFake struct {
	*fakeBackend
	qmu      sync.Mutex
	overview queue.Overview
	inbox    []items.Item
	byItem   map[string]items.Item
	// rejectCode, when set, makes the next queue/item Apply fail with that
	// daemon code (asker_gone, answered).
	rejectCode string
}

func (q *queueFake) Queue(ctx context.Context) (queue.Overview, error) {
	if err := ctx.Err(); err != nil {
		return queue.Overview{}, err
	}
	q.qmu.Lock()
	defer q.qmu.Unlock()
	return q.overview, nil
}

func (q *queueFake) InboxItems(ctx context.Context) ([]items.Item, error) {
	q.qmu.Lock()
	defer q.qmu.Unlock()
	return q.inbox, nil
}

func (q *queueFake) Item(ctx context.Context, id string) (items.Item, bool, error) {
	q.qmu.Lock()
	defer q.qmu.Unlock()
	it, ok := q.byItem[id]
	return it, ok, nil
}

func (q *queueFake) Apply(ctx context.Context, a core.Action) (string, error) {
	q.qmu.Lock()
	code := q.rejectCode
	q.rejectCode = ""
	q.qmu.Unlock()
	if code != "" && (a.Action == core.ActionQueueAnswer || a.Action == core.ActionItemState) {
		q.applies.Add(1)
		return "", &daemon.RejectedError{Msg: "rejected", Code: code}
	}
	return q.fakeBackend.Apply(ctx, a)
}

// Queue fixture ids (queue.EntryID shapes).
const (
	qPermission = "p-aaaaaaaaaa"
	qQuestion   = "q-bbbbbbbbbb"
	qMulti      = "q-cccccccccc"
	qGoal       = "g-dddddddddd"
	qDecision   = "d-eeeeeeeeee"
	qConsole    = "q-ffffffffff" // raised by the admin console: never shown
	qOther      = "g-gggggggggg" // workgroup wg3
	itDecision  = "it-0000000a"
	itRisk      = "it-0000000b"
	itConsole   = "it-0000000c"
	itOther     = "it-0000000d"
)

// queueRows is the inventory the queue tests use: workgroups wg1 (member a1)
// and wg3 (member c1), the overseer and the admin console.
func queueRows() []core.Session {
	caps := &core.SessionCaps{Prompt: true, Interject: true, Cancel: true}
	open := hp.SessionPRs{{Repo: "acme/api", Number: 12, State: "open", URL: "https://github.com/acme/api/pull/12"}}
	return []core.Session{
		{ID: "wg1", Title: "Billing", IsRoot: true, Role: hp.RoleCoordinator, Section: hp.SectionWorkgroups, Caps: caps, Runtime: "claude",
			Source: "workspace", State: core.StateWaiting, Supervision: &hp.Supervision{NeedsYou: []string{"a1"}, Goal: "active"}},
		{ID: "a1", Title: "api work", RootID: "wg1", Section: hp.SectionWorkgroups, Caps: caps, Runtime: "claude", Source: "workspace", State: core.StateWaiting},
		{ID: "wg3", Title: "Docs", IsRoot: true, Role: hp.RoleCoordinator, Section: hp.SectionWorkgroups, Caps: caps, Runtime: "claude",
			Source: "workspace", State: core.StateReady},
		{ID: "c1", Title: "docs work", RootID: "wg3", Section: hp.SectionWorkgroups, Caps: caps, Runtime: "claude", Source: "workspace",
			State: core.StateReady, PRs: &open},
		{ID: OverseerID, Title: "amux overseer", Role: hp.RoleOverseer, Mode: "overseer", Caps: caps, Runtime: "claude", Source: "workspace", State: core.StateReady},
		{ID: ConsoleID, Title: "amux console", Role: hp.RoleConsole, Mode: "console", Caps: caps, Runtime: "claude", Source: "workspace"},
	}
}

func queueEntries() []queue.Entry {
	return []queue.Entry{
		{ID: qPermission, Kind: queue.KindPermission, Session: "a1", SessionTitle: "api work", Workgroup: "wg1",
			Title: "permission: Bash", Body: "rm -rf build/", Tool: "Bash", Action: "rm -rf build/", RequestID: "toolu_01", RuntimeGeneration: "gen-7"},
		{ID: qQuestion, Kind: queue.KindQuestion, Session: "a1", SessionTitle: "api work", Workgroup: "wg1", Title: "Which database?",
			Questions: []queue.Question{{Header: "Database", Question: "Which database?", Options: []queue.Option{{Label: "sqlite", Description: "embedded"}, {Label: "postgres", Preview: "DATABASE_URL=postgres://..."}}}}},
		{ID: qMulti, Kind: queue.KindQuestion, Session: "a1", SessionTitle: "api work", Workgroup: "wg1", Title: "Scope? (+1 more)",
			Questions: []queue.Question{
				{Header: "Scope", Question: "Scope?", Options: []queue.Option{{Label: "api"}, {Label: "web"}}, MultiSelect: true},
				{Header: "Ship", Question: "Ship today?", Options: []queue.Option{{Label: "yes"}, {Label: "no"}}},
			}},
		{ID: qGoal, Kind: queue.KindGoalInput, Session: "wg1", SessionTitle: "Billing", Workgroup: "wg1", Title: "Which region?", Body: "Which region should the rollout start in?", Created: 1795400000000},
		{ID: qDecision, Kind: queue.KindDecision, Session: "wg1", Workgroup: "wg1", Item: itDecision, Title: "Rewrite billing?",
			Body: "The old billing module blocks the migration.", Suggest: "new workgroup for the rewrite", PRs: []string{"acme/api#12"}, Created: 1795300000000},
		{ID: qConsole, Kind: queue.KindQuestion, Session: ConsoleID, Title: "Track a repo?", Questions: []queue.Question{{Question: "Track a repo?"}}},
		{ID: qOther, Kind: queue.KindGoalInput, Session: "wg3", Workgroup: "wg3", Title: "Docs tone?"},
	}
}

func queueItems() []items.Item {
	gi := items.GlobalInbox()
	return []items.Item{
		{ID: itDecision, Kind: items.KindDecisionNeeded, Title: "Rewrite billing?", Detail: "The old billing module blocks the migration.",
			PRs: []string{"acme/api#12"}, Suggest: "new workgroup for the rewrite", State: items.StateOpen, Source: "wg1", Workgroup: "wg1",
			Recipient: gi, Raised: 1, Created: 1795300000000, Updated: 1795300000000,
			History: []items.Event{{At: 1795300000000, By: "wg1", Action: items.EventRaised}}},
		{ID: itRisk, Kind: items.KindRisk, Title: "Flaky CI on macOS", Links: []string{"https://github.com/acme/api/actions/runs/1"},
			State: items.StateAcknowledged, Source: "a1", Workgroup: "wg1", Recipient: gi, Raised: 2, Created: 1795200000000, Updated: 1795250000000},
		{ID: itConsole, Kind: items.KindFYI, Title: "Admin note", State: items.StateOpen, Source: ConsoleID, Recipient: gi, Created: 1, Updated: 1},
		{ID: itOther, Kind: items.KindFollowUp, Title: "Docs follow-up", State: items.StateOpen, Source: "c1", Workgroup: "wg3", Recipient: gi, Created: 2, Updated: 2},
	}
}

func newQueueFake() *queueFake {
	q := &queueFake{fakeBackend: newFake(queueRows()...), byItem: map[string]items.Item{}}
	q.overview = queue.Overview{Entries: queueEntries()}
	q.inbox = queueItems()
	for _, it := range q.inbox {
		q.byItem[it.ID] = it
	}
	done := items.Item{ID: "it-0000000e", Kind: items.KindFollowUp, Title: "Closed", State: items.StateDone, Source: "a1", Workgroup: "wg1", Recipient: items.GlobalInbox()}
	q.byItem[done.ID] = done
	return q
}

// validateCloudCeiling tries to enable cloud issuance with ceiling res.
func validateCloudCeiling(t *testing.T, a *Authority, res ResourceScope) error {
	t.Helper()
	_, err := a.EnableCloud(CloudPolicyRequest{Permissions: []string{PermRead}, Resource: res, OwnerPin: "owner-1", ProviderPin: "prov-1"})
	return err
}

// dialWatchPath dials a watch-style WebSocket route.
func dialWatchPath(t *testing.T, ts *httptest.Server, path string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + path
	conn, _, err := (&websocket.Dialer{}).Dial(u, http.Header{"Host": {testHost}})
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}
