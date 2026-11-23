package access

import (
	"context"
	"testing"

	"amux/internal/core"
	"amux/internal/items"
)

// Item verbs are admitted only with the caller as the target; escalation only
// for a coordinator. The console holds no delegated grant here: it too acts
// only as itself (where it sees every item).
func TestItemVerbsAreSelfOnly(t *testing.T) {
	p := Policy{Resolver: fakeResolver{resources: map[string]Resource{
		"console": {ID: "console", Role: "console"},
		"wg1":     {ID: "wg1", Role: "coordinator", Scope: "work"},
		"a1":      {ID: "a1", RootID: "wg1"},
		"a2":      {ID: "a2", RootID: "wg1"},
		"old":     {ID: "old", RootID: "wg1", Archived: true},
		"api":     {ID: "api", Role: "repo", Scope: "repo", Repo: "api"},
	}}}
	as := func(id string) Principal { return Principal{Kind: SubjectSession, SubjectID: id} }
	act := func(verb, id string) Request { return Request{Route: RouteAction, Verb: verb, ID: id} }
	query := func(id string) Request { return Request{Route: RouteQuery, Verb: core.QueryItems, ID: id} }
	tests := []struct {
		name string
		who  Principal
		req  Request
		want bool
	}{
		{"member raises as itself", as("a1"), act(core.ActionRaiseItem, "a1"), true},
		{"member cannot raise as a sibling", as("a1"), act(core.ActionRaiseItem, "a2"), false},
		{"coordinator cannot raise as its member", as("wg1"), act(core.ActionRaiseItem, "a1"), false},
		{"console cannot raise as a member", as("console"), act(core.ActionRaiseItem, "a1"), false},
		{"console raises as itself", as("console"), act(core.ActionRaiseItem, "console"), true},
		{"repo home raises as itself", as("api"), act(core.ActionRaiseItem, "api"), true},
		{"archived session cannot raise", as("old"), act(core.ActionRaiseItem, "old"), false},
		{"member moves items as itself", as("a1"), act(core.ActionItemState, "a1"), true},
		{"coordinator cannot move as its member", as("wg1"), act(core.ActionItemState, "a1"), false},
		{"coordinator escalates as itself", as("wg1"), act(core.ActionEscalateItem, "wg1"), true},
		{"member cannot escalate", as("a1"), act(core.ActionEscalateItem, "a1"), false},
		{"console cannot escalate", as("console"), act(core.ActionEscalateItem, "console"), false},
		{"repo home cannot escalate", as("api"), act(core.ActionEscalateItem, "api"), false},
		{"member lists its own items", as("a1"), query("a1"), true},
		{"member cannot list a sibling's", as("a1"), query("a2"), false},
		{"coordinator cannot list as its member", as("wg1"), query("a1"), false},
		{"console lists as itself", as("console"), query("console"), true},
		{"console cannot list as a member", as("console"), query("a1"), false},
		{"items need the acting id", as("a1"), Request{Route: RouteQuery, Verb: core.QueryItems}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := p.Authorize(context.Background(), tc.who, tc.req)
			if (err == nil) != tc.want {
				t.Fatalf("Authorize = %v, want allowed=%v", err, tc.want)
			}
		})
	}
}

func TestItemRecipient(t *testing.T) {
	wg := Resource{ID: "wg1", Role: "coordinator"}
	cases := []struct {
		name          string
		subject, root Resource
		rootFound     bool
		to, workgroup string
	}{
		{"member to its coordinator", Resource{ID: "a1", RootID: "wg1"}, wg, true, "wg1", "wg1"},
		{"coordinator to console", wg, Resource{}, false, items.GlobalInbox(), "wg1"},
		{"member of archived workgroup to console", Resource{ID: "a1", RootID: "wg1"}, Resource{ID: "wg1", Role: "coordinator", Archived: true}, true, items.GlobalInbox(), ""},
		{"agent in a hidden repo root to console", Resource{ID: "one", RootID: "hidden"}, Resource{ID: "hidden", Scope: "repo"}, true, items.GlobalInbox(), ""},
		{"repo home to console", Resource{ID: "api", Role: "repo"}, Resource{}, false, items.GlobalInbox(), ""},
		{"console to console", Resource{ID: "console", Role: "console"}, Resource{}, false, items.GlobalInbox(), ""},
		{"mismatched root never routes", Resource{ID: "a1", RootID: "wg1"}, Resource{ID: "wg2", Role: "coordinator"}, true, items.GlobalInbox(), ""},
	}
	for _, c := range cases {
		to, wgID := ItemRecipient(c.subject, c.root, c.rootFound)
		if to != c.to || wgID != c.workgroup {
			t.Errorf("%s: ItemRecipient = (%q, %q), want (%q, %q)", c.name, to, wgID, c.to, c.workgroup)
		}
	}
}

func TestItemTriageRules(t *testing.T) {
	console := Resource{ID: "console", Role: "console"}
	wg1 := Resource{ID: "wg1", Role: "coordinator"}
	wg2 := Resource{ID: "wg2", Role: "coordinator"}
	a1 := Resource{ID: "a1", RootID: "wg1"}
	a2 := Resource{ID: "a2", RootID: "wg1"}
	received := ItemParties{Source: "a1", Workgroup: "wg1", Recipient: "wg1"}
	escalated := ItemParties{Source: "a1", Workgroup: "wg1", Recipient: items.GlobalInbox()}

	if !ItemVisible(a1, received) || !ItemVisible(wg1, received) || !ItemVisible(console, received) {
		t.Fatal("source, recipient or console cannot see the item")
	}
	if ItemVisible(a2, received) || ItemVisible(wg2, received) {
		t.Fatal("a sibling or a foreign coordinator sees the item")
	}
	if !ItemVisible(wg1, escalated) {
		t.Fatal("a coordinator lost sight of its workgroup's escalated item")
	}

	// The recipient triages; the source may only withdraw or complete.
	for _, st := range []string{items.StateAcknowledged, items.StateDismissed, items.StateDone, items.StateOpen} {
		if !ItemTransitionAllowed(wg1, received, st) || !ItemTransitionAllowed(console, received, st) {
			t.Errorf("recipient/console refused %s", st)
		}
	}
	if !ItemTransitionAllowed(a1, received, items.StateDismissed) || !ItemTransitionAllowed(a1, received, items.StateDone) {
		t.Error("source cannot withdraw its own item")
	}
	if ItemTransitionAllowed(a1, received, items.StateAcknowledged) || ItemTransitionAllowed(a1, received, items.StateOpen) {
		t.Error("source may acknowledge or reopen its own item")
	}
	if ItemTransitionAllowed(a2, received, items.StateDone) || ItemTransitionAllowed(wg2, received, items.StateDone) {
		t.Error("a non-party moved the item")
	}
	// Once escalated, the coordinator no longer holds it.
	if ItemTransitionAllowed(wg1, escalated, items.StateDone) {
		t.Error("coordinator moved an item it escalated")
	}

	if !ItemEscalationAllowed(wg1, received) {
		t.Error("holding coordinator cannot escalate")
	}
	for name, who := range map[string]Resource{"member": a1, "foreign coordinator": wg2, "console": console} {
		if ItemEscalationAllowed(who, received) {
			t.Errorf("%s may escalate", name)
		}
	}
	if ItemEscalationAllowed(wg1, escalated) {
		t.Error("coordinator re-escalated an item the console holds")
	}
}
