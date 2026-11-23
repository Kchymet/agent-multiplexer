package store

import (
	"errors"
	"testing"

	"amux/internal/items"
)

func raiseOK(t *testing.T, db *DB, source, wg, to string, r items.Raise, now int64) (items.Item, bool) {
	t.Helper()
	r, err := r.Validate()
	if err != nil {
		t.Fatal(err)
	}
	it, created, err := db.RaiseItem(source, wg, to, r, now)
	if err != nil {
		t.Fatal(err)
	}
	return it, created
}

func TestRaiseItemDeduplicatesActiveItems(t *testing.T) {
	db := openTemp(t)
	first, created := raiseOK(t, db, "m1", "wg", "wg", items.Raise{Kind: items.KindFollowUp, Title: "Add docs", PRs: []string{"r#1"}}, 10)
	if !created || first.Recipient != "wg" || first.State != items.StateOpen || !first.Unnotified || first.Raised != 1 {
		t.Fatalf("first raise = %+v created=%v", first, created)
	}
	// Same source, kind and normalized title: the open item is updated.
	again, created := raiseOK(t, db, "m1", "wg", "wg", items.Raise{Kind: items.KindFollowUp, Title: "add  DOCS.", Detail: "more", PRs: []string{"r#2"}}, 20)
	if created || again.ID != first.ID || again.Raised != 2 || again.Detail != "more" || len(again.PRs) != 2 || again.Updated != 20 {
		t.Fatalf("re-raise = %+v created=%v", again, created)
	}
	// Another kind, another source, or another title is a new item.
	for _, c := range []struct{ src, kind, title string }{
		{"m1", items.KindRisk, "Add docs"}, {"m2", items.KindFollowUp, "Add docs"}, {"m1", items.KindFollowUp, "Add tests"},
	} {
		if it, created := raiseOK(t, db, c.src, "wg", "wg", items.Raise{Kind: c.kind, Title: c.title}, 30); !created || it.ID == first.ID {
			t.Fatalf("%+v deduplicated onto %s", c, first.ID)
		}
	}
	// A closed item no longer absorbs raises.
	if _, err := db.UpdateItem(first.ID, func(it *items.Item) error { it.State = items.StateDone; return nil }); err != nil {
		t.Fatal(err)
	}
	if it, created := raiseOK(t, db, "m1", "wg", "wg", items.Raise{Kind: items.KindFollowUp, Title: "Add docs"}, 40); !created || it.ID == first.ID {
		t.Fatalf("raise after done reused %s", first.ID)
	}
}

func TestRaiseItemBoundsActiveItemsPerSource(t *testing.T) {
	db := openTemp(t)
	for i := 0; i < items.MaxActivePerSource; i++ {
		raiseOK(t, db, "m1", "", items.ConsoleRecipient, items.Raise{Kind: items.KindFYI, Title: "note " + string(rune('A'+i%26)) + string(rune('a'+i/26))}, int64(i))
	}
	r, _ := items.Raise{Kind: items.KindFYI, Title: "one too many"}.Validate()
	if _, _, err := db.RaiseItem("m1", "", items.ConsoleRecipient, r, 1000); !errors.Is(err, ErrTooManyItems) {
		t.Fatalf("raise past the bound = %v, want ErrTooManyItems", err)
	}
	// A re-raise of an existing item still updates it.
	r, _ = items.Raise{Kind: items.KindFYI, Title: "note Aa"}.Validate()
	if _, created, err := db.RaiseItem("m1", "", items.ConsoleRecipient, r, 1001); err != nil || created {
		t.Fatalf("re-raise at the bound = created %v, %v", created, err)
	}
}

func TestItemsFilters(t *testing.T) {
	db := openTemp(t)
	a, _ := raiseOK(t, db, "m1", "wg", "wg", items.Raise{Kind: items.KindRisk, Title: "a"}, 1)
	b, _ := raiseOK(t, db, "wg", "wg", items.ConsoleRecipient, items.Raise{Kind: items.KindRisk, Title: "b"}, 2)
	c, _ := raiseOK(t, db, "x1", "wg2", "wg2", items.Raise{Kind: items.KindRisk, Title: "c"}, 3)
	ids := func(f ItemFilter) []string {
		t.Helper()
		got, _, err := db.Items(f)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, it := range got {
			out = append(out, it.ID)
		}
		return out
	}
	if got := ids(ItemFilter{}); len(got) != 3 || got[0] != c.ID {
		t.Fatalf("all = %v (want newest first)", got)
	}
	if got := ids(ItemFilter{Involving: "wg"}); len(got) != 2 {
		t.Fatalf("involving wg = %v, want %s and %s", got, a.ID, b.ID)
	}
	if got := ids(ItemFilter{Involving: "m1"}); len(got) != 1 || got[0] != a.ID {
		t.Fatalf("involving m1 = %v", got)
	}
	if got := ids(ItemFilter{Recipient: items.ConsoleRecipient}); len(got) != 1 || got[0] != b.ID {
		t.Fatalf("console inbox = %v", got)
	}
	if got, truncated, _ := db.Items(ItemFilter{Limit: 2}); len(got) != 2 || !truncated {
		t.Fatalf("limit 2 = %d items truncated=%v", len(got), truncated)
	}
	if got := ids(ItemFilter{States: []string{items.StateDone}}); len(got) != 0 {
		t.Fatalf("done = %v", got)
	}
}

