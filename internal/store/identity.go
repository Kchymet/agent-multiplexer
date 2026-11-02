package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"amux/internal/accounts"
)

// identity.go is the store's session incarnation: a random 128-bit token the
// store generates when it INSERTs a session row and never changes afterwards.
// It tells "the session this id named when it was created" apart from a later,
// unrelated session that reuses the id (after a delete, or in a replaced or
// restored store), which the id alone cannot do.
//
// The incarnation is store-local and host-only. It is deliberately not a field
// of Session (or of core.Session, the published harness rows, provider
// inventory, mux frames or session RPC): only CreateSession and
// SessionIdentities return it, so no caller can supply or overwrite one, and
// no ordinary listing discloses it. Every other writer (PutSession's upsert and
// the field setters) leaves the column alone.

// Identity is what a creation reports: the created session and its workgroup
// root, each with its incarnation. For a new workgroup the root is the session
// itself.
type Identity struct {
	ID              string `json:"id"`
	Incarnation     string `json:"incarnation"`
	RootID          string `json:"rootId"`
	RootIncarnation string `json:"rootIncarnation"`
}

// Complete reports whether every field is set.
func (i Identity) Complete() bool {
	return i.ID != "" && i.Incarnation != "" && i.RootID != "" && i.RootIncarnation != ""
}

// SessionIdentity is one existing session's current incarnation and parent
// (RootID "" for a root). Incarnation is "" only for a row an older binary
// inserted after this store was migrated and before the next backfill: such a
// row matches no recorded incarnation.
type SessionIdentity struct {
	ID          string `json:"id"`
	Incarnation string `json:"incarnation"`
	RootID      string `json:"rootId"`
}

// MaxIdentityLookup bounds one SessionIdentities call.
const MaxIdentityLookup = 64

// ErrRootChanged refuses a member creation whose workgroup root is missing, is
// not a root, or is no longer the incarnation the caller expected.
var ErrRootChanged = errors.New("the workgroup root is missing or was replaced")

// ErrSessionExists refuses CreateSession for an id that already has a row.
var ErrSessionExists = errors.New("a session with this id already exists")

// newIncarnation returns 128 random bits, hex encoded.
func newIncarnation() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// backfillIncarnationSQL gives every row without an incarnation a fresh one.
// SQLite's randomblob is its CSPRNG; each row gets its own value.
const backfillIncarnationSQL = `UPDATE sessions SET incarnation=lower(hex(randomblob(16))) WHERE incarnation=''`

// migrateIncarnations adds the column, its partial index and the backfill. It
// runs inside migrateSchema's write transaction, so a database is never seen
// with the column but without incarnations.
func migrateIncarnations(q querier) error {
	if err := addColumn(q, "sessions", "incarnation", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// The partial index keeps the per-open probe (needsIncarnationBackfill) a
	// lookup of an almost always empty index instead of a table scan.
	if _, err := q.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_no_incarnation ON sessions(id) WHERE incarnation=''`); err != nil {
		return err
	}
	_, err := q.Exec(backfillIncarnationSQL)
	return err
}

// hasIncarnations reports whether the sessions table has the incarnation
// column (one schema read; see openMigrated).
func (d *DB) hasIncarnations() (bool, error) {
	var n int
	err := d.sql.QueryRow(`SELECT count(*) FROM pragma_table_info('sessions') WHERE name='incarnation'`).Scan(&n)
	return n > 0, err
}

// backfillIncarnations repairs rows an older binary inserted (with the
// column's empty-string default) after the migration. Normal opens stay read-only: the
// probe is a read, and only a store that needs it takes the write reservation.
func (d *DB) backfillIncarnations() error {
	var needed bool
	if err := d.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM sessions WHERE incarnation='')`).Scan(&needed); err != nil {
		return err
	}
	if !needed {
		return nil
	}
	return d.writeTx(func(q querier) error {
		_, err := q.Exec(backfillIncarnationSQL)
		return err
	})
}

