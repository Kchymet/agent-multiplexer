package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/items"
	"amux/internal/itemsvc"
	"amux/internal/prwatch"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

// Through the authenticated session RPC an agent acts only as itself: a
// member raises to its coordinator, cannot act as or see a sibling, cannot
// escalate; its coordinator triages and escalates; errors carry a stable code
// and a message the caller can act on.
func TestItemsOverSessionRPC(t *testing.T) {
	root := store.Session{ID: "wg1", Name: "wg", Agent: "claude", Scope: store.ScopeWork}
	m1 := store.Session{ID: "m1", RootID: root.ID, Name: "m1", Agent: "claude"}
	m2 := store.Session{ID: "m2", RootID: root.ID, Name: "m2", Agent: "claude"}
	_, r, principals := sessionRuntimeFixture(t, root, m1, m2)
	ctx := context.Background()
	call := func(who string, route access.Route, verb, id string, fields map[string]string) sessionrpc.DispatchResult {
		t.Helper()
		c := sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: route, Verb: verb, ID: id, Fields: fields}
		res, err := r.dispatch(ctx, sessionrpc.DispatchRequest{Principal: principals[who], RequestID: "0123456789abcdef0123456789abcdef", Call: c})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := call(m1.ID, access.RouteAction, core.ActionRaiseItem, m1.ID, map[string]string{
		core.ItemKindField: items.KindOutstanding, core.ItemTitleField: "Half the migration", core.ItemLinksField: "https://example.com/a\nhttps://example.com/b",
	})
	if res.Status != sessionrpc.StatusOK {
		t.Fatalf("raise = %+v", res)
	}
	var result core.Result
	var it items.Item
	if err := json.Unmarshal(res.Body, &result); err != nil || json.Unmarshal(result.Item, &it) != nil || it.Recipient != root.ID || len(it.Links) != 2 {
		t.Fatalf("raise result = %s", res.Body)
	}
	// Acting as someone else is refused by the route rule.
	if res := call(m1.ID, access.RouteAction, core.ActionRaiseItem, m2.ID, map[string]string{core.ItemKindField: items.KindRisk, core.ItemTitleField: "x"}); res.Status != sessionrpc.StatusDenied {
		t.Fatalf("raise as sibling = %+v", res)
	}
	if res := call(root.ID, access.RouteQuery, core.QueryItems, m1.ID, nil); res.Status != sessionrpc.StatusDenied {
		t.Fatalf("coordinator listing as member = %+v", res)
	}
	// A sibling sees nothing and cannot learn the item exists.
	res = call(m2.ID, access.RouteQuery, core.QueryItems, m2.ID, nil)
	var list items.List
	if res.Status != sessionrpc.StatusOK || json.Unmarshal(res.Body, &list) != nil || len(list.Items) != 0 {
		t.Fatalf("sibling list = %+v %s", res, res.Body)
	}
	res = call(m2.ID, access.RouteAction, core.ActionItemState, m2.ID, map[string]string{core.ItemIDField: it.ID, core.ItemStateField: items.VerbDone})
	if res.Status == sessionrpc.StatusOK || res.Code != "item_not_found" {
		t.Fatalf("sibling transition = %+v", res)
	}
	// The member cannot escalate; its message names the reason.
	if res := call(m1.ID, access.RouteAction, core.ActionEscalateItem, m1.ID, map[string]string{core.ItemIDField: it.ID}); res.Status != sessionrpc.StatusDenied {
		t.Fatalf("member escalation = %+v", res)
	}
	// Invalid input comes back with the validation message.
	res = call(m1.ID, access.RouteAction, core.ActionRaiseItem, m1.ID, map[string]string{core.ItemKindField: "todo", core.ItemTitleField: "x"})
	if res.Status != sessionrpc.StatusInvalid || json.Unmarshal(res.Body, &result) != nil || !strings.Contains(result.Error, "unknown kind") {
		t.Fatalf("invalid raise = %+v %s", res, res.Body)
	}
	// The coordinator sees it in its inbox and escalates it.
	res = call(root.ID, access.RouteQuery, core.QueryItems, root.ID, map[string]string{core.ItemBoxField: core.ItemBoxInbox})
	if res.Status != sessionrpc.StatusOK || json.Unmarshal(res.Body, &list) != nil || len(list.Items) != 1 || list.Items[0].ID != it.ID {
		t.Fatalf("coordinator inbox = %+v %s", res, res.Body)
	}
	res = call(root.ID, access.RouteAction, core.ActionEscalateItem, root.ID, map[string]string{core.ItemIDField: it.ID, core.ItemNoteField: "needs the user"})
	if res.Status != sessionrpc.StatusOK || json.Unmarshal(res.Body, &result) != nil || json.Unmarshal(result.Item, &it) != nil || it.Recipient != items.GlobalInbox() {
		t.Fatalf("escalation = %+v %s", res, res.Body)
	}
}

