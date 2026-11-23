package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"unicode/utf8"

	"amux/internal/access"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/items"
	"amux/internal/itemsvc"
	"amux/internal/prwatch"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

// This file serves upward signals (internal/items) over the daemon's two
// control paths and delivers them:
//
//   - a session raises, lists and moves items through its authenticated
//     session RPC (`amux agent raise|items|escalate`). The route rules admit
//     an item verb only with the caller as its target (access.allowSelfItem);
//     the item service then decides per item from stored state;
//   - the host may act as any session through the control socket (`amux do
//     raise-item <id> …`, and later the TUI's console inbox);
//   - an item its recipient has not been told about is offered as a KindItems
//     wake on the PR watcher's queue — the same coalescing, revalidation and
//     delivery gating as PR and member wake-ups — to its coordinator, or to the
//     console. offerItemWakes runs after every change that leaves an item
//     unnotified, when a coordinator leaves the rail (its items were carried
//     over to the console in the archive transaction), and once at startup.

func newItemService() *itemsvc.Service {
	return &itemsvc.Service{Open: itemsvc.OpenStore, OpenRead: itemsvc.OpenReadStore, Resolver: newDaemonAccessResolver(), Now: store.Now}
}

// isItemVerb reports whether an action or query verb is an item verb.
func isItemVerb(verb string) bool {
	switch verb {
	case core.ActionRaiseItem, core.ActionItemState, core.ActionEscalateItem:
		return true
	}
	return false
}

func isItemOperation(req access.Request) bool {
	return (req.Route == access.RouteAction && isItemVerb(req.Verb)) ||
		(req.Route == access.RouteQuery && req.Verb == core.QueryItems)
}

// validateItemAction checks an item action's shape; content (lengths,
// references, states) is validated by the item service.
func validateItemAction(req access.Request) error {
	if strings.TrimSpace(req.ID) == "" {
		return fmt.Errorf("action %q requires the acting session id", req.Verb)
	}
	if req.Target != "" {
		return fmt.Errorf("action %q does not accept target", req.Verb)
	}
	switch req.Verb {
	case core.ActionRaiseItem:
		if err := validateFields(req.Fields, core.ItemKindField, core.ItemTitleField, core.ItemDetailField,
			core.ItemPRsField, core.ItemLinksField, core.ItemSuggestField); err != nil {
			return err
		}
		return requireField(req.Fields, core.ItemTitleField)
	case core.ActionItemState:
		if err := validateFields(req.Fields, core.ItemIDField, core.ItemStateField, core.ItemNoteField); err != nil {
			return err
		}
		return joinValidation(requireField(req.Fields, core.ItemIDField), requireField(req.Fields, core.ItemStateField))
	case core.ActionEscalateItem:
		if err := validateFields(req.Fields, core.ItemIDField, core.ItemNoteField); err != nil {
			return err
		}
		return requireField(req.Fields, core.ItemIDField)
	}
	return fmt.Errorf("unknown item action %q", req.Verb)
}

// validateItemQuery checks a QueryItems request's shape.
func validateItemQuery(req access.Request) error {
	if strings.TrimSpace(req.ID) == "" {
		return fmt.Errorf("query %q requires the acting session id", req.Verb)
	}
	if err := validateFields(req.Fields, core.ItemIDField, core.ItemStatesField, core.ItemBoxField, core.ItemLimitField); err != nil {
		return err
	}
	_, err := itemQueryOf(req.Fields)
	return err
}

func itemQueryOf(fields map[string]string) (itemsvc.Query, error) {
	var q itemsvc.Query
	switch states := strings.TrimSpace(fields[core.ItemStatesField]); states {
	case "":
	case "all":
		q.AllStates = true
	default:
		for _, s := range strings.Split(states, ",") {
			s = strings.TrimSpace(s)
			if !items.KnownState(s) {
				return q, fmt.Errorf("unknown item state %q", s)
			}
			q.States = append(q.States, s)
		}
	}
	switch box := fields[core.ItemBoxField]; box {
	case "", core.ItemBoxRaised, core.ItemBoxInbox:
		q.Box = box
	default:
		return q, fmt.Errorf("unknown item box %q", box)
	}
	if v, ok := fields[core.ItemLimitField]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > itemsvc.MaxLimit || strconv.Itoa(n) != v {
			return q, fmt.Errorf("item limit must be 1..%d", itemsvc.MaxLimit)
		}
		q.Limit = n
	}
	return q, nil
}