func TestArchiveCarriesActiveItemsToConsole(t *testing.T) {
	db := openTemp(t)
	if err := db.PutSession(Session{ID: "wg", Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	open, _ := raiseOK(t, db, "m1", "wg", "wg", items.Raise{Kind: items.KindDecisionNeeded, Title: "pick"}, 1)
	acked, _ := raiseOK(t, db, "m2", "wg", "wg", items.Raise{Kind: items.KindRisk, Title: "flaky"}, 2)
	done, _ := raiseOK(t, db, "m3", "wg", "wg", items.Raise{Kind: items.KindFYI, Title: "noted"}, 3)
	if _, err := db.UpdateItem(acked.ID, func(it *items.Item) error { it.State = items.StateAcknowledged; it.Unnotified = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateItem(done.ID, func(it *items.Item) error { it.State = items.StateDone; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := db.SetArchivedFlag("wg", true, 99); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{open.ID, acked.ID} {
		it, _, _ := db.Item(id)
		last := it.History[len(it.History)-1]
		if it.Recipient != items.ConsoleRecipient || it.State != items.StateOpen || !it.Unnotified ||
			last.Action != items.EventCarriedOver || last.From != "wg" {
			t.Fatalf("%s after archive = %+v", id, it)
		}
	}
	if it, _, _ := db.Item(done.ID); it.Recipient != "wg" {
		t.Fatalf("closed item moved: %+v", it)
	}
	if s, _, _ := db.GetSession("wg"); !s.Archived || s.ArchivedAt != 99 {
		t.Fatalf("archive flag not written: %+v", s)
	}
	recipients, err := db.UnnotifiedItemRecipients()
	if err != nil || len(recipients) != 1 || recipients[0] != items.ConsoleRecipient {
		t.Fatalf("unnotified recipients = %v, %v", recipients, err)
	}
}

func TestDeleteCarriesActiveItemsToConsole(t *testing.T) {
	db := openTemp(t)
	if err := db.PutSession(Session{ID: "wg", Agent: "claude"}); err != nil {
		t.Fatal(err)
	}
	it, _ := raiseOK(t, db, "m1", "wg", "wg", items.Raise{Kind: items.KindOutstanding, Title: "half done"}, 1)
	if err := db.DeleteSession("wg"); err != nil {
		t.Fatal(err)
	}
	got, found, err := db.Item(it.ID)
	if err != nil || !found || got.Recipient != items.ConsoleRecipient {
		t.Fatalf("after delete = %+v found=%v err=%v", got, found, err)
	}
}

func TestMarkItemsNotifiedSkipsChangedItems(t *testing.T) {
	db := openTemp(t)
	a, _ := raiseOK(t, db, "m1", "wg", "wg", items.Raise{Kind: items.KindRisk, Title: "a"}, 1)
	b, _ := raiseOK(t, db, "m1", "wg", "wg", items.Raise{Kind: items.KindRisk, Title: "b"}, 1)
	// b changes after the notice was composed.
	raiseOK(t, db, "m1", "wg", "wg", items.Raise{Kind: items.KindRisk, Title: "b", Detail: "new"}, 5)
	if err := db.MarkItemsNotified("wg", map[string]int64{a.ID: a.Updated, b.ID: b.Updated}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := db.Item(a.ID); got.Unnotified {
		t.Fatal("a stayed unnotified")
	}
	if got, _, _ := db.Item(b.ID); !got.Unnotified {
		t.Fatal("b's newer change was marked notified")
	}
	// Wrong recipient (escalated meanwhile) is left alone too.
	if err := db.MarkItemsNotified("other", map[string]int64{b.ID: 5}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := db.Item(b.ID); !got.Unnotified {
		t.Fatal("another recipient cleared b's notice")
	}
}
