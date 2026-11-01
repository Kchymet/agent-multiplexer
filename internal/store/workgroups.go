package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"amux/internal/core"
	"amux/internal/hostprep"
)

// migrateRepoWorkgroups promotes legacy wrappers in place. Members, their
// runtime identities, and their physical sandboxes are never rewritten. Repo
// homes remain management sessions. Explicit grants are initialized here even
// on databases where the older coordinator-grant migration has already run.
func (d *DB) migrateRepoWorkgroups() error {
	// Normal opens must remain read-only here. Besides avoiding needless WAL
	// writes, this prevents concurrent opens from upgrading stale read snapshots
	// into writers (SQLITE_BUSY_SNAPSHOT).
	var needed bool
	if err := d.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM sessions WHERE root_id='' AND scope='repo' AND id!=repo)
		OR (SELECT user_version FROM pragma_user_version)!=?`, CurrentSchemaVersion).Scan(&needed); err != nil {
		return err
	}
	if !needed {
		return nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Acquire the writer reservation before reading. A second startup waits for
	// this migration and then reads the committed state instead of a stale one.
	if _, err := tx.Exec(`UPDATE sessions SET scope=scope WHERE 0`); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT id,repo,agent,claude_id FROM sessions WHERE root_id='' AND scope='repo' AND id!=repo`)
	if err != nil {
		return err
	}
	type legacy struct{ id, repo, agent, conversation string }
	var roots []legacy
	for rows.Next() {
		var r legacy
		if err := rows.Scan(&r.id, &r.repo, &r.agent, &r.conversation); err != nil {
			rows.Close()
			return err
		}
		roots = append(roots, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, r := range roots {
		if r.id == "" || r.id == "." || r.id == ".." || filepath.Base(r.id) != r.id {
			return fmt.Errorf("invalid legacy workgroup id %q", r.id)
		}
		// Never mount the old container itself: it may contain current or moved
		// members. Refuse aliases rather than provisioning through a symlink.
		dir := CoordinatorDir(r.id)
		if err := os.MkdirAll(core.SessionsDir(), 0o755); err != nil {
			return err
		}
		container, err := hostprep.OpenSession(core.SessionsDir())
		if err != nil {
			return err
		}
		own, err := container.Sub(filepath.Join(r.id, "coordinator"), true, 0o755)
		container.Close()
		if err != nil {
			return err
		}
		own.Close()
		// Old wrappers were bound to the historical default Claude account.
		// Keep that binding/runtime rather than silently switching accounts.
		if r.agent == "" {
			r.agent = "claude"
		}
		if r.agent == "claude" && r.conversation == "" {
			r.conversation = NewUUID()
		}
		if _, err := tx.Exec(`UPDATE sessions SET scope='work',dir=?,agent=?,claude_id=? WHERE id=?`, dir, r.agent, r.conversation, r.id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM coordinator_repo_grants WHERE coordinator_id=?`, r.id); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO coordinator_grant_state(coordinator_id,initialized) VALUES(?,1)`, r.id); err != nil {
			return err
		}
		// Removed repositories grant nothing; history remains available. Never
		// derive a wider ceiling from a member's mutable repository list.
		for _, repo := range normalizedGrantRepos(SplitRepos(r.repo)) {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO coordinator_repo_grants(coordinator_id,repo) SELECT ?,name FROM repos WHERE name=?`, r.id, strings.TrimSpace(repo)); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, CurrentSchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// CheckCoordinatorRepos is the shared execution-time ceiling for membership
// creation, repository changes, and moves. Host grant changes are a separate,
// explicit operation; moving a member is never itself a grant.
func (d *DB) CheckCoordinatorRepos(id string, repos []string) error {
	root, ok, err := d.GetSession(id)
	if err != nil {
		return err
	}
	if !ok || root.Role() != RoleCoordinator || root.Archived {
		return fmt.Errorf("%q is not an active workgroup", id)
	}
	grants, initialized, err := d.CoordinatorRepoGrants(id)
	if err != nil {
		return err
	}
	if !initialized {
		return fmt.Errorf("coordinator %q has no initialized repository grants", id)
	}
	allowed := make(map[string]bool, len(grants))
	for _, repo := range grants {
		allowed[repo] = true
	}
	for _, repo := range normalizedGrantRepos(repos) {
		if !allowed[repo] {
			return fmt.Errorf("repository %q is outside coordinator %q grants (or is not tracked); the host can widen them with `amux workgroup grants %s --all` (or `... %s <repo>...`)", repo, id, id, id)
		}
	}
	return nil
}
