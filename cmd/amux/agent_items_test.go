package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/items"
)

type itemCallRecord struct {
	route  access.Route
	verb   string
	id     string
	fields map[string]string
}

// fakeItems replaces the daemon: it records each call and answers with reply.
func fakeItems(t *testing.T, self string, reply func(itemCallRecord) ([]byte, error)) *[]itemCallRecord {
	t.Helper()
	var calls []itemCallRecord
	oldCall, oldLoad := itemCall, loadAgentSessionContext
	itemCall = func(route access.Route, verb, id string, fields map[string]string) ([]byte, error) {
		rec := itemCallRecord{route, verb, id, fields}
		calls = append(calls, rec)
		return reply(rec)
	}
	loadAgentSessionContext = func() (access.SessionContext, error) { return access.SessionContext{SubjectID: self}, nil }
	t.Cleanup(func() { itemCall, loadAgentSessionContext = oldCall, oldLoad })
	return &calls
}

func itemResult(t *testing.T, it items.Item) []byte {
	t.Helper()
	raw, _ := json.Marshal(it)
	body, err := json.Marshal(core.Result{Type: "result", OK: true, NewID: it.ID, Item: raw})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestAgentRaiseSendsFieldsAsSelf(t *testing.T) {
	calls := fakeItems(t, "m1", func(rec itemCallRecord) ([]byte, error) {
		return itemResult(t, items.Item{ID: "it-0000000a", Kind: rec.fields[core.ItemKindField], Recipient: "wg1", Raised: 1}), nil
	})
	out := captureStdout(t, func() {
		err := cmdAgentRaise([]string{"--kind", "follow_up", "--title=Add docs", "--detail", "why",
			"--pr", "o/r#1,o/r#2", "--pr=o/r#3", "--link", "https://a.example/x,y", "--link", "https://b.example", "--suggest", "open a PR"})
		if err != nil {
			t.Fatal(err)
		}
	})
	if len(*calls) != 1 {
		t.Fatalf("calls = %+v", *calls)
	}
	c := (*calls)[0]
	if c.route != access.RouteAction || c.verb != core.ActionRaiseItem || c.id != "m1" {
		t.Fatalf("call = %+v", c)
	}
	want := map[string]string{
		core.ItemKindField: "follow_up", core.ItemTitleField: "Add docs", core.ItemDetailField: "why",
		core.ItemPRsField: "o/r#1,o/r#2,o/r#3", core.ItemLinksField: "https://a.example/x,y\nhttps://b.example", core.ItemSuggestField: "open a PR",
	}
	for k, v := range want {
		if c.fields[k] != v {
			t.Errorf("field %s = %q, want %q", k, c.fields[k], v)
		}
	}
	if !strings.Contains(out, "raised it-0000000a [follow_up] → coordinator wg1") {
		t.Fatalf("output = %q", out)
	}
}

func TestAgentRaiseValidatesLocally(t *testing.T) {
	calls := fakeItems(t, "m1", func(itemCallRecord) ([]byte, error) { return nil, errors.New("unreachable") })
	for _, args := range [][]string{
		{"--title", "x"},
		{"--kind", "follow_up"},
		{"--kind", "todo", "--title", "x"},
		{"--kind", "risk", "--title", "x", "--bogus"},
		{"--kind", "risk", "--title"},
	} {
		if err := cmdAgentRaise(args); err == nil {
			t.Errorf("raise %q accepted", args)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("invalid raises reached the daemon: %+v", *calls)
	}
}

func TestAgentRaiseDetailFromStdin(t *testing.T) {
	calls := fakeItems(t, "m1", func(itemCallRecord) ([]byte, error) {
		return itemResult(t, items.Item{ID: "it-0000000b", Recipient: items.GlobalInbox(), Raised: 2}), nil
	})
	old := itemStdin
	itemStdin = strings.NewReader("multi\nline detail\n")
	t.Cleanup(func() { itemStdin = old })
	out := captureStdout(t, func() {
		if err := cmdAgentRaise([]string{"--kind", "risk", "--title", "x", "--detail", "-"}); err != nil {
			t.Fatal(err)
		}
	})
	if (*calls)[0].fields[core.ItemDetailField] != "multi\nline detail\n" {
		t.Fatalf("detail = %q", (*calls)[0].fields[core.ItemDetailField])
	}
	if !strings.Contains(out, "updated (raised 2 times) it-0000000b") || !strings.Contains(out, "the global inbox") {
		t.Fatalf("output = %q", out)
	}
}

func TestAgentItemsListAndTransitions(t *testing.T) {
	calls := fakeItems(t, "wg1", func(rec itemCallRecord) ([]byte, error) {
		switch rec.verb {
		case core.QueryItems:
			return json.Marshal(items.List{Items: []items.Item{{ID: "it-0000000c", State: "open", Kind: "risk", Source: "m1", Recipient: "wg1", Title: "Flaky"}}, Truncated: true})
		case core.ActionItemState:
			if rec.fields[core.ItemIDField] == "it-bad00000" {
				return nil, errors.New("no such item")
			}
			return itemResult(t, items.Item{ID: rec.fields[core.ItemIDField], State: items.StateAcknowledged}), nil
		case core.ActionEscalateItem:
			return itemResult(t, items.Item{ID: rec.fields[core.ItemIDField], Recipient: items.GlobalInbox()}), nil
		}
		return nil, errors.New("unexpected")
	})
	out := captureStdout(t, func() {
		if err := cmdAgentItems([]string{"--inbox", "--state", "open,acknowledged", "--limit", "5"}); err != nil {
			t.Fatal(err)
		}
	})
	c := (*calls)[0]
	if c.route != access.RouteQuery || c.id != "wg1" || c.fields[core.ItemBoxField] != core.ItemBoxInbox ||
		c.fields[core.ItemStatesField] != "open,acknowledged" || c.fields[core.ItemLimitField] != "5" {
		t.Fatalf("list call = %+v", c)
	}
	if !strings.Contains(out, "it-0000000c") || !strings.Contains(out, "m1 → you") || !strings.Contains(out, "more items") {
		t.Fatalf("list output = %q", out)
	}

	*calls = nil
	var err error
	captureStdout(t, func() { err = cmdAgentItems([]string{"ack", "it-0000000c", "it-bad00000", "--note", "on it"}) })
	if err == nil || !strings.Contains(err.Error(), "it-bad00000") {
		t.Fatalf("partial failure = %v", err)
	}
	if len(*calls) != 2 || (*calls)[0].fields[core.ItemStateField] != items.VerbAck || (*calls)[0].fields[core.ItemNoteField] != "on it" {
		t.Fatalf("ack calls = %+v", *calls)
	}

	*calls = nil
	out = captureStdout(t, func() {
		if err := cmdAgentEscalate([]string{"it-0000000c", "--note=user decides"}); err != nil {
			t.Fatal(err)
		}
	})
	if (*calls)[0].verb != core.ActionEscalateItem || (*calls)[0].fields[core.ItemNoteField] != "user decides" || !strings.Contains(out, "→ the global inbox") {
		t.Fatalf("escalate = %+v %q", *calls, out)
	}
	if err := cmdAgentEscalate(nil); err == nil {
		t.Fatal("escalate with no id accepted")
	}
}

func TestAgentItemsOutsideAnAgent(t *testing.T) {
	old := loadAgentSessionContext
	loadAgentSessionContext = func() (access.SessionContext, error) { return access.SessionContext{}, errors.New("none") }
	t.Cleanup(func() { loadAgentSessionContext = old })
	if err := cmdAgentItems(nil); err == nil || !strings.Contains(err.Error(), "not inside an amux agent") {
		t.Fatalf("outside agent = %v", err)
	}
}