func splitItemList(value, sep string) []string {
	var out []string
	for _, v := range strings.Split(value, sep) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// itemOutcome is one item operation's answer: the encoded body or a failure
// with a stable code and a message safe for the caller.
type itemOutcome struct {
	body    []byte
	code    string
	message string
	denied  bool
	invalid bool
}

func itemFailure(err error) itemOutcome {
	var inv *itemsvc.InvalidError
	switch {
	case errors.As(err, &inv):
		return itemOutcome{code: "item_invalid", message: inv.Msg, invalid: true}
	case errors.Is(err, itemsvc.ErrNotFound):
		return itemOutcome{code: "item_not_found", message: itemsvc.ErrNotFound.Error()}
	case errors.Is(err, itemsvc.ErrDenied):
		return itemOutcome{code: "item_denied", message: itemsvc.ErrDenied.Error(), denied: true}
	case errors.Is(err, itemsvc.ErrActor):
		return itemOutcome{code: "access_denied", message: itemsvc.ErrActor.Error(), denied: true}
	}
	log.Printf("items: %v", err)
	return itemOutcome{code: "item_failed", message: "item operation failed"}
}

// runItemOperation executes one canonical item action or query as actor. It
// is shared by the restricted session path (actor = the authenticated caller)
// and the host path (actor = Action.ID).
func (d *Daemon) runItemOperation(ctx context.Context, a core.Action) itemOutcome {
	svc := d.items
	if svc == nil {
		return itemOutcome{code: "item_failed", message: "item service unavailable"}
	}
	if a.Action == core.ActionQuery && a.Query == core.QueryItems {
		var value any
		if id := a.Fields[core.ItemIDField]; id != "" {
			it, err := svc.Get(ctx, a.ID, id)
			if err != nil {
				return itemFailure(err)
			}
			value = items.List{Items: []items.Item{it}}
		} else {
			q, err := itemQueryOf(a.Fields)
			if err != nil {
				return itemOutcome{code: "item_invalid", message: err.Error(), invalid: true}
			}
			list, err := svc.List(ctx, a.ID, q)
			if err != nil {
				return itemFailure(err)
			}
			value = list
		}
		body, err := boundedItemList(value)
		if err != nil {
			return itemFailure(err)
		}
		return itemOutcome{body: body}
	}
	var (
		it  items.Item
		err error
	)
	switch a.Action {
	case core.ActionRaiseItem:
		it, _, err = svc.Raise(ctx, a.ID, items.Raise{
			Kind:    a.Fields[core.ItemKindField],
			Title:   a.Fields[core.ItemTitleField],
			Detail:  a.Fields[core.ItemDetailField],
			PRs:     splitItemList(a.Fields[core.ItemPRsField], ","),
			Links:   splitItemList(a.Fields[core.ItemLinksField], "\n"),
			Suggest: a.Fields[core.ItemSuggestField],
		})
	case core.ActionItemState:
		it, err = svc.Transition(ctx, a.ID, a.Fields[core.ItemIDField], a.Fields[core.ItemStateField], a.Fields[core.ItemNoteField])
	case core.ActionEscalateItem:
		it, err = svc.Escalate(ctx, a.ID, a.Fields[core.ItemIDField], a.Fields[core.ItemNoteField])
	default:
		return itemOutcome{code: "item_invalid", message: "unknown item operation", invalid: true}
	}
	if err != nil {
		return itemFailure(err)
	}
	if it.Unnotified {
		d.offerItemWakes()
	}
	raw, err := json.Marshal(it)
	if err != nil {
		return itemFailure(err)
	}
	body, err := json.Marshal(core.Result{Type: "result", OK: true, NewID: it.ID, Item: raw})
	if err != nil {
		return itemFailure(err)
	}
	return itemOutcome{body: body}
}

// boundedItemList encodes a list within the session RPC response bound: a
// page that does not fit drops its oldest items (and says it is truncated).
func boundedItemList(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil || len(body) <= sessionrpc.MaxResponseBody {
		return body, err
	}
	list, ok := value.(items.List)
	if !ok {
		return nil, fmt.Errorf("item response exceeds transport bound")
	}
	for len(list.Items) > 0 {
		list.Items = list.Items[:len(list.Items)*3/4]
		list.Truncated = true
		if body, err = json.Marshal(list); err != nil || len(body) <= sessionrpc.MaxResponseBody {
			return body, err
		}
	}
	return nil, fmt.Errorf("item response exceeds transport bound")
}

// dispatchItems serves an item verb for a restricted caller. It holds the
// caller's session lock (and, for a raise, its workgroup root's) so an archive
// of either cannot cross between routing and the write — an item never lands
// with a recipient whose archive already carried its inbox over.
func (r *sessionRuntime) dispatchItems(ctx context.Context, principal access.Principal, req access.Request, action core.Action) (sessionrpc.DispatchResult, error) {
	locks := []string{principal.SubjectID}
	if req.Verb == core.ActionRaiseItem && r.resolver != nil {
		if subject, ok, err := r.resolver.Lookup(ctx, principal.SubjectID); err == nil && ok && subject.RootID != "" {
			locks = append(locks, subject.RootID)
		}
	}
	release := r.d.admitSessions(locks...)
	defer release()
	if denied, ok := r.admissionCheck(ctx, principal, req); !ok {
		return denied, nil
	}
	if req.ID != principal.SubjectID {
		// Policy already requires self; never act as anyone else.
		return rpcDenied("access_denied"), nil
	}
	out := r.d.runItemOperation(ctx, action)
	if out.body != nil {
		return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: out.body}, nil
	}
	body, _ := json.Marshal(core.Result{Type: "result", Code: out.code, Error: out.message})
	status := sessionrpc.StatusFailed
	switch {
	case out.denied:
		status = sessionrpc.StatusDenied
	case out.invalid:
		status = sessionrpc.StatusInvalid
	}
	return sessionrpc.DispatchResult{Status: status, Code: out.code, Body: body}, nil
}

