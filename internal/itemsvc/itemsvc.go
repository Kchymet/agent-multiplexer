// Package itemsvc is the internal API for upward signals (internal/items): it
// joins the item store (internal/store) with the access rules
// (internal/access, items.go) so every caller — the daemon's authenticated
// session RPC, the host control socket, and later surfaces such as a global
// inbox view or `amux ls` — raises, lists and moves items through one set of
// checks.
//
// Every method takes the id of the session acting. The service resolves it
// from daemon-owned state (Resolver), refuses an unknown or archived actor, and
// decides from the stored item, never from caller-supplied parties. An item the
// actor may not see is reported as ErrNotFound, so existence never leaks.
package itemsvc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"amux/internal/access"
	"amux/internal/items"
	"amux/internal/store"
)

// Store is the item persistence the service needs (*store.DB).
type Store interface {
	RaiseItem(source, workgroup, recipient string, r items.Raise, now int64) (items.Item, bool, error)
	Item(id string) (items.Item, bool, error)
	UpdateItem(id string, fn func(*items.Item) error) (items.Item, error)
	Items(f store.ItemFilter) ([]items.Item, bool, error)
	UnnotifiedItemRecipients() ([]string, error)
	MarkItemsNotified(recipient string, seen map[string]int64) error
}

// Service is the item API. Open, Resolver and Now are required.
type Service struct {
	// Open returns a writable store handle and its closer.
	Open func() (Store, func() error, error)
	// OpenRead returns a read-only handle for reads (nil: Open). A read never
	// creates or migrates the store.
	OpenRead func() (Store, func() error, error)
	// Resolver derives sessions' roles and membership (the daemon's access
	// resolver).
	Resolver access.Resolver
	// Now returns the current time in Unix milliseconds.
	Now func() int64
}

// OpenStore opens the daemon's store for a Service.
func OpenStore() (Store, func() error, error) {
	db, err := store.Open()
	if err != nil {
		return nil, nil, err
	}
	return db, db.Close, nil
}

// OpenReadStore opens the daemon's store read-only for a Service.
func OpenReadStore() (Store, func() error, error) {
	db, err := store.OpenReadOnly()
	if err != nil {
		return nil, nil, err
	}
	return db, db.Close, nil
}

// Errors. Messages are safe to return to the restricted caller.
var (
	ErrNotFound = errors.New("no such item")
	ErrDenied   = errors.New("not allowed for this item")
	// ErrActor: the acting session is unknown or archived.
	ErrActor = errors.New("acting session is unknown or archived")
)

// InvalidError is a request the service refused as malformed.
type InvalidError struct{ Msg string }

func (e *InvalidError) Error() string { return e.Msg }

func invalid(format string, args ...any) error {
	return &InvalidError{Msg: fmt.Sprintf(format, args...)}
}

// Query selects what List returns.
type Query struct {
	// States filters by state; empty means the active states.
	States []string
	// AllStates lists every state (overrides States).
	AllStates bool
	// Box narrows to what the actor raised (core.ItemBoxRaised) or currently
	// receives (core.ItemBoxInbox); "" is both, plus — for a coordinator —
	// its workgroup's items and — for the global inbox owner and the admin
	// console — every item.
	Box string
	// Limit bounds the page (default DefaultLimit, at most MaxLimit).
	Limit int
}

// List bounds.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// Box values (mirroring core.ItemBoxRaised / core.ItemBoxInbox).
const (
	BoxRaised = "raised"
	BoxInbox  = "inbox"
)

func (s *Service) actor(ctx context.Context, id string) (access.Resource, error) {
	if s == nil || s.Resolver == nil || s.Open == nil {
		return access.Resource{}, fmt.Errorf("item service unavailable")
	}
	if strings.TrimSpace(id) == "" {
		return access.Resource{}, ErrActor
	}
	r, ok, err := s.Resolver.Lookup(ctx, id)
	if err != nil {
		return access.Resource{}, err
	}
	if !ok || r.Archived {
		return access.Resource{}, ErrActor
	}
	return r, nil
}

func (s *Service) now() int64 {
	if s.Now != nil {
		return s.Now()
	}
	return store.Now()
}

