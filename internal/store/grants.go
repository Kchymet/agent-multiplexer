package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

const coordinatorGrantMigrationVersion = 1

// CoordinatorRepoGrants returns the persistent repository ceiling and whether
// it was deliberately initialized. An initialized empty result is distinct
// from a legacy/unknown coordinator with no grant decision.
func (d *DB) CoordinatorRepoGrants(coordinatorID string) ([]string, bool, error) {
	var initialized int
	err := d.sql.QueryRow(`SELECT initialized FROM coordinator_grant_state WHERE coordinator_id=?`, coordinatorID).Scan(&initialized)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rows, err := d.sql.Query(`SELECT repo FROM coordinator_repo_grants WHERE coordinator_id=? ORDER BY repo`, coordinatorID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var repos []string
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			return nil, false, err
		}
		repos = append(repos, repo)
	}
	return repos, initialized == 1, rows.Err()
}

// SetCoordinatorRepoGrants is the explicit host initialization/update path.
// It validates the target role and every tracked repository, then replaces the
// ceiling and its initialized marker in one transaction. Passing no repos
// deliberately persists an empty ceiling.
func (d *DB) SetCoordinatorRepoGrants(coordinatorID string, repos []string) error {
	repos = normalizedGrantRepos(repos)
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var rootID, scope string
	if err := tx.QueryRow(`SELECT root_id,scope FROM sessions WHERE id=?`, coordinatorID).Scan(&rootID, &scope); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("coordinator %q not found", coordinatorID)
		}
		return err
	}
	if rootID != "" || scope == ScopeRepo {
		return fmt.Errorf("session %q is not a coordinator", coordinatorID)
	}
	for _, repo := range repos {
		var exists int
		if err := tx.QueryRow(`SELECT 1 FROM repos WHERE name=?`, repo).Scan(&exists); err != nil {
			if err == sql.ErrNoRows {
				return fmt.Errorf("unknown repo %q", repo)
			}
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM coordinator_repo_grants WHERE coordinator_id=?`, coordinatorID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO coordinator_grant_state(coordinator_id,initialized) VALUES(?,1)
		ON CONFLICT(coordinator_id) DO UPDATE SET initialized=1`, coordinatorID); err != nil {
		return err
	}
	for _, repo := range repos {
		if _, err := tx.Exec(`INSERT INTO coordinator_repo_grants(coordinator_id,repo) VALUES(?,?)`, coordinatorID, repo); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// migrateCoordinatorGrants is a one-time, explicit legacy initialization run
// by writable Open after schema/import work. It preserves nonempty repository
// ceilings already persisted on coordinator rows, but never derives a ceiling
// from child history and leaves blank legacy coordinators uninitialized.
func (d *DB) migrateCoordinatorGrants() error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var marker int
	err = tx.QueryRow(`SELECT version FROM coordinator_grant_migrations WHERE version=?`, coordinatorGrantMigrationVersion).Scan(&marker)
	if err == nil {
		return tx.Commit()
	}
	if err != sql.ErrNoRows {
		return err
	}
	rows, err := tx.Query(`SELECT id,repo FROM sessions
		WHERE root_id='' AND scope!='repo' AND trim(repo)!='' ORDER BY id`)
	if err != nil {
		return err
	}
	type legacyGrant struct {
		id    string
		repos []string
	}
	var legacy []legacyGrant
	for rows.Next() {
		var id, value string
		if err := rows.Scan(&id, &value); err != nil {
			_ = rows.Close()
			return err
		}
		legacy = append(legacy, legacyGrant{id: id, repos: normalizedGrantRepos(SplitRepos(value))})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, grant := range legacy {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO coordinator_grant_state(coordinator_id,initialized) VALUES(?,1)`, grant.id); err != nil {
			return err
		}
		for _, repo := range grant.repos {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO coordinator_repo_grants(coordinator_id,repo) VALUES(?,?)`, grant.id, repo); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO coordinator_grant_migrations(version) VALUES(?)`, coordinatorGrantMigrationVersion); err != nil {
		return err
	}
	return tx.Commit()
}

func normalizedGrantRepos(repos []string) []string {
	seen := make(map[string]struct{}, len(repos))
	out := make([]string, 0, len(repos))
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		if _, ok := seen[repo]; ok {
			continue
		}
		seen[repo] = struct{}{}
		out = append(out, repo)
	}
	sort.Strings(out)
	return out
}