// CreateSession inserts a new session row with a fresh incarnation and returns
// its identity. It never overwrites: an existing id is ErrSessionExists.
//
// For a member (s.RootID != "") the root is checked in the same write
// transaction as the insert: it must exist, be a root and, when expectRoot is
// set, still be that incarnation; otherwise ErrRootChanged and nothing is
// written. A root with no incarnation yet (an older binary's row) gets one
// first, so the returned identity is always complete. For a root, RootID and
// RootIncarnation are its own.
func (d *DB) CreateSession(s Session, expectRoot string) (Identity, error) {
	if s.ID == "" {
		return Identity{}, fmt.Errorf("create session: empty id")
	}
	if s.RootID == "" && expectRoot != "" {
		// A root has no parent to expect; refuse rather than ignore.
		return Identity{}, ErrRootChanged
	}
	// Read-only prechecks before the account binding, as PutSession binds
	// before it persists: a refusal known now leaves no binding behind, and an
	// existing id is reported as ErrSessionExists, never masked by the account
	// store's ErrBlocked for that id's binding. No store transaction is held
	// while binding (the account store has its own lock), so the lock order is
	// unchanged. Limitation: a refusal that only the transaction below detects
	// (the root deleted or replaced, or the id taken, between this check and the
	// insert) can leave a binding for an id that has no row; nothing reads it,
	// and a later session reusing the id with the same harness reuses it.
	if err := d.precheckCreate(s, expectRoot); err != nil {
		return Identity{}, err
	}
	if err := accounts.Bind(s.ID, s.Agent); err != nil {
		return Identity{}, err
	}
	afterCreatePrecheck(s)
	id := Identity{ID: s.ID, Incarnation: newIncarnation()}
	err := d.writeTx(func(q querier) error {
		if s.RootID == "" {
			id.RootID, id.RootIncarnation = s.ID, id.Incarnation
		} else {
			if _, err := q.Exec(`UPDATE sessions SET incarnation=lower(hex(randomblob(16))) WHERE id=? AND incarnation=''`, s.RootID); err != nil {
				return err
			}
			var parent, inc string
			switch err := q.QueryRow(`SELECT root_id,incarnation FROM sessions WHERE id=?`, s.RootID).Scan(&parent, &inc); {
			case errors.Is(err, sql.ErrNoRows):
				return ErrRootChanged
			case err != nil:
				return err
			}
			if parent != "" || inc == "" || (expectRoot != "" && inc != expectRoot) {
				return ErrRootChanged
			}
			id.RootID, id.RootIncarnation = s.RootID, inc
		}
		var one int
		switch err := q.QueryRow(`SELECT 1 FROM sessions WHERE id=?`, s.ID).Scan(&one); {
		case err == nil:
			return ErrSessionExists
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err := q.Exec(
			`INSERT INTO sessions(id,root_id,name,agent,model,mode,repo,branch,dir,claude_id,prompt,created,scope,archived,archived_at,incarnation)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			s.ID, s.RootID, s.Name, s.Agent, s.Model, s.Mode, s.Repo, s.Branch, s.Dir, s.ClaudeID, s.Prompt, s.Created, s.Scope, b2i(s.Archived), s.ArchivedAt, id.Incarnation)
		return err
	})
	if err != nil {
		return Identity{}, err
	}
	return id, nil
}

// afterCreatePrecheck is a test seam between CreateSession's read-only
// precheck and its write transaction.
var afterCreatePrecheck = func(Session) {}

// precheckCreate is CreateSession's read-only early refusal. The transaction
// repeats every check atomically with the insert.
func (d *DB) precheckCreate(s Session, expectRoot string) error {
	ids := []string{s.ID}
	if s.RootID != "" {
		ids = append(ids, s.RootID)
	}
	cur, err := d.SessionIdentities(ids)
	if err != nil {
		return err
	}
	if _, taken := cur[s.ID]; taken {
		return ErrSessionExists
	}
	if s.RootID != "" {
		root, ok := cur[s.RootID]
		// An empty root incarnation is repaired inside the transaction, so it
		// is refused here only when the caller pinned one.
		if !ok || root.RootID != "" || (expectRoot != "" && root.Incarnation != expectRoot) {
			return ErrRootChanged
		}
	}
	return nil
}

// SessionIdentities returns the current identity of each id that exists, keyed
// by id; a missing id is absent from the map. At most MaxIdentityLookup ids.
func (d *DB) SessionIdentities(ids []string) (map[string]SessionIdentity, error) {
	if len(ids) > MaxIdentityLookup {
		return nil, fmt.Errorf("at most %d session identities per lookup", MaxIdentityLookup)
	}
	out := make(map[string]SessionIdentity, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := d.sql.Query(`SELECT id,incarnation,root_id FROM sessions WHERE id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s SessionIdentity
		if err := rows.Scan(&s.ID, &s.Incarnation, &s.RootID); err != nil {
			return nil, err
		}
		out[s.ID] = s
	}
	return out, rows.Err()
}
