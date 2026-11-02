// Package store is the SQLite-backed registry of tracked repositories and
// sessions. Sessions form a one-level hierarchy: a root session (RootID == "")
// is a container; its sub-sessions (RootID == root's id) each bind one agent to
// one worktree. Each process opens the DB independently; WAL + a busy timeout
// handle the daemon polling while the CLI writes.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"amux/internal/accounts"
	"amux/internal/core"

	_ "modernc.org/sqlite"
)

// Repo is a tracked repository: a local bare clone used as a worktree source.
type Repo struct {
	Name   string
	Source string
	GitDir string
}

// Session is a root container (RootID == "") or a sub-session (an agent + a
// worktree under a root).
type Session struct {
	ID       string
	RootID   string // "" => this is a root container
	Name     string // optional display name (agent-settable)
	Agent    string // claude | codex | hermes
	Model    string // optional per-session model override
	Mode     string // task | interactive
	Repo     string // repo name (sub-sessions); may list several for migrated roots
	Branch   string // git branch (sub-sessions)
	Dir      string // worktree dir (sub-sessions) / container dir (roots)
	ClaudeID string // pinned claude conversation id (resume across restarts)
	Prompt   string // initial prompt
	Created  int64
	Scope    string // roots only: work (coordinator workgroup) | repo (management home)
	Archived bool   // marked done/archived: hidden from the active rail (reversible)
	// ArchivedAt is the store.Now() timestamp at which Archived last became true,
	// or 0 when not archived. Distinct from Created so the rail can order the
	// ARCHIVED section by when agents were archived, not when they were made.
	ArchivedAt int64
}

// IsRoot reports whether s is a root container.
func (s Session) IsRoot() bool { return s.RootID == "" }

// Display is the human label: name if set, else id.
func (s Session) Display() string {
	if strings.TrimSpace(s.Name) != "" {
		return s.Name
	}
	return s.ID
}

// DB is a handle to the session store.
type DB struct {
	sql *sql.DB
	// shared marks a process-wide handle (see ShareHandles); Close leaves it open.
	shared bool
}

// The schema version range says which databases this build can migrate and
// open. Version zero is the legacy, unmarked schema used before explicit
// versioning; Open migrates it in place. Future migrations advance the current
// version only after their changes have completed successfully.
const (
	MinSchemaVersion     = 0
	CurrentSchemaVersion = 3
)

// SchemaVersionError means the database is outside the range this daemon can
// safely migrate and open. Refusing before migration prevents a binary from
// making assumptions about a schema it does not understand.
type SchemaVersionError struct {
	Have int
	Min  int
	Max  int
}

func (e *SchemaVersionError) Error() string {
	if e.Min == e.Max {
		return fmt.Sprintf("database schema %d is unsupported by this daemon (requires %d)", e.Have, e.Min)
	}
	return fmt.Sprintf("database schema %d is unsupported by this daemon (supported range %d-%d)", e.Have, e.Min, e.Max)
}

// Open opens (creating if needed) the store, applies the schema, and imports any
// legacy JSON registry once.
func Open() (*DB, error) {
	if err := os.MkdirAll(core.DataDir(), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + core.DBPath() + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)"
	sqldb, shared, err := openHandle(dsn)
	if err != nil {
		return nil, err
	}
	d := &DB{sql: sqldb, shared: shared}
	path := core.DBPath()
	if ok, err := d.openMigrated(path); err != nil {
		return nil, err
	} else if ok {
		return d, nil
	}
	// First open of this database in this process. Concurrent first opens (the
	// daemon starts its poll and request loops together) single-flight here and
	// recheck, so only one goroutine migrates and the rest take the fast path.
	// Other processes are serialized by the write transactions below, not by
	// this lock.
	unlock := lockMigration(path)
	defer unlock()
	if ok, err := d.openMigrated(path); err != nil {
		return nil, err
	} else if ok {
		return d, nil
	}
	if err := ensureWAL(path); err != nil {
		_ = d.Close()
		return nil, err
	}
	if err := d.migrate(); err != nil {
		_ = d.Close()
		return nil, err
	}
	if err := d.importLegacy(); err != nil {
		// A registry that cannot be parsed is left in place and does not block
		// usage, as before. The store is still recorded as migrated below, so
		// this process's hot path stays read-only, and a fixed file is imported
		// by the next process's first Open (a restart). Any other error means
		// the import rolled back with its file still in place, so fail this
		// Open; the next one retries it.
		var unreadable *LegacyRegistryError
		if !errors.As(err, &unreadable) {
			_ = d.Close()
			return nil, err
		}
		log.Printf("amux: %v. Nothing was imported. To import it, fix the file and restart amux "+
			"before creating any session: a store that has sessions never imports the registry.", err)
	}
	if err := d.migrateCoordinatorGrants(); err != nil {
		_ = d.Close()
		return nil, err
	}
	if err := d.migrateRepoWorkgroups(); err != nil {
		_ = d.Close()
		return nil, err
	}
	if version, err := d.SchemaVersion(); err == nil && version == CurrentSchemaVersion {
		migratedStores.Store(path, struct{}{})
	}
	return d, nil
}

