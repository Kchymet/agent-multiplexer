// Package store is the SQLite-backed registry of tracked repositories and
// sessions. Sessions form a one-level hierarchy: a root session (RootID == "")
// is a container; its sub-sessions (RootID == root's id) each bind one agent to
// one worktree. Each process opens the DB independently; WAL + a busy timeout
// handle the daemon polling while the CLI writes.
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

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
	Agent    string // claude | hermes
	Model    string // optional per-session model override
	Mode     string // task | loop
	Repo     string // repo name (sub-sessions); may list several for migrated roots
	Branch   string // git branch (sub-sessions)
	Dir      string // worktree dir (sub-sessions) / container dir (roots)
	ClaudeID string // pinned claude conversation id (resume across restarts)
	Prompt   string // initial prompt
	Created  int64
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
type DB struct{ sql *sql.DB }

// Open opens (creating if needed) the store, applies the schema, and imports any
// legacy JSON registry once.
func Open() (*DB, error) {
	if err := os.MkdirAll(core.DataDir(), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + core.DBPath() + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)"
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	d := &DB{sql: sqldb}
	if err := d.migrate(); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	if err := d.importLegacy(); err != nil {
		// Non-fatal: a failed import shouldn't block usage.
		_ = err
	}
	_ = d.BackfillWorkspaceRepos()
	return d, nil
}

func (d *DB) Close() error { return d.sql.Close() }

func (d *DB) migrate() error {
	_, err := d.sql.Exec(`
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
  created   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_sessions_root ON sessions(root_id);
`)
	return err
}

// ---- repos ---------------------------------------------------------------

func (d *DB) PutRepo(r Repo) error {
	_, err := d.sql.Exec(
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
	_, err := d.sql.Exec(`DELETE FROM repos WHERE name=?`, name)
	return err
}

// ---- sessions ------------------------------------------------------------

func (d *DB) PutSession(s Session) error {
	_, err := d.sql.Exec(
		`INSERT INTO sessions(id,root_id,name,agent,model,mode,repo,branch,dir,claude_id,prompt,created)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   root_id=excluded.root_id, name=excluded.name, agent=excluded.agent,
		   model=excluded.model, mode=excluded.mode, repo=excluded.repo,
		   branch=excluded.branch, dir=excluded.dir, claude_id=excluded.claude_id,
		   prompt=excluded.prompt, created=excluded.created`,
		s.ID, s.RootID, s.Name, s.Agent, s.Model, s.Mode, s.Repo, s.Branch, s.Dir, s.ClaudeID, s.Prompt, s.Created)
	return err
}

func scanSessions(rows *sql.Rows) ([]Session, error) {
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.RootID, &s.Name, &s.Agent, &s.Model, &s.Mode,
			&s.Repo, &s.Branch, &s.Dir, &s.ClaudeID, &s.Prompt, &s.Created); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

const sessionCols = `id,root_id,name,agent,model,mode,repo,branch,dir,claude_id,prompt,created`

func (d *DB) GetSession(id string) (Session, bool, error) {
	var s Session
	err := d.sql.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE id=?`, id).
		Scan(&s.ID, &s.RootID, &s.Name, &s.Agent, &s.Model, &s.Mode,
			&s.Repo, &s.Branch, &s.Dir, &s.ClaudeID, &s.Prompt, &s.Created)
	if err == sql.ErrNoRows {
		return Session{}, false, nil
	}
	return s, err == nil, err
}

// AllSessions returns every session (roots and subs), roots before their subs.
func (d *DB) AllSessions() ([]Session, error) {
	rows, err := d.sql.Query(`SELECT ` + sessionCols + ` FROM sessions ORDER BY created`)
	if err != nil {
		return nil, err
	}
	return scanSessions(rows)
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
	_, err := d.sql.Exec(`DELETE FROM sessions WHERE id=?`, id)
	return err
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
