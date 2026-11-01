package store

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

const (
	coordinatorGrantMigrationVersion = 1
	// emptyGrantMigrationVersion converts initialized-but-empty ceilings to
	// all-tracked mode (see migrateEmptyCoordinatorGrants).
	emptyGrantMigrationVersion = 2
)

// CoordinatorRepoGrants returns the persistent repository ceiling and whether
// it was deliberately initialized. An initialized empty result is distinct
// from a legacy/unknown coordinator with no grant decision.
//
// A coordinator in all-tracked mode (CoordinatorGrantsAllTracked) resolves to
// the live set of repositories tracked on this host, so a repo tracked after
// the workgroup was created is immediately assignable and an untracked repo
// immediately is not. Authority never extends past tracked repositories.
func (d *DB) CoordinatorRepoGrants(coordinatorID string) ([]string, bool, error) {
	initialized, all, err := d.coordinatorGrantState(coordinatorID)
	if err != nil || !initialized {
		return nil, false, err
	}
	query := `SELECT repo FROM coordinator_repo_grants WHERE coordinator_id=? ORDER BY repo`
	args := []any{coordinatorID}
	if all {
		query, args = `SELECT name FROM repos ORDER BY name`, nil
	}
	rows, err := d.sql.Query(query, args...)
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
	return repos, true, rows.Err()
}

// CoordinatorGrantsAllTracked reports whether a coordinator's ceiling follows
// every tracked repository rather than a fixed list.
func (d *DB) CoordinatorGrantsAllTracked(coordinatorID string) (bool, error) {
	initialized, all, err := d.coordinatorGrantState(coordinatorID)
	return initialized && all, err
}

func (d *DB) coordinatorGrantState(coordinatorID string) (initialized, all bool, err error) {
	var init, allRepos int
	err = d.sql.QueryRow(`SELECT initialized,all_repos FROM coordinator_grant_state WHERE coordinator_id=?`, coordinatorID).Scan(&init, &allRepos)
	if err != nil && strings.Contains(err.Error(), "no such column") {
		// A read-only handle on a store the writable daemon has not migrated
		// yet: the column's absence means no coordinator is in all-tracked mode.
		allRepos = 0
		err = d.sql.QueryRow(`SELECT initialized FROM coordinator_grant_state WHERE coordinator_id=?`, coordinatorID).Scan(&init)
	}
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return init == 1, allRepos == 1, nil
}

// SetCoordinatorRepoGrants is the explicit host initialization/update path.
// It validates the target role and every tracked repository, then replaces the
// ceiling and its initialized marker in one transaction. Passing no repos
// deliberately persists an empty ceiling.
func (d *DB) SetCoordinatorRepoGrants(coordinatorID string, repos []string) error {
	return d.setCoordinatorGrants(coordinatorID, normalizedGrantRepos(repos), false)
}

// SetCoordinatorRepoGrantsAllTracked initializes (or resets) a coordinator's
// ceiling to every repository tracked on the host, live: repos tracked later
// are included and untracked ones drop out. This is the default for any
// workgroup created without naming repositories.
func (d *DB) SetCoordinatorRepoGrantsAllTracked(coordinatorID string) error {
	return d.setCoordinatorGrants(coordinatorID, nil, true)
}

func (d *DB) setCoordinatorGrants(coordinatorID string, repos []string, all bool) error {
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
	allRepos := 0
	if all {
		allRepos = 1
	}
	if _, err := tx.Exec(`INSERT INTO coordinator_grant_state(coordinator_id,initialized,all_repos) VALUES(?,1,?)
		ON CONFLICT(coordinator_id) DO UPDATE SET initialized=1,all_repos=excluded.all_repos`, coordinatorID, allRepos); err != nil {
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

// migrateEmptyCoordinatorGrants is a one-time repair. Before all-tracked mode
// existed, a workgroup created remotely (the web dashboard's empty-workgroup
// verb, or new-workgroup with an empty repos field) persisted an initialized
// EMPTY ceiling, so its coordinator could neither list nor assign any of the
// host's repositories. Those coordinators move to all-tracked mode. Fixed,
// nonempty ceilings and uninitialized legacy coordinators are left as they are;
// the host can re-seal any workgroup with `amux workgroup grants <id> --none`.
func (d *DB) migrateEmptyCoordinatorGrants() error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var marker int
	err = tx.QueryRow(`SELECT version FROM coordinator_grant_migrations WHERE version=?`, emptyGrantMigrationVersion).Scan(&marker)
	if err == nil {
		return tx.Commit()
	}
	if err != sql.ErrNoRows {
		return err
	}
	if _, err := tx.Exec(`UPDATE coordinator_grant_state SET all_repos=1
		WHERE initialized=1 AND NOT EXISTS (
			SELECT 1 FROM coordinator_repo_grants g WHERE g.coordinator_id=coordinator_grant_state.coordinator_id)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO coordinator_grant_migrations(version) VALUES(?)`, emptyGrantMigrationVersion); err != nil {
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
