package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"amux/internal/items"
)

// Items are the upward signals sessions raise (internal/items). Each row keeps
// the columns the daemon filters and de-duplicates on, plus the whole item as
// a JSON document, so a field added to items.Item later needs no migration.
// The table is additive (CREATE TABLE IF NOT EXISTS, no schema version bump):
// a binary that predates it ignores it, and a read-only handle on a database
// no writer has migrated yet reads it as empty.
//
// Every write runs in one BEGIN IMMEDIATE transaction, so the de-duplication
// check and its insert, and an archive and its carry-over, are atomic against
// every other writer in any process.

const itemsSchema = `
CREATE TABLE IF NOT EXISTS items (
  id         TEXT PRIMARY KEY,
  source     TEXT NOT NULL,
  kind       TEXT NOT NULL,
  dedupe     TEXT NOT NULL,
  recipient  TEXT NOT NULL,
  workgroup  TEXT NOT NULL DEFAULT '',
  state      TEXT NOT NULL,
  unnotified INTEGER NOT NULL DEFAULT 0,
  created    INTEGER NOT NULL DEFAULT 0,
  updated    INTEGER NOT NULL DEFAULT 0,
  doc        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_items_recipient ON items(recipient, state);
CREATE INDEX IF NOT EXISTS idx_items_source ON items(source, kind, dedupe);
`

// ErrItemNotFound is returned for an unknown item id.
var ErrItemNotFound = errors.New("no such item")

// ErrTooManyItems is returned when a source already has
// items.MaxActivePerSource active items.
var ErrTooManyItems = fmt.Errorf("too many active items from this session (limit %d): resolve or dismiss some first", items.MaxActivePerSource)

// activeStates is the SQL list of active states.
const activeStates = `('` + items.StateOpen + `','` + items.StateAcknowledged + `')`

// RaiseItem records a raise from source to recipient. An active item with the
// same source, kind and normalized title is refreshed instead (its detail and
// suggestion replaced, references merged, Raised incremented, its current
// recipient re-notified); created reports whether a new item was inserted.
// workgroup is the source's coordinator ("" outside a workgroup).
func (d *DB) RaiseItem(source, workgroup, recipient string, r items.Raise, now int64) (it items.Item, created bool, err error) {
	key := items.DedupeKey(r.Title)
	err = d.writeTx(func(q querier) error {
		existing, found, err := scanItem(q.QueryRow(`SELECT doc FROM items WHERE source=? AND kind=? AND dedupe=? AND state IN `+activeStates+
			` ORDER BY created DESC LIMIT 1`, source, r.Kind, key))
		if err != nil {
			return err
		}
		if found {
			it = existing
			it.Title = r.Title
			it.Detail = r.Detail
			if r.Suggest != "" {
				it.Suggest = r.Suggest
			}
			it.PRs = items.MergeRefs(it.PRs, r.PRs)
			it.Links = items.MergeRefs(it.Links, r.Links)
			it.Raised++
			it.Updated = now
			it.Unnotified = true
			it.AddEvent(items.Event{At: now, By: source, Action: items.EventUpdated})
			return putItem(q, it)
		}
		var active int
		if err := q.QueryRow(`SELECT COUNT(*) FROM items WHERE source=? AND state IN `+activeStates, source).Scan(&active); err != nil {
			return err
		}
		if active >= items.MaxActivePerSource {
			return ErrTooManyItems
		}
		it = items.Item{
			Kind: r.Kind, Title: r.Title, Detail: r.Detail, PRs: r.PRs, Links: r.Links, Suggest: r.Suggest,
			State: items.StateOpen, Source: source, Workgroup: workgroup, Recipient: recipient,
			Raised: 1, Created: now, Updated: now, Unnotified: true,
			History: []items.Event{{At: now, By: source, Action: items.EventRaised, To: recipient}},
		}
		for attempt := 0; ; attempt++ {
			it.ID = items.NewID()
			var n int
			if err := q.QueryRow(`SELECT COUNT(*) FROM items WHERE id=?`, it.ID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				break
			}
			if attempt > 8 {
				return fmt.Errorf("could not mint a unique item id")
			}
		}
		created = true
		return putItem(q, it)
	})
	return it, created, err
}

// Item reads one item.
func (d *DB) Item(id string) (items.Item, bool, error) {
	it, found, err := scanItem(d.sql.QueryRow(`SELECT doc FROM items WHERE id=?`, id))
	if err != nil && missingTable(err) {
		return items.Item{}, false, nil
	}
	return it, found, err
}

// UpdateItem reads an item, applies fn and stores the result, atomically. fn
// returning an error aborts without writing; ErrItemNotFound is returned for
// an unknown id. fn must not change the item's ID.
func (d *DB) UpdateItem(id string, fn func(*items.Item) error) (items.Item, error) {
	var out items.Item
	err := d.writeTx(func(q querier) error {
		it, found, err := scanItem(q.QueryRow(`SELECT doc FROM items WHERE id=?`, id))
		if err != nil {
			return err
		}
		if !found {
			return ErrItemNotFound
		}
		if err := fn(&it); err != nil {
			return err
		}
		it.ID = id
		out = it
		return putItem(q, it)
	})
	return out, err
}

