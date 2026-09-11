package wsops

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"amux/internal/agent"
	"amux/internal/cfghome"
	"amux/internal/console"
	"amux/internal/git"
	"amux/internal/store"
)

// Default sessions: every container the rail shows hosts one long-lived agent
// scoped to it — the machine-wide console, a workgroup root's coordinator, and
// a tracked repo's home (store.Role*). This file is where those sessions are
// resolved and materialized, so the daemon's pane, steer, and transcript
// lookups, the rail, and the CLI all see the same inventory:
//
//   - The console is synthetic (console.Session; never a store row).
//   - A work-scoped root IS its coordinator. New roots have a dedicated own
//     directory beside (never above) member sandboxes.
//   - A repo home is a repo-scoped root whose id is the repo name
//     (store.RepoHomeID). It is created when a repo is tracked, and on first
//     host-authorized creation for repos tracked before default sessions.
//
// A hidden single-member repo root (the wrapper around a one-off agent) hosts
// nothing and resolves as a bare store row, as before.

// ResolveSession resolves an id the way every daemon-side lookup must: the
// console or an existing store row (agent, coordinator root, or repo home).
// Resolution is read-only for stored sessions. Legacy roots are never silently
// relocated or provisioned while answering a lookup; they fail with explicit
// host-authorized recovery guidance and all existing files remain untouched.
// ok=false means no such session or repo.
func ResolveSession(id string) (store.Session, bool, error) {
	if id == console.ID {
		return console.Session(), true, nil
	}
	db, err := store.Open()
	if err != nil {
		return store.Session{}, false, err
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
	if err != nil {
		return store.Session{}, false, err
	}
	if ok {
		if s.Role() == store.RoleCoordinator {
			if err := validateContainerHome(s, store.CoordinatorDir(s.ID), "workgroup coordinator"); err != nil {
				return store.Session{}, false, err
			}
		}
		if s.Role() == store.RoleRepo {
			if err := validateContainerHome(s, store.RootDir(s.ID), "repo home"); err != nil {
				return store.Session{}, false, err
			}
		}
		return s, true, nil
	}
	// A tracked repo whose home predates explicit provisioning must be repaired by
	// a host-authorized lifecycle operation, never created as a side effect of a
	// query, endpoint lookup, or launch resolution.
	if r, ok, _ := db.Repo(id); ok {
		return store.Session{}, false, fmt.Errorf("repo %q has no provisioned home session; run a host-authorized repo repair before launch", r.Name)
	}
	return store.Session{}, false, nil
}

func validateContainerHome(s store.Session, want, label string) error {
	if s.Dir == "" || filepath.Clean(s.Dir) != filepath.Clean(want) {
		return fmt.Errorf("%s %q uses an unsupported legacy layout at %q; files were left unchanged and host-authorized migration or recreation is required", label, s.ID, s.Dir)
	}
	info, err := os.Lstat(s.Dir)
	if err != nil {
		return fmt.Errorf("%s %q own directory is unavailable: %w", label, s.ID, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s %q own directory is not a real directory: %s", label, s.ID, s.Dir)
	}
	return nil
}

// ensureRepoHome returns the home session of a tracked repo, creating it (the
// root row, its sandbox dir, its private config home) if it doesn't exist yet.
func ensureRepoHome(db *store.DB, repo string) (store.Session, error) {
	id := store.RepoHomeID(repo)
	if s, ok, err := db.GetSession(id); err != nil {
		return store.Session{}, err
	} else if ok {
		if s.Role() != store.RoleRepo {
			return store.Session{}, fmt.Errorf("session id %q is taken by a %s, not repo %s's home", id, describeRole(s), repo)
		}
		if err := validateContainerHome(s, store.RootDir(s.ID), "repo home"); err != nil {
			return store.Session{}, err
		}
		return s, nil
	}
	kind := agent.DefaultKind()
	s := store.Session{
		ID: id, RootID: "", Scope: store.ScopeRepo, Repo: repo,
		Agent: kind, Mode: store.ModeInteractive,
		Dir: store.RootDir(id), ClaudeID: agent.HarnessFor(kind).NewSessionID(),
		Created: store.Now(),
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return store.Session{}, err
	}
	if err := db.PutSession(s); err != nil {
		return store.Session{}, err
	}
	return s, nil
}

// EnsureRepoHome creates (or returns) the home session of a tracked repo. It is
// called when a repo is tracked so the home exists from the start. Repos that
// predate home sessions require an explicit host-authorized repair; reads never
// create one lazily.
func EnsureRepoHome(repo string) (store.Session, error) {
	db, err := store.Open()
	if err != nil {
		return store.Session{}, err
	}
	defer db.Close()
	if _, ok, _ := db.Repo(repo); !ok {
		return store.Session{}, fmt.Errorf("unknown repo %q\n  %s", repo, trackedRepos(db))
	}
	return ensureRepoHome(db, repo)
}

// removeRepoHome deletes a repo's home session — its engine is stopped by the
// daemon first (rm-repo stops the engine) — and its sandbox. The home never
// holds a member's sandbox (an agent can't be moved into a repo-scoped root),
// so the whole dir goes; a child row would be a bug, and is left alone with its
// files rather than deleted along with them.
func removeRepoHome(db *store.DB, repo string) {
	id := store.RepoHomeID(repo)
	s, ok, _ := db.GetSession(id)
	if !ok || s.Role() != store.RoleRepo {
		return
	}
	if kids, _ := db.Children(id); len(kids) > 0 {
		log.Printf("amux: repo %s's home has %d child sessions; leaving its dir on disk", repo, len(kids))
		_ = db.DeleteSession(id)
		return
	}
	if spec, ok := agent.HarnessFor(s.Agent).Config(s); ok {
		cfghome.Forget(spec)
	}
	if s.Dir != "" {
		_ = os.RemoveAll(s.Dir)
	}
	_ = db.DeleteSession(id)
}

// removeContainerFiles removes only the coordinator's dedicated own directory.
// It never scans the member-containing workgroup parent. Legacy shared roots
// fail validation before deletion, preserving unknown and moved-session data.
func removeContainerFiles(_ *store.DB, s store.Session) error {
	if err := validateContainerHome(s, store.CoordinatorDir(s.ID), "workgroup coordinator"); err != nil {
		return err
	}
	managedRoot, err := sessionStorageRoot(s.Dir)
	if err != nil {
		return err
	}
	if err := git.RemoveManagedTree(s.Dir, managedRoot, gitStagingDir()); err != nil {
		return fmt.Errorf("remove coordinator directory %s: %w", s.ID, err)
	}
	if spec, ok := agent.HarnessFor(s.Agent).Config(s); ok {
		cfghome.Forget(spec)
	}
	return nil
}

func describeRole(s store.Session) string {
	switch s.Role() {
	case store.RoleCoordinator:
		return "workgroup"
	case store.RoleRepo:
		return "repo home"
	default:
		if s.IsRoot() {
			return "workgroup"
		}
		return "agent"
	}
}

// ScopeOf is the AMUX_SCOPE a session runs under: "global" for the console,
// its workgroup's scope (work | repo) for a member agent, "work" for a
// coordinator, "repo" for a repo home.
func ScopeOf(s store.Session) string {
	switch s.Role() {
	case store.RoleConsole:
		return "global"
	case store.RoleCoordinator:
		return store.ScopeWork
	case store.RoleRepo:
		return store.ScopeRepo
	}
	if s.IsRoot() {
		return s.Scope
	}
	return agentScope(s.RootID)
}