// openMigrated is Open's fast path. It reports true when this process already
// brought the database at path to the current schema and ran its one-time
// backfills. Opening is on the daemon's hot path (every reconcile tick and
// session lookup), so it skips the DDL, legacy import and grant backfill and
// keeps only the cheap checks: the version (a newer binary's schema or a
// recreated file takes the full path and fails or migrates exactly as a first
// open would) and the single-query legacy workgroup probe, which an older
// binary's write could still satisfy. On error d is closed.
func (d *DB) openMigrated(path string) (bool, error) {
	if _, done := migratedStores.Load(path); !done {
		return false, nil
	}
	if version, err := d.SchemaVersion(); err == nil && version == CurrentSchemaVersion {
		if err := d.migrateRepoWorkgroups(); err != nil {
			_ = d.Close()
			return false, err
		}
		// The version cannot tell a current store from a pre-incarnation
		// schema-3 file restored over this path since it was migrated: the
		// column is additive within version 3 so older binaries keep opening
		// it. Without the column, take the full migration path again.
		if present, err := d.hasIncarnations(); err != nil {
			_ = d.Close()
			return false, err
		} else if !present {
			migratedStores.Delete(path)
			return false, nil
		}
		// An older binary may have inserted rows since (with no incarnation).
		if err := d.backfillIncarnations(); err != nil {
			_ = d.Close()
			return false, err
		}
		return true, nil
	}
	migratedStores.Delete(path)
	return false, nil
}

// walSwitchTimeout bounds ensureWAL's retries; it matches the DSN busy timeout.
const walSwitchTimeout = 5 * time.Second