// handleItemAction serves an item verb from the host control path, acting as
// a.ID.
func (d *Daemon) handleItemAction(ctx context.Context, a core.Action) core.Result {
	req := access.Request{Route: access.RouteAction, Verb: a.Action, ID: a.ID, Target: a.Target, Fields: a.Fields}
	if err := validateItemAction(req); err != nil {
		return fail("%v", err)
	}
	release := d.admitSessions(a.ID)
	defer release()
	out := d.runItemOperation(ctx, a)
	if out.body == nil {
		return core.Result{Type: "result", Code: out.code, Error: out.message}
	}
	var res core.Result
	if err := json.Unmarshal(out.body, &res); err != nil {
		return fail("decode item result: %v", err)
	}
	return res
}

// hostItemQuery serves QueryItems on the host read path. Without an id the
// host reads as the console (every item).
func (d *Daemon) hostItemQuery(ctx context.Context, a core.Action) (any, error) {
	if a.ID == "" {
		a.ID = console.ID
	}
	req := access.Request{Route: access.RouteQuery, Verb: core.QueryItems, ID: a.ID, Fields: a.Fields}
	if err := validateItemQuery(req); err != nil {
		return nil, err
	}
	a.Action = core.ActionQuery
	out := d.runItemOperation(ctx, a)
	if out.body == nil {
		return nil, fmt.Errorf("%s", out.message)
	}
	return json.RawMessage(out.body), nil
}

// offerItemWakes queues a KindItems wake for every recipient with active
// items it has not been told about. It never blocks on delivery: the PR
// watcher coalesces and delivers (deliverPRWake), and an undelivered notice
// stays recorded on the items, so the next offer (a later change, a
// coordinator leaving, a daemon restart) tries again.
func (d *Daemon) offerItemWakes() {
	if d.prs == nil || d.items == nil {
		return
	}
	recipients, err := d.items.UnnotifiedRecipients()
	if err != nil {
		log.Printf("items: list unnotified recipients: %v", err)
		return
	}
	for _, r := range recipients {
		d.prs.Notify(prwatch.Wake{Kind: prwatch.KindItems, Workgroup: r, SessionID: r})
	}
}

