package itemsvc

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"amux/internal/access"
	"amux/internal/items"
	"amux/internal/store"
)

type fakeResolver map[string]access.Resource

func (f fakeResolver) Lookup(_ context.Context, id string) (access.Resource, bool, error) {
	r, ok := f[id]
	return r, ok, nil
}
func (fakeResolver) RepoGranted(context.Context, string, string) (bool, error) { return false, nil }
func (fakeResolver) ProviderAllows(context.Context, string, access.Request) (bool, error) {
	return false, nil
}

func newService(t *testing.T) (*Service, fakeResolver) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	db, err := store.Open() // migrate once
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	res := fakeResolver{
		"console": {ID: "console", Role: "console"},
		"wg1":     {ID: "wg1", Role: "coordinator"},
		"wg2":     {ID: "wg2", Role: "coordinator"},
		"a1":      {ID: "a1", RootID: "wg1"},
		"a2":      {ID: "a2", RootID: "wg1"},
		"b1":      {ID: "b1", RootID: "wg2"},
		"old":     {ID: "old", RootID: "wg1", Archived: true},
	}
	now := int64(0)
	return &Service{Open: OpenStore, OpenRead: OpenReadStore, Resolver: res, Now: func() int64 { now++; return now }}, res
}

func TestRaiseRoutesUpward(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	it, created, err := svc.Raise(ctx, "a1", items.Raise{Kind: items.KindFollowUp, Title: "Add docs", PRs: []string{"repo#4"}})
	if err != nil || !created || it.Recipient != "wg1" || it.Workgroup != "wg1" || it.Source != "a1" {
		t.Fatalf("member raise = %+v created=%v err=%v", it, created, err)
	}
	it, _, err = svc.Raise(ctx, "wg1", items.Raise{Kind: items.KindDecisionNeeded, Title: "Ship?"})
	if err != nil || it.Recipient != items.GlobalInbox() || it.Workgroup != "wg1" {
		t.Fatalf("coordinator raise = %+v err=%v", it, err)
	}
	if _, _, err := svc.Raise(ctx, "old", items.Raise{Kind: items.KindRisk, Title: "x"}); !errors.Is(err, ErrActor) {
		t.Fatalf("archived raise = %v, want ErrActor", err)
	}
	if _, _, err := svc.Raise(ctx, "ghost", items.Raise{Kind: items.KindRisk, Title: "x"}); !errors.Is(err, ErrActor) {
		t.Fatalf("unknown raise = %v, want ErrActor", err)
	}
	var inv *InvalidError
	if _, _, err := svc.Raise(ctx, "a1", items.Raise{Kind: "nope", Title: "x"}); !errors.As(err, &inv) {
		t.Fatalf("invalid kind = %v, want InvalidError", err)
	}
	// Re-raise de-duplicates.
	again, created, err := svc.Raise(ctx, "a1", items.Raise{Kind: items.KindFollowUp, Title: "add docs"})
	if err != nil || created || again.Raised != 2 {
		t.Fatalf("re-raise = %+v created=%v err=%v", again, created, err)
	}
}