// ensureWAL switches a rollback-journal database (a new file, or one created
// before amux used WAL) to WAL before the store's pool opens connections.
// Every pooled connection also requests WAL in its DSN, but that switch needs
// an exclusive lock, and when another process holds the file SQLite can report
// SQLITE_BUSY at once instead of waiting out busy_timeout, so racing first
// opens of a new store failed while opening a connection. Here the switch is retried from scratch, on SQLITE_BUSY only,
// until walSwitchTimeout. Once the file is in WAL mode (which persists) the
// pragma is a no-op that needs no exclusive lock, so this costs one cheap
// statement per process and later connections cannot hit the error.
func ensureWAL(path string) error {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	deadline := time.Now().Add(walSwitchTimeout)
	for {
		var mode string
		err := db.QueryRow(`PRAGMA journal_mode=WAL`).Scan(&mode)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			// A filesystem that cannot use WAL keeps its journal mode, as the
			// DSN pragma always allowed; only errors are reported.
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// isBusy reports whether err is SQLite's SQLITE_BUSY (any extended code).
func isBusy(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && coded.Code()&0xff == 5
}

// migratedStores holds the database paths this process has fully migrated.
var migratedStores sync.Map

// migrationLocks single-flights first opens per database path within this
// process. Entries are reference counted and removed when idle, so opening
// many distinct paths (tests) does not accumulate state.
var (
	migrationLocksMu sync.Mutex
	migrationLocks   = map[string]*migrationLock{}
)

type migrationLock struct {
	mu   sync.Mutex
	refs int
}

func lockMigration(path string) (unlock func()) {
	migrationLocksMu.Lock()
	l := migrationLocks[path]
	if l == nil {
		l = &migrationLock{}
		migrationLocks[path] = l
	}
	l.refs++
	migrationLocksMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		migrationLocksMu.Lock()
		if l.refs--; l.refs == 0 {
			delete(migrationLocks, path)
		}
		migrationLocksMu.Unlock()
	}
}

// querier is the subset of *sql.DB, *sql.Conn and *sql.Tx migrations use.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// connQuerier adapts a pinned *sql.Conn to querier.
type connQuerier struct{ c *sql.Conn }

func (q connQuerier) Exec(query string, args ...any) (sql.Result, error) {
	return q.c.ExecContext(context.Background(), query, args...)
}
func (q connQuerier) Query(query string, args ...any) (*sql.Rows, error) {
	return q.c.QueryContext(context.Background(), query, args...)
}
func (q connQuerier) QueryRow(query string, args ...any) *sql.Row {
	return q.c.QueryRowContext(context.Background(), query, args...)
}

// writeTx runs fn inside BEGIN IMMEDIATE on one pinned connection and commits
// if fn succeeds. IMMEDIATE takes SQLite's write reservation before fn reads
// anything, waiting out other writers (in this or another process, e.g. the
// daemon and a CLI) via the busy timeout. So fn's check-then-change steps see
// the latest committed state and cannot interleave with another migrator, and
// the transaction can never fail with SQLITE_BUSY_SNAPSHOT by upgrading a stale
// read snapshot. On any failure everything fn did is rolled back; if the
// rollback itself fails the connection is discarded rather than returned to
// the pool with a transaction open.
func (d *DB) writeTx(fn func(querier) error) (err error) {
	ctx := context.Background()
	conn, err := d.sql.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	q := connQuerier{conn}
	if err = fn(q); err == nil {
		if _, err = conn.ExecContext(ctx, `COMMIT`); err == nil {
			return nil
		}
	}
	if _, rbErr := conn.ExecContext(ctx, `ROLLBACK`); rbErr != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	return err
}

// OpenReadOnly opens the already-migrated store without creating directories,
// importing legacy data, running schema migration, or performing backfills.
// Authorization and restricted projections use this path so reads can never
// initialize or widen a coordinator's repository ceiling.
func OpenReadOnly() (*DB, error) {
	dsn := "file:" + core.DBPath() + "?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)&_pragma=foreign_keys(on)"
	sqldb, shared, err := openHandle(dsn)
	if err != nil {
		return nil, err
	}
	d := &DB{sql: sqldb, shared: shared}
	version, err := d.SchemaVersion()
	if err != nil {
		_ = d.Close()
		return nil, err
	}
	if version != CurrentSchemaVersion {
		_ = d.Close()
		return nil, &SchemaVersionError{Have: version, Min: CurrentSchemaVersion, Max: CurrentSchemaVersion}
	}
	return d, nil
}

func (d *DB) Close() error {
	if d.shared {
		return nil
	}
	return d.sql.Close()
}

var (
	shareHandles  atomic.Bool
	sharedMu      sync.Mutex
	sharedHandles = map[string]*sql.DB{}
)

// ShareHandles makes this process reuse one connection pool per database DSN
// instead of opening (and re-parsing the schema of) a fresh SQLite connection
// on every Open/OpenReadOnly. The daemon opens the store on every reconcile
// tick and several times per session report, and connection setup dominated
// that cost. Short-lived CLI processes keep per-call handles. Turning sharing
// off closes the pooled handles (tests and benchmarks).
func ShareHandles(on bool) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	shareHandles.Store(on)
	if !on {
		for dsn, db := range sharedHandles {
			_ = db.Close()
			delete(sharedHandles, dsn)
		}
	}
}

func openHandle(dsn string) (*sql.DB, bool, error) {
	if !shareHandles.Load() {
		db, err := sql.Open("sqlite", dsn)
		return db, false, err
	}
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if db, ok := sharedHandles[dsn]; ok {
		return db, true, nil
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, false, err
	}
	db.SetMaxIdleConns(4)
	sharedHandles[dsn] = db
	return db, true, nil
}

// migrate applies the schema in one write transaction: the version check,
// every idempotent CREATE/ADD COLUMN and the version bump commit together or
// not at all, serialized against other migrators in any process.
func (d *DB) migrate() error {
	return d.writeTx(migrateSchema)
}