// maxItemWakeLines bounds the items listed in one wake-up.
const maxItemWakeLines = 6

// maxItemWakeTitleRunes bounds a title quoted in a wake line.
const maxItemWakeTitleRunes = 100

// itemWakeLine renders one item for a wake-up.
func itemWakeLine(it items.Item) string {
	title := it.Title
	if utf8.RuneCountInString(title) > maxItemWakeTitleRunes {
		title = string([]rune(title)[:maxItemWakeTitleRunes]) + "…"
	}
	via := "from " + it.Source
	if n := len(it.History); n > 0 {
		switch last := it.History[n-1]; last.Action {
		case items.EventEscalated:
			via += ", escalated by " + last.By
		case items.EventCarriedOver:
			via += ", carried over from " + last.From
		case items.EventUpdated:
			via += ", updated"
		case items.EventReopened:
			via += ", reopened"
		}
	}
	return fmt.Sprintf("item %s [%s] %q (%s)", it.ID, it.Kind, title, via)
}

// itemWakeLines renders a recipient's unnotified items as wake lines.
func itemWakeLines(pending []items.Item, total int) []string {
	lines := make([]string, 0, len(pending)+1)
	for _, it := range pending {
		lines = append(lines, itemWakeLine(it))
	}
	if total > len(pending) {
		lines = append(lines, fmt.Sprintf("(+%d more items)", total-len(pending)))
	}
	return lines
}

// loadItemWake reads what a KindItems wake for recipient should say: up to
// maxItemWakeLines unnotified items (shown) and the full unnotified set
// (marked notified on delivery).
func (d *Daemon) loadItemWake(recipient string) (shown, all []items.Item, err error) {
	if d.items == nil {
		return nil, nil, nil
	}
	all, err = d.items.Unnotified(recipient, 0)
	if err != nil {
		return nil, nil, err
	}
	shown = all
	if len(shown) > maxItemWakeLines {
		shown = shown[:maxItemWakeLines]
	}
	return shown, all, nil
}

// itemsTriageHint closes a wake-up that lists items.
const itemsTriageHint = "Triage them with `amux agent items` (ack, dismiss, done; a coordinator may `amux agent escalate <id>` to the console)."

// deliverConsoleItemWakes offers the console its unnotified items. Like a
// coordinator wake-up it is typed only into a live console between turns with
// no prompt open; otherwise it is deferred. The items stay in the console's
// inbox either way.
func (d *Daemon) deliverConsoleItemWakes(ctx context.Context, wakes []prwatch.Wake) []prwatch.Delivery {
	results := make([]prwatch.Delivery, len(wakes))
	for i, w := range wakes {
		results[i] = prwatch.Deferred
		if w.Kind != prwatch.KindItems {
			results[i] = prwatch.Dropped
		}
	}
	release := d.admitSessions(console.ID)
	defer release()
	shown, all, err := d.loadItemWake(console.ID)
	if err != nil {
		log.Printf("items: console wake-up: %v", err)
		return results
	}
	if len(all) == 0 {
		for i := range results {
			results[i] = prwatch.Dropped
		}
		return results
	}
	sess := console.Session()
	if ready, why := d.coordinatorTakesWake(sess); !ready {
		if why != "" {
			log.Printf("items: console wake-up deferred: %s", why)
		}
		return results
	}
	text := prWakeText(false, itemWakeLines(shown, len(all)), wakeKinds{items: true})
	outcome, err := d.beginPRWake(ctx, sess, text)
	if err != nil || outcome != prwatch.Delivered {
		if outcome == prwatch.Indeterminate {
			for i := range results {
				results[i] = prwatch.Indeterminate
			}
		}
		log.Printf("items: console wake-up not delivered: %v", err)
		return results
	}
	if err := d.items.MarkNotified(console.ID, all); err != nil {
		log.Printf("items: mark console items notified: %v", err)
	}
	for i := range results {
		if wakes[i].Kind == prwatch.KindItems {
			results[i] = prwatch.Delivered
		}
	}
	log.Printf("items: woke console (%d item(s))", len(all))
	return results
}