func (s *Service) withStore(fn func(Store) error) error {
	db, closeFn, err := s.Open()
	if err != nil {
		return err
	}
	if closeFn != nil {
		defer closeFn()
	}
	return fn(db)
}

func (s *Service) withReadStore(fn func(Store) error) error {
	open := s.OpenRead
	if open == nil {
		open = s.Open
	}
	db, closeFn, err := open()
	if err != nil {
		return err
	}
	if closeFn != nil {
		defer closeFn()
	}
	return fn(db)
}

// Raise validates r and raises it from actorID, routed by access.ItemRecipient.
// created is false when an active item with the same source, kind and
// normalized title was refreshed instead.
func (s *Service) Raise(ctx context.Context, actorID string, r items.Raise) (it items.Item, created bool, err error) {
	actor, err := s.actor(ctx, actorID)
	if err != nil {
		return items.Item{}, false, err
	}
	clean, err := r.Validate()
	if err != nil {
		return items.Item{}, false, invalid("%v", err)
	}
	var root access.Resource
	rootFound := false
	if actor.RootID != "" {
		if root, rootFound, err = s.Resolver.Lookup(ctx, actor.RootID); err != nil {
			return items.Item{}, false, err
		}
	}
	recipient, workgroup := access.ItemRecipient(actor, root, rootFound)
	err = s.withStore(func(db Store) error {
		var err error
		it, created, err = db.RaiseItem(actor.ID, workgroup, recipient, clean, s.now())
		return err
	})
	if errors.Is(err, store.ErrTooManyItems) {
		return items.Item{}, false, invalid("%v", err)
	}
	return it, created, err
}

// Get reads one item the actor may see.
func (s *Service) Get(ctx context.Context, actorID, itemID string) (items.Item, error) {
	actor, err := s.actor(ctx, actorID)
	if err != nil {
		return items.Item{}, err
	}
	if !items.ValidID(itemID) {
		return items.Item{}, ErrNotFound
	}
	var it items.Item
	err = s.withReadStore(func(db Store) error {
		var found bool
		var err error
		it, found, err = db.Item(itemID)
		if err == nil && (!found || !access.ItemVisible(actor, access.PartiesOf(it))) {
			err = ErrNotFound
		}
		return err
	})
	return it, err
}

// List returns the items the actor may see that match q, newest first.
func (s *Service) List(ctx context.Context, actorID string, q Query) (items.List, error) {
	actor, err := s.actor(ctx, actorID)
	if err != nil {
		return items.List{}, err
	}
	f := store.ItemFilter{Limit: q.Limit}
	if f.Limit <= 0 {
		f.Limit = DefaultLimit
	}
	if f.Limit > MaxLimit {
		f.Limit = MaxLimit
	}
	switch {
	case q.AllStates:
	case len(q.States) > 0:
		for _, st := range q.States {
			if !items.KnownState(st) {
				return items.List{}, invalid("unknown state %q", st)
			}
		}
		f.States = q.States
	default:
		f.States = []string{items.StateOpen, items.StateAcknowledged}
	}
	switch q.Box {
	case "":
		if actor.Role != "console" && !items.IsGlobalInbox(actor.ID) {
			f.Involving = actor.ID
		}
	case BoxRaised:
		f.Source = actor.ID
	case BoxInbox:
		f.Recipient = actor.ID
	default:
		return items.List{}, invalid("unknown box %q", q.Box)
	}
	var out items.List
	err = s.withReadStore(func(db Store) error {
		rows, truncated, err := db.Items(f)
		if err != nil {
			return err
		}
		out.Truncated = truncated
		out.Items = make([]items.Item, 0, len(rows))
		for _, it := range rows {
			if access.ItemVisible(actor, access.PartiesOf(it)) {
				out.Items = append(out.Items, it)
			}
		}
		return nil
	})
	return out, err
}