func migrateSchema(q querier) error {
	var version int
	if err := q.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version < MinSchemaVersion || version > CurrentSchemaVersion {
		return &SchemaVersionError{Have: version, Min: MinSchemaVersion, Max: CurrentSchemaVersion}
	}
	if _, err := q.Exec(`
CREATE TABLE IF NOT EXISTS repos (
  name    TEXT PRIMARY KEY,
  source  TEXT NOT NULL,
  git_dir TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  id        TEXT PRIMARY KEY,
  root_id   TEXT NOT NULL DEFAULT '',
  name      TEXT NOT NULL DEFAULT '',
  agent     TEXT NOT NULL DEFAULT 'claude',
  model     TEXT NOT NULL DEFAULT '',
  mode      TEXT NOT NULL DEFAULT 'task',
  repo      TEXT NOT NULL DEFAULT '',
  branch    TEXT NOT NULL DEFAULT '',
  dir       TEXT NOT NULL DEFAULT '',
  claude_id TEXT NOT NULL DEFAULT '',
  prompt    TEXT NOT NULL DEFAULT '',
  created     INTEGER NOT NULL DEFAULT 0,
  scope       TEXT NOT NULL DEFAULT 'work',
  archived    INTEGER NOT NULL DEFAULT 0,
  archived_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sessions_root ON sessions(root_id);
CREATE TABLE IF NOT EXISTS coordinator_grant_state (
  coordinator_id TEXT PRIMARY KEY,
  initialized    INTEGER NOT NULL CHECK(initialized = 1)
);
CREATE TABLE IF NOT EXISTS coordinator_repo_grants (
  coordinator_id TEXT NOT NULL,
  repo           TEXT NOT NULL,
  PRIMARY KEY(coordinator_id, repo)
);
CREATE TABLE IF NOT EXISTS coordinator_grant_migrations (
  version INTEGER PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS legacy_registry_import (
  version INTEGER PRIMARY KEY
);
CREATE TABLE IF NOT EXISTS session_prs (
  session_id TEXT PRIMARY KEY,
  state      TEXT NOT NULL DEFAULT '',
  updated    INTEGER NOT NULL DEFAULT 0
);
`); err != nil {
		return err
	}
	// Idempotently add columns introduced after the initial schema, so existing
	// DBs gain them (CREATE TABLE IF NOT EXISTS won't alter an existing table).
	if err := addColumn(q, "sessions", "scope", "TEXT NOT NULL DEFAULT 'work'"); err != nil {
		return err
	}
	if err := addColumn(q, "sessions", "archived", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := addColumn(q, "sessions", "archived_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// all_repos marks a coordinator whose ceiling is the live set of tracked
	// repositories rather than a fixed list (see CoordinatorRepoGrants).
	if err := addColumn(q, "coordinator_grant_state", "all_repos", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Session incarnations (identity.go): column, index and backfill commit
	// with the rest of the schema.
	if err := migrateIncarnations(q); err != nil {
		return err
	}
	// Covers SessionHeads, so the daemon's per-tick scan reads only this index
	// instead of every row's overflow pages (archived follows the prompt).
	if _, err := q.Exec(`CREATE INDEX IF NOT EXISTS idx_sessions_heads ON sessions(created, id, root_id, archived)`); err != nil {
		return err
	}
	// Version 3 includes the wrapper promotion below. Do not advertise it
	// until the data migration has committed successfully.
	if version < 2 {
		_, err := q.Exec(`PRAGMA user_version = 2`)
		return err
	}
	return nil
}

// SchemaVersion returns SQLite's persistent user_version marker. It is zero for
// databases created by amux before schema versioning was introduced.
func (d *DB) SchemaVersion() (int, error) {
	var version int
	err := d.sql.QueryRow(`PRAGMA user_version`).Scan(&version)
	return version, err
}

// addColumn adds col to table if it isn't already present.
func addColumn(q querier, table, col, def string) error {
	rows, err := q.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == col {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = q.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + def)
	return err
}

// ---- repos ---------------------------------------------------------------

func (d *DB) PutRepo(r Repo) error { return putRepoRow(d.sql, r) }

func putRepoRow(q querier, r Repo) error {
	_, err := q.Exec(
		`INSERT INTO repos(name,source,git_dir) VALUES(?,?,?)
		 ON CONFLICT(name) DO UPDATE SET source=excluded.source, git_dir=excluded.git_dir`,
		r.Name, r.Source, r.GitDir)
	return err
}

func (d *DB) Repos() ([]Repo, error) {
	rows, err := d.sql.Query(`SELECT name,source,git_dir FROM repos ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		var r Repo
		if err := rows.Scan(&r.Name, &r.Source, &r.GitDir); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) Repo(name string) (Repo, bool, error) {
	var r Repo
	err := d.sql.QueryRow(`SELECT name,source,git_dir FROM repos WHERE name=?`, name).
		Scan(&r.Name, &r.Source, &r.GitDir)
	if err == sql.ErrNoRows {
		return Repo{}, false, nil
	}
	return r, err == nil, err
}

func (d *DB) DeleteRepo(name string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM coordinator_repo_grants WHERE repo=?`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM repos WHERE name=?`, name); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- sessions ------------------------------------------------------------

func (d *DB) PutSession(s Session) error {
	_, exists, err := d.GetSession(s.ID)
	if err != nil {
		return err
	}
	bind := accounts.Bind
	if exists {
		bind = accounts.BindExisting
	}
	if err := bind(s.ID, s.Agent); err != nil {
		return err
	}
	return putSessionRow(d.sql, s)
}

// putSessionRow upserts the whole row; callers bind the session's account. A
// new row gets a fresh incarnation; an existing row keeps its own (the update
// never names the column).
func putSessionRow(q querier, s Session) error {
	_, err := q.Exec(
		`INSERT INTO sessions(id,root_id,name,agent,model,mode,repo,branch,dir,claude_id,prompt,created,scope,archived,archived_at,incarnation)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   root_id=excluded.root_id, name=excluded.name, agent=excluded.agent,
		   model=excluded.model, mode=excluded.mode, repo=excluded.repo,
		   branch=excluded.branch, dir=excluded.dir, claude_id=excluded.claude_id,
		   prompt=excluded.prompt, created=excluded.created, scope=excluded.scope,
		   archived=excluded.archived, archived_at=excluded.archived_at`,
		s.ID, s.RootID, s.Name, s.Agent, s.Model, s.Mode, s.Repo, s.Branch, s.Dir, s.ClaudeID, s.Prompt, s.Created, s.Scope, b2i(s.Archived), s.ArchivedAt, newIncarnation())
	return err
}

// Field-scoped session updaters. Each writes a single logical field with one
// UPDATE statement, so it touches only the column(s) named — not the whole row
// like PutSession's upsert. That closes a lost-update race: a launch adopting a
// codex id (SetClaudeID) concurrent with a rename (SetName) or archive
// (SetArchivedFlag) from the TUI no longer last-write-wins the entire row and
// silently reverts the other change. Each is a single autocommit statement, so
// it's atomic; a missing id is a no-op (zero rows affected), never an error —
// callers that need a not-found error check existence with GetSession first.

// SetName updates a session's display name.
func (d *DB) SetName(id, name string) error {
	_, err := d.sql.Exec(`UPDATE sessions SET name=? WHERE id=?`, name, id)
	return err
}

// SetModel updates the model amux will display and pass on the next launch. The
// runtime-observation path uses this after an in-session model switch.
func (d *DB) SetModel(id, model string) error {
	_, err := d.sql.Exec(`UPDATE sessions SET model=? WHERE id=?`, model, id)
	return err
}

// SetClaudeID updates a session's pinned conversation id (Claude's --session-id
// or Codex's adopted rollout uuid).
func (d *DB) SetClaudeID(id, claudeID string) error {
	_, err := d.sql.Exec(`UPDATE sessions SET claude_id=? WHERE id=?`, claudeID, id)
	return err
}

// SetArchivedFlag updates a session's archived flag and its archive timestamp
// together (one statement, so the pair can't tear).
func (d *DB) SetArchivedFlag(id string, archived bool, archivedAt int64) error {
	_, err := d.sql.Exec(`UPDATE sessions SET archived=?, archived_at=? WHERE id=?`, b2i(archived), archivedAt, id)
	return err
}

// SetDir updates a session's sandbox dir (filled in for a workgroup root that
// predates default sessions, the first time it is resolved as a coordinator).
func (d *DB) SetDir(id, dir string) error {
	_, err := d.sql.Exec(`UPDATE sessions SET dir=? WHERE id=?`, dir, id)
	return err
}

// SetRepoScope updates a session's repo list (its comma-joined worktree scope).
func (d *DB) SetRepoScope(id, repo string) error {
	_, err := d.sql.Exec(`UPDATE sessions SET repo=? WHERE id=?`, repo, id)
	return err
}

// SetRootID updates a session's parent workgroup (used when re-parenting an agent).
func (d *DB) SetRootID(id, rootID string) error {
	_, err := d.sql.Exec(`UPDATE sessions SET root_id=? WHERE id=?`, rootID, id)
	return err
}

func scanSessions(rows *sql.Rows) ([]Session, error) {
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		var archived int64
		if err := rows.Scan(&s.ID, &s.RootID, &s.Name, &s.Agent, &s.Model, &s.Mode,
			&s.Repo, &s.Branch, &s.Dir, &s.ClaudeID, &s.Prompt, &s.Created, &s.Scope, &archived, &s.ArchivedAt); err != nil {
			return nil, err
		}
		s.Archived = archived != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

const sessionCols = `id,root_id,name,agent,model,mode,repo,branch,dir,claude_id,prompt,created,scope,archived,archived_at`

func (d *DB) GetSession(id string) (Session, bool, error) {
	var s Session
	var archived int64
	err := d.sql.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE id=?`, id).
		Scan(&s.ID, &s.RootID, &s.Name, &s.Agent, &s.Model, &s.Mode,
			&s.Repo, &s.Branch, &s.Dir, &s.ClaudeID, &s.Prompt, &s.Created, &s.Scope, &archived, &s.ArchivedAt)
	if err == sql.ErrNoRows {
		return Session{}, false, nil
	}
	s.Archived = archived != 0
	return s, err == nil, err
}

// b2i maps a bool to the 0/1 integer SQLite stores.
func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// AllSessions returns every session (roots and subs), roots before their subs.
func (d *DB) AllSessions() ([]Session, error) {
	rows, err := d.sql.Query(`SELECT ` + sessionCols + ` FROM sessions ORDER BY created`)
	if err != nil {
		return nil, err
	}
	return scanSessions(rows)
}

// SessionHeads returns every session's ID, RootID and Archived flag, in
// AllSessions' order, leaving every other field zero. The daemon's reconcile
// loop scans the whole table every tick and needs only these; selecting full
// rows (prompts included) through the ORDER BY sorter dominated its cost.
func (d *DB) SessionHeads() ([]Session, error) {
	rows, err := d.sql.Query(`SELECT id,root_id,archived FROM sessions ORDER BY created`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		var archived int64
		if err := rows.Scan(&s.ID, &s.RootID, &archived); err != nil {
			return nil, err
		}
		s.Archived = archived != 0
		out = append(out, s)
	}
	return out, rows.Err()
}

// Roots returns root containers ordered by creation.
func (d *DB) Roots() ([]Session, error) {
	rows, err := d.sql.Query(`SELECT ` + sessionCols + ` FROM sessions WHERE root_id='' ORDER BY created`)
	if err != nil {
		return nil, err
	}
	return scanSessions(rows)
}

// Children returns the sub-sessions of a root, ordered by creation.
func (d *DB) Children(rootID string) ([]Session, error) {
	rows, err := d.sql.Query(`SELECT `+sessionCols+` FROM sessions WHERE root_id=? ORDER BY created`, rootID)
	if err != nil {
		return nil, err
	}
	return scanSessions(rows)
}

// DeleteSession removes a single session row.
func (d *DB) DeleteSession(id string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM coordinator_repo_grants WHERE coordinator_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM coordinator_grant_state WHERE coordinator_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM session_prs WHERE session_id=?`, id); err != nil && !missingTable(err) {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// NewID returns a short unique session id.
func (d *DB) NewID() string {
	for {
		var b [3]byte
		_, _ = rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		if _, ok, _ := d.GetSession(id); !ok {
			return id
		}
	}
}

// newIDIn is NewID inside a transaction, so ids written earlier in it count.
func newIDIn(q querier) (string, error) {
	for {
		var b [3]byte
		_, _ = rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		var one int
		switch err := q.QueryRow(`SELECT 1 FROM sessions WHERE id=?`, id).Scan(&one); err {
		case sql.ErrNoRows:
			return id, nil
		case nil:
		default:
			return "", err
		}
	}
}

// NewUUID returns a random RFC-4122 v4 UUID (for pinning agent session ids).
func NewUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Slug normalizes an arbitrary string into a filesystem/branch-safe token.
func Slug(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
