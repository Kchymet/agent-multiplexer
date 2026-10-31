package store

import (
	"database/sql"
	"strings"
)

// Session PR state is the PR watcher's last observation of the pull requests
// each session owns (internal/prwatch). The store keeps it as an opaque JSON
// document per session so the watcher can compare a fresh observation with the
// last one across daemon restarts — the basis of its transition detection and
// wake de-duplication. The table is additive (CREATE TABLE IF NOT EXISTS in
// migrate) and needs no schema version bump: a binary that predates it ignores
// it, and a read-only handle on a database no writer has migrated yet reads it
// as empty.

// SessionPRStates returns every session's stored PR state document, keyed by
// session id.
func (d *DB) SessionPRStates() (map[string]string, error) {
	rows, err := d.sql.Query(`SELECT session_id,state FROM session_prs`)
	if err != nil {
		if missingTable(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			return nil, err
		}
		out[id] = state
	}
	return out, rows.Err()
}

// PutSessionPRState stores a session's PR state document; an empty document
// deletes the row.
func (d *DB) PutSessionPRState(id, state string, updated int64) error {
	if state == "" {
		_, err := d.sql.Exec(`DELETE FROM session_prs WHERE session_id=?`, id)
		return err
	}
	_, err := d.sql.Exec(`INSERT INTO session_prs(session_id,state,updated) VALUES(?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET state=excluded.state, updated=excluded.updated`, id, state, updated)
	return err
}

func missingTable(err error) bool {
	return err != nil && err != sql.ErrNoRows && strings.Contains(err.Error(), "no such table")
}