func itemsWake(recipient string) prwatch.Wake {
	return prwatch.Wake{Kind: prwatch.KindItems, Workgroup: recipient, SessionID: recipient}
}

// A member's raised item reaches its coordinator as one coalesced wake-up
// listing it, and is then marked notified so it is not announced twice.
func TestItemWakeReachesCoordinator(t *testing.T) {
	d, in, root, member, _ := prWakeFixture(t)
	ctx := context.Background()
	it, _, err := d.items.Raise(ctx, member.ID, items.Raise{Kind: items.KindFollowUp, Title: "Document the flag", Suggest: "add a docs PR"})
	if err != nil {
		t.Fatal(err)
	}
	if it.Recipient != root.ID {
		t.Fatalf("recipient = %q, want the coordinator", it.Recipient)
	}
	got := d.deliverPRWake(ctx, root.ID, []prwatch.Wake{itemsWake(root.ID)})
	if len(got) != 1 || got[0] != prwatch.Delivered {
		t.Fatalf("items wake = %v, want Delivered", got)
	}
	text := in.written()
	for _, want := range []string{"Items raised", it.ID, "Document the flag", "from member", "amux agent items"} {
		if !strings.Contains(text, want) {
			t.Fatalf("wake %q lacks %q", text, want)
		}
	}
	if pending, _ := d.items.Unnotified(root.ID, 0); len(pending) != 0 {
		t.Fatalf("still unnotified after delivery: %+v", pending)
	}
	// Nothing new: a repeated wake is dropped, not typed again.
	if got := d.deliverPRWake(ctx, root.ID, []prwatch.Wake{itemsWake(root.ID)}); len(got) != 1 || got[0] != prwatch.Dropped {
		t.Fatalf("empty items wake = %v, want Dropped", got)
	}
	// A forged wake naming another recipient is dropped.
	if got := d.deliverPRWake(ctx, root.ID, []prwatch.Wake{{Kind: prwatch.KindItems, Workgroup: root.ID, SessionID: member.ID}}); got[0] != prwatch.Dropped {
		t.Fatalf("mis-addressed items wake = %v, want Dropped", got)
	}
}