// Transition moves an item by verb (items.VerbAck, VerbDismiss, VerbDone,
// VerbReopen, or a state name), recording note in its history. Reopening
// re-notifies the current recipient.
func (s *Service) Transition(ctx context.Context, actorID, itemID, verb, note string) (items.Item, error) {
	actor, err := s.actor(ctx, actorID)
	if err != nil {
		return items.Item{}, err
	}
	state, ok := items.TargetState(verb)
	if !ok {
		return items.Item{}, invalid("unknown item state change %q (want one of %s)", verb, strings.Join(items.Verbs(), ", "))
	}
	if note, err = items.CleanNote(note); err != nil {
		return items.Item{}, invalid("%v", err)
	}
	if !items.ValidID(itemID) {
		return items.Item{}, ErrNotFound
	}
	now := s.now()
	var out items.Item
	err = s.withStore(func(db Store) error {
		var err error
		out, err = db.UpdateItem(itemID, func(it *items.Item) error {
			parties := access.PartiesOf(*it)
			if !access.ItemVisible(actor, parties) {
				return ErrNotFound
			}
			if !access.ItemTransitionAllowed(actor, parties, state) {
				return ErrDenied
			}
			if !items.CanTransition(it.State, state) {
				return invalid("item %s is %s; cannot %s it", it.ID, it.State, verb)
			}
			it.State = state
			it.Updated = now
			if items.Active(state) {
				it.Closed = 0
			} else {
				it.Closed = now
			}
			// Reopening is news for the recipient; any other move is the
			// recipient (or source) having dealt with it.
			it.Unnotified = state == items.StateOpen
			it.AddEvent(items.Event{At: now, By: actor.ID, Action: items.EventFor(state), Note: note, Host: IsHost(ctx)})
			return nil
		})
		return err
	})
	if errors.Is(err, store.ErrItemNotFound) {
		err = ErrNotFound
	}
	return out, err
}

// Escalate hands an item the actor (a coordinator) holds to the global inbox,
// reopening it there and recording note in its escalation history.
func (s *Service) Escalate(ctx context.Context, actorID, itemID, note string) (items.Item, error) {
	actor, err := s.actor(ctx, actorID)
	if err != nil {
		return items.Item{}, err
	}
	if note, err = items.CleanNote(note); err != nil {
		return items.Item{}, invalid("%v", err)
	}
	if !items.ValidID(itemID) {
		return items.Item{}, ErrNotFound
	}
	now := s.now()
	var out items.Item
	err = s.withStore(func(db Store) error {
		var err error
		out, err = db.UpdateItem(itemID, func(it *items.Item) error {
			parties := access.PartiesOf(*it)
			if !access.ItemVisible(actor, parties) {
				return ErrNotFound
			}
			if !access.ItemEscalationAllowed(actor, parties) {
				return ErrDenied
			}
			if !it.Active() {
				return invalid("item %s is %s; reopen it before escalating", it.ID, it.State)
			}
			it.AddEvent(items.Event{At: now, By: actor.ID, Action: items.EventEscalated, From: it.Recipient, To: items.GlobalInbox(), Note: note})
			it.Recipient = items.GlobalInbox()
			it.State = items.StateOpen
			it.Updated = now
			it.Unnotified = true
			return nil
		})
		return err
	})
	if errors.Is(err, store.ErrItemNotFound) {
		err = ErrNotFound
	}
	return out, err
}

// UnnotifiedRecipients lists the recipients with active items they have not
// been woken for (the daemon offers each a wake-up).
func (s *Service) UnnotifiedRecipients() ([]string, error) {
	var out []string
	err := s.withReadStore(func(db Store) error {
		var err error
		out, err = db.UnnotifiedItemRecipients()
		return err
	})
	return out, err
}

// Unnotified lists a recipient's active items it has not been woken for,
// oldest first, at most limit.
func (s *Service) Unnotified(recipient string, limit int) ([]items.Item, error) {
	var out []items.Item
	err := s.withReadStore(func(db Store) error {
		rows, _, err := db.Items(store.ItemFilter{Recipient: recipient, States: []string{items.StateOpen, items.StateAcknowledged}, Limit: MaxLimit})
		if err != nil {
			return err
		}
		for i := len(rows) - 1; i >= 0; i-- {
			if rows[i].Unnotified {
				out = append(out, rows[i])
			}
		}
		return nil
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, err
}

// MarkNotified records that recipient was woken for these items (as they were
// when read); an item changed since stays unnotified.
func (s *Service) MarkNotified(recipient string, notified []items.Item) error {
	seen := make(map[string]int64, len(notified))
	for _, it := range notified {
		seen[it.ID] = it.Updated
	}
	return s.withStore(func(db Store) error { return db.MarkItemsNotified(recipient, seen) })
}