func TestListIsScopedPerActor(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	mustRaise := func(who, title string) items.Item {
		it, _, err := svc.Raise(ctx, who, items.Raise{Kind: items.KindRisk, Title: title})
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	fromA1 := mustRaise("a1", "a1 risk")
	mustRaise("a2", "a2 risk")
	mustRaise("b1", "b1 risk")
	fromWG1 := mustRaise("wg1", "wg1 risk")

	count := func(who string, q Query) int {
		t.Helper()
		l, err := svc.List(ctx, who, q)
		if err != nil {
			t.Fatal(err)
		}
		return len(l.Items)
	}
	if n := count("a1", Query{}); n != 1 {
		t.Fatalf("a1 sees %d, want its own 1", n)
	}
	if n := count("wg1", Query{}); n != 3 {
		t.Fatalf("wg1 sees %d, want its members' 2 + its own 1", n)
	}
	if n := count("wg1", Query{Box: BoxInbox}); n != 2 {
		t.Fatalf("wg1 inbox = %d, want 2", n)
	}
	if n := count("wg1", Query{Box: BoxRaised}); n != 1 {
		t.Fatalf("wg1 raised = %d, want 1", n)
	}
	if n := count("wg2", Query{}); n != 1 {
		t.Fatalf("wg2 sees %d, want 1", n)
	}
	if n := count("console", Query{}); n != 4 {
		t.Fatalf("console sees %d, want all 4", n)
	}
	if n := count("console", Query{Box: BoxInbox}); n != 1 {
		t.Fatalf("console inbox = %d, want wg1's 1", n)
	}
	if _, err := svc.Get(ctx, "a2", fromA1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sibling Get = %v, want ErrNotFound", err)
	}
	if _, err := svc.Get(ctx, "wg2", fromWG1.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign coordinator Get = %v, want ErrNotFound", err)
	}
	if it, err := svc.Get(ctx, "console", fromA1.ID); err != nil || it.ID != fromA1.ID {
		t.Fatalf("console Get = %+v, %v", it, err)
	}
	var inv *InvalidError
	if _, err := svc.List(ctx, "a1", Query{States: []string{"bogus"}}); !errors.As(err, &inv) {
		t.Fatalf("bogus state = %v", err)
	}
}

func TestTriageAndEscalation(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	it, _, err := svc.Raise(ctx, "a1", items.Raise{Kind: items.KindDecisionNeeded, Title: "Which DB?"})
	if err != nil {
		t.Fatal(err)
	}
	// Non-parties cannot touch it; a sibling does not even learn it exists.
	if _, err := svc.Transition(ctx, "a2", it.ID, items.VerbDone, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("sibling transition = %v", err)
	}
	if _, err := svc.Escalate(ctx, "a1", it.ID, ""); !errors.Is(err, ErrDenied) {
		t.Fatalf("member escalation = %v, want ErrDenied", err)
	}
	if _, err := svc.Escalate(ctx, "wg2", it.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign escalation = %v", err)
	}
	// The source cannot acknowledge its own item.
	if _, err := svc.Transition(ctx, "a1", it.ID, items.VerbAck, ""); !errors.Is(err, ErrDenied) {
		t.Fatalf("source ack = %v, want ErrDenied", err)
	}
	acked, err := svc.Transition(ctx, "wg1", it.ID, items.VerbAck, "looking")
	if err != nil || acked.State != items.StateAcknowledged || acked.Unnotified {
		t.Fatalf("ack = %+v, %v", acked, err)
	}
	var inv *InvalidError
	if _, err := svc.Transition(ctx, "wg1", it.ID, items.VerbAck, ""); !errors.As(err, &inv) {
		t.Fatalf("double ack = %v, want InvalidError", err)
	}
	up, err := svc.Escalate(ctx, "wg1", it.ID, "needs the user")
	if err != nil || up.Recipient != items.GlobalInbox() || up.State != items.StateOpen || !up.Unnotified {
		t.Fatalf("escalate = %+v, %v", up, err)
	}
	last := up.History[len(up.History)-1]
	if last.Action != items.EventEscalated || last.By != "wg1" || last.From != "wg1" || last.To != items.GlobalInbox() || last.Note != "needs the user" {
		t.Fatalf("escalation history = %+v", last)
	}
	// The coordinator no longer holds it; the console does.
	if _, err := svc.Transition(ctx, "wg1", it.ID, items.VerbDone, ""); !errors.Is(err, ErrDenied) {
		t.Fatalf("coordinator moved an escalated item: %v", err)
	}
	if _, err := svc.Escalate(ctx, "wg1", it.ID, ""); !errors.Is(err, ErrDenied) {
		t.Fatalf("re-escalation = %v", err)
	}
	done, err := svc.Transition(ctx, "console", it.ID, items.VerbDone, "decided: sqlite")
	if err != nil || done.State != items.StateDone || done.Closed == 0 {
		t.Fatalf("console done = %+v, %v", done, err)
	}
	reopened, err := svc.Transition(ctx, "console", it.ID, items.VerbReopen, "")
	if err != nil || reopened.State != items.StateOpen || reopened.Closed != 0 || !reopened.Unnotified {
		t.Fatalf("reopen = %+v, %v", reopened, err)
	}
	// The source may withdraw its own item.
	if w, err := svc.Transition(ctx, "a1", it.ID, items.VerbDismiss, "moot"); err != nil || w.State != items.StateDismissed {
		t.Fatalf("source withdraw = %+v, %v", w, err)
	}
	if _, err := svc.Transition(ctx, "console", "it-nothere", items.VerbDone, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("malformed id = %v", err)
	}
	if _, err := svc.Transition(ctx, "console", "it-00000000", items.VerbDone, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id = %v", err)
	}
}

func TestUnnotifiedAndMarkNotified(t *testing.T) {
	svc, _ := newService(t)
	ctx := context.Background()
	for _, title := range []string{"one", "two"} {
		if _, _, err := svc.Raise(ctx, "a1", items.Raise{Kind: items.KindFYI, Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	recipients, err := svc.UnnotifiedRecipients()
	if err != nil || len(recipients) != 1 || recipients[0] != "wg1" {
		t.Fatalf("recipients = %v, %v", recipients, err)
	}
	pending, err := svc.Unnotified("wg1", 0)
	if err != nil || len(pending) != 2 || pending[0].Title != "one" {
		t.Fatalf("pending = %+v, %v (want oldest first)", pending, err)
	}
	if err := svc.MarkNotified("wg1", pending); err != nil {
		t.Fatal(err)
	}
	if recipients, _ := svc.UnnotifiedRecipients(); len(recipients) != 0 {
		t.Fatalf("still unnotified: %v", recipients)
	}
}