// Archiving a coordinator carries its active items to the global inbox (the
// overseer), and the overseer is offered them; a wake-up that cannot be
// delivered is deferred and the items stay unnotified for the next offer.
func TestArchivedCoordinatorItemsReachGlobalInbox(t *testing.T) {
	d, _, root, member, _ := prWakeFixture(t)
	ctx := context.Background()
	it, _, err := d.items.Raise(ctx, member.ID, items.Raise{Kind: items.KindDecisionNeeded, Title: "Pick a name"})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetArchivedFlag(root.ID, true, 1); err != nil {
		t.Fatal(err)
	}
	db.Close()
	inbox, err := d.items.List(ctx, items.GlobalInbox(), itemsQueryInbox())
	if err != nil || len(inbox.Items) != 1 || inbox.Items[0].ID != it.ID {
		t.Fatalf("global inbox = %+v, %v", inbox, err)
	}
	if recipients, _ := d.items.UnnotifiedRecipients(); len(recipients) != 1 || recipients[0] != items.GlobalInbox() {
		t.Fatalf("unnotified recipients = %v, want the global inbox owner", recipients)
	}
	// Without a runtime engine the overseer cannot be started: deferred, never lost.
	eng := d.engine
	d.engine = nil
	if got := d.deliverPRWake(ctx, items.GlobalInbox(), []prwatch.Wake{itemsWake(items.GlobalInbox())}); len(got) != 1 || got[0] != prwatch.Deferred {
		t.Fatalf("global inbox wake = %v, want Deferred", got)
	}
	if pending, _ := d.items.Unnotified(items.GlobalInbox(), 0); len(pending) != 1 {
		t.Fatalf("deferred wake marked items notified: %+v", pending)
	}
	// The overseer starts lazily: a wake-up for a stopped overseer is the
	// prompt that starts it, and the items are then notified.
	d.engine = eng
	if got := d.deliverPRWake(ctx, items.GlobalInbox(), []prwatch.Wake{itemsWake(items.GlobalInbox())}); len(got) != 1 || got[0] != prwatch.Delivered {
		t.Fatalf("global inbox wake to a stopped overseer = %v, want Delivered", got)
	}
	if pending, _ := d.items.Unnotified(items.GlobalInbox(), 0); len(pending) != 0 {
		t.Fatalf("delivered wake left items unnotified: %+v", pending)
	}
	// Nothing new to announce: an items wake is dropped.
	if _, err := d.items.Transition(ctx, items.GlobalInbox(), it.ID, items.VerbAck, ""); err != nil {
		t.Fatal(err)
	}
	if got := d.deliverPRWake(ctx, items.GlobalInbox(), []prwatch.Wake{itemsWake(items.GlobalInbox())}); got[0] != prwatch.Dropped {
		t.Fatalf("global inbox wake after triage = %v, want Dropped", got)
	}
	// A coordinator update is delivered on its own, without items.
	update := prwatch.Wake{Kind: prwatch.KindOverseer, Workgroup: items.GlobalInbox(), SessionID: root.ID, Text: "coordinator wg asked the user: which?"}
	if got := d.deliverPRWake(ctx, items.GlobalInbox(), []prwatch.Wake{update}); got[0] != prwatch.Delivered {
		t.Fatalf("coordinator update = %v, want Delivered", got)
	}
}

func itemsQueryInbox() itemsvc.Query { return itemsvc.Query{Box: itemsvc.BoxInbox} }