// ItemFilter selects items. Empty fields match everything.
type ItemFilter struct {
	// Involving matches items the session raised, receives, or (for a
	// coordinator) that were raised in its workgroup; "" matches all.
	Involving string
	// Recipient matches the current recipient exactly.
	Recipient string
	// Source matches the raising session exactly.
	Source string
	// States matches any of these states; empty means every state.
	States []string
	// Limit bounds the result (newest updated first); 0 means no bound.
	Limit int
}

// Items lists items matching f, most recently updated first. truncated is
// true when Limit cut the list.
func (d *DB) Items(f ItemFilter) (out []items.Item, truncated bool, err error) {
	var where []string
	var args []any
	if f.Involving != "" {
		where = append(where, `(source=? OR recipient=? OR workgroup=?)`)
		args = append(args, f.Involving, f.Involving, f.Involving)
	}
	if f.Recipient != "" {
		where = append(where, `recipient=?`)
		args = append(args, f.Recipient)
	}
	if f.Source != "" {
		where = append(where, `source=?`)
		args = append(args, f.Source)
	}
	if len(f.States) > 0 {
		where = append(where, `state IN (?`+strings.Repeat(`,?`, len(f.States)-1)+`)`)
		for _, s := range f.States {
			args = append(args, s)
		}
	}
	query := `SELECT doc FROM items`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, ` AND `)
	}
	query += ` ORDER BY updated DESC, id`
	if f.Limit > 0 {
		query += fmt.Sprintf(` LIMIT %d`, f.Limit+1)
	}
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		if missingTable(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			return nil, false, err
		}
		var it items.Item
		if err := json.Unmarshal([]byte(doc), &it); err != nil {
			return nil, false, fmt.Errorf("decode item: %w", err)
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out, truncated = out[:f.Limit], true
	}
	return out, truncated, nil
}

// UnnotifiedItemRecipients lists the recipients with active items they have
// not been woken for.
func (d *DB) UnnotifiedItemRecipients() ([]string, error) {
	rows, err := d.sql.Query(`SELECT DISTINCT recipient FROM items WHERE unnotified=1 AND state IN ` + activeStates + ` ORDER BY recipient`)
	if err != nil {
		if missingTable(err) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkItemsNotified clears the unnotified flag of the given items (id →
// the Updated stamp the notice described) while they are still with
// recipient and unchanged since, so a change that raced the wake-up is
// notified again.
func (d *DB) MarkItemsNotified(recipient string, seen map[string]int64) error {
	if len(seen) == 0 {
		return nil
	}
	return d.writeTx(func(q querier) error {
		for id, updated := range seen {
			it, found, err := scanItem(q.QueryRow(`SELECT doc FROM items WHERE id=? AND recipient=? AND updated=?`, id, recipient, updated))
			if err != nil {
				return err
			}
			if !found || !it.Unnotified {
				continue
			}
			it.Unnotified = false
			if err := putItem(q, it); err != nil {
				return err
			}
		}
		return nil
	})
}

// carryOverItems moves every active item whose recipient is id to the
// global inbox (items.GlobalInbox), recording why. It runs inside the archive or delete of id,
// so an item is never left with a recipient that can no longer act on it.
func carryOverItems(q querier, id, reason string, now int64) error {
	if id == "" || items.IsGlobalInbox(id) {
		return nil
	}
	rows, err := q.Query(`SELECT doc FROM items WHERE recipient=? AND state IN `+activeStates, id)
	if err != nil {
		if missingTable(err) {
			return nil
		}
		return err
	}
	var moved []items.Item
	for rows.Next() {
		var doc string
		if err := rows.Scan(&doc); err != nil {
			rows.Close()
			return err
		}
		var it items.Item
		if err := json.Unmarshal([]byte(doc), &it); err != nil {
			rows.Close()
			return fmt.Errorf("decode item: %w", err)
		}
		moved = append(moved, it)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, it := range moved {
		it.Recipient = items.GlobalInbox()
		it.State = items.StateOpen
		it.Updated = now
		it.Unnotified = true
		it.AddEvent(items.Event{At: now, Action: items.EventCarriedOver, From: id, To: items.GlobalInbox(), Note: reason})
		if err := putItem(q, it); err != nil {
			return err
		}
	}
	return nil
}

func putItem(q querier, it items.Item) error {
	doc, err := json.Marshal(it)
	if err != nil {
		return err
	}
	_, err = q.Exec(`INSERT INTO items(id,source,kind,dedupe,recipient,workgroup,state,unnotified,created,updated,doc)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET source=excluded.source, kind=excluded.kind, dedupe=excluded.dedupe,
		  recipient=excluded.recipient, workgroup=excluded.workgroup, state=excluded.state,
		  unnotified=excluded.unnotified, created=excluded.created, updated=excluded.updated, doc=excluded.doc`,
		it.ID, it.Source, it.Kind, items.DedupeKey(it.Title), it.Recipient, it.Workgroup, it.State,
		b2i(it.Unnotified), it.Created, it.Updated, string(doc))
	return err
}

func scanItem(row *sql.Row) (items.Item, bool, error) {
	var doc string
	if err := row.Scan(&doc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return items.Item{}, false, nil
		}
		return items.Item{}, false, err
	}
	var it items.Item
	if err := json.Unmarshal([]byte(doc), &it); err != nil {
		return items.Item{}, false, fmt.Errorf("decode item: %w", err)
	}
	return it, true, nil
}