func TestItemOperationValidation(t *testing.T) {
	ok := []access.Request{
		{Route: access.RouteAction, Verb: core.ActionRaiseItem, ID: "a1", Fields: map[string]string{core.ItemKindField: "risk", core.ItemTitleField: "x", core.ItemPRsField: "r#1"}},
		{Route: access.RouteAction, Verb: core.ActionItemState, ID: "a1", Fields: map[string]string{core.ItemIDField: "it-00000000", core.ItemStateField: "done"}},
		{Route: access.RouteAction, Verb: core.ActionEscalateItem, ID: "wg1", Fields: map[string]string{core.ItemIDField: "it-00000000", core.ItemNoteField: "why"}},
		{Route: access.RouteQuery, Verb: core.QueryItems, ID: "a1"},
		{Route: access.RouteQuery, Verb: core.QueryItems, ID: "a1", Fields: map[string]string{core.ItemStatesField: "open,done", core.ItemBoxField: "inbox", core.ItemLimitField: "5"}},
	}
	for _, req := range ok {
		if _, err := canonicalSessionOperation(req); err != nil {
			t.Errorf("%s %s rejected: %v", req.Route, req.Verb, err)
		}
	}
	bad := map[string]access.Request{
		"raise without id":     {Route: access.RouteAction, Verb: core.ActionRaiseItem, Fields: map[string]string{core.ItemKindField: "risk", core.ItemTitleField: "x"}},
		"raise without title":  {Route: access.RouteAction, Verb: core.ActionRaiseItem, ID: "a1", Fields: map[string]string{core.ItemKindField: "risk"}},
		"raise forged field":   {Route: access.RouteAction, Verb: core.ActionRaiseItem, ID: "a1", Fields: map[string]string{core.ItemTitleField: "x", "recipient": "console"}},
		"raise with target":    {Route: access.RouteAction, Verb: core.ActionRaiseItem, ID: "a1", Target: "wg1", Fields: map[string]string{core.ItemTitleField: "x"}},
		"state without item":   {Route: access.RouteAction, Verb: core.ActionItemState, ID: "a1", Fields: map[string]string{core.ItemStateField: "done"}},
		"escalate forged":      {Route: access.RouteAction, Verb: core.ActionEscalateItem, ID: "wg1", Fields: map[string]string{core.ItemIDField: "it-00000000", "to": "wg2"}},
		"query without id":     {Route: access.RouteQuery, Verb: core.QueryItems},
		"query bad state":      {Route: access.RouteQuery, Verb: core.QueryItems, ID: "a1", Fields: map[string]string{core.ItemStatesField: "bogus"}},
		"query bad box":        {Route: access.RouteQuery, Verb: core.QueryItems, ID: "a1", Fields: map[string]string{core.ItemBoxField: "everyone"}},
		"query limit too high": {Route: access.RouteQuery, Verb: core.QueryItems, ID: "a1", Fields: map[string]string{core.ItemLimitField: "100000"}},
	}
	for name, req := range bad {
		if _, err := canonicalSessionOperation(req); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// The host path acts as the named session and answers with the item.
func TestHostItemActionsActAsTheNamedSession(t *testing.T) {
	d, _, root, member, _ := prWakeFixture(t)
	ctx := context.Background()
	res := d.handle(ctx, core.Action{Action: core.ActionRaiseItem, ID: member.ID, Fields: map[string]string{
		core.ItemKindField: items.KindRisk, core.ItemTitleField: "Flaky test", core.ItemPRsField: "o/r#3",
	}})
	if !res.OK || res.NewID == "" {
		t.Fatalf("host raise = %+v", res)
	}
	var it items.Item
	if err := json.Unmarshal(res.Item, &it); err != nil || it.Recipient != root.ID || len(it.PRs) != 1 {
		t.Fatalf("raised item = %+v, %v", it, err)
	}
	// A member cannot escalate even through the host path.
	if res := d.handle(ctx, core.Action{Action: core.ActionEscalateItem, ID: member.ID, Fields: map[string]string{core.ItemIDField: it.ID}}); res.OK || res.Code != "item_denied" {
		t.Fatalf("member escalation via host = %+v", res)
	}
	if res := d.handle(ctx, core.Action{Action: core.ActionEscalateItem, ID: root.ID, Fields: map[string]string{core.ItemIDField: it.ID, core.ItemNoteField: "user call"}}); !res.OK {
		t.Fatalf("coordinator escalation via host = %+v", res)
	}
	rows, err := d.readModel(core.Action{Action: core.ActionQuery, Query: core.QueryItems})
	if err != nil {
		t.Fatal(err)
	}
	var list items.List
	if err := json.Unmarshal(rows.(json.RawMessage), &list); err != nil || len(list.Items) != 1 || list.Items[0].Recipient != items.GlobalInbox() {
		t.Fatalf("host (global inbox) view = %+v, %v", list, err)
	}
}

func TestBoundedItemListTruncates(t *testing.T) {
	big := strings.Repeat("x", items.MaxDetailRunes)
	var list items.List
	for i := 0; i < 40; i++ {
		list.Items = append(list.Items, items.Item{ID: items.NewID(), Detail: big})
	}
	body, err := boundedItemList(list)
	if err != nil {
		t.Fatal(err)
	}
	var got items.List
	if err := json.Unmarshal(body, &got); err != nil || !got.Truncated || len(got.Items) == 0 || len(got.Items) >= 40 {
		t.Fatalf("bounded list = %d items truncated=%v err=%v", len(got.Items), got.Truncated, err)
	}
}
