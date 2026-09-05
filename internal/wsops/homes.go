package wsops

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"amux/internal/agent"
	"amux/internal/cfghome"
	"amux/internal/console"
	"amux/internal/store"
)

// Default sessions: every container the rail shows hosts one long-lived agent
// scoped to it — the machine-wide console, a workgroup root's coordinator, and
// a tracked repo's home (store.Role*). This file is where those sessions are
// resolved and materialized, so the daemon's pane, steer, and transcript
// lookups, the rail, and the CLI all see the same inventory:
//
//   - The console is synthetic (console.Session; never a store row).
//   - A work-scoped root IS its coordinator: the root row gains a sandbox dir
//     (its container dir, which already holds every member's sandbox) and a
//     pinned conversation id. Roots created before default sessions get both
//     filled in the first time they are resolved.
//   - A repo home is a repo-scoped root whose id is the repo name
//     (store.RepoHomeID). It is created when a repo is tracked, and on first
//     resolve for repos tracked before default sessions.
//
// A hidden single-member repo root (the wrapper around a one-off agent) hosts
// nothing and resolves as a bare store row, as before.

// ResolveSession resolves an id the way every daemon-side lookup must: the
// console, a store row (agent, coordinator root, or repo home), or a tracked
// repo whose home session doesn't exist yet. A coordinator or home that is
// missing its sandbox dir or conversation id gets them now, persisted, so the
// launch that follows resumes durably across restarts. ok=false means no such
// session or repo.
func ResolveSession(id string) (store.Session, bool, error) {
	if id == console.ID {
		if err := console.Ensure(); err != nil {
			return store.Session{}, false, err
		}
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
			return ensureContainerHome(db, s)
		}
		return s, true, nil
	}
	// Not a session: a tracked repo's home that predates default sessions?
	if r, ok, _ := db.Repo(id); ok {
		home, err := ensureRepoHome(db, r.Name)
		return home, err == nil, err
	}
	return store.Session{}, false, nil
}

// ensureContainerHome fills in a coordinator root's sandbox dir and pinned
// conversation id when either is missing (a root created before default
// sessions), writing them back field by field so a concurrent rename or
// archive of the same row is never reverted.
func ensureContainerHome(db *store.DB, s store.Session) (store.Session, bool, error) {
	// A container session's sandbox is always its container dir. A root imported
	// from the legacy registry recorded the workspace's combined worktree dir
	// instead, and a guide or config home written there would dirty a checkout.
	if want := store.RootDir(s.ID); s.Dir != want {
		s.Dir = want
		if err := db.SetDir(s.ID, s.Dir); err != nil {
			return store.Session{}, false, err
		}
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return store.Session{}, false, err
	}
	if s.ClaudeID == "" {
		if id := agent.HarnessFor(s.Agent).NewSessionID(); id != "" {
			s.ClaudeID = id
			if err := db.SetClaudeID(s.ID, id); err != nil {
				return store.Session{}, false, err
			}
		}
	}
	return s, true, nil
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
		return ensureRepoHomeDir(db, s)
	}
	kind := agent.DefaultKind()
	s := store.Session{
		ID: id, RootID: "", Scope: store.ScopeRepo, Repo: repo,
		Agent: kind, Mode: store.ModeInteractive,
		Dir: store.RootDir(id), ClaudeID: agent.HarnessFor(kind).NewSessionID(),
		Created: store.Now(),
	}
	if err := db.PutSession(s); err != nil {
		return store.Session{}, err
	}
	return ensureRepoHomeDir(db, s)
}

func ensureRepoHomeDir(db *store.DB, s store.Session) (store.Session, error) {
	s, _, err := ensureContainerHome(db, s)
	if err != nil {
		return store.Session{}, err
	}
	return s, nil
}

// EnsureRepoHome creates (or returns) the home session of a tracked repo. It is
// called when a repo is tracked so the home exists from the start; ResolveSession
// creates it lazily for repos tracked before default sessions.
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

// removeContainerFiles clears a coordinator's own files from a workgroup's
// container dir when the workgroup is deleted, without touching any agent
// sandbox that still lives under it (an agent moved out of this workgroup keeps
// its dir here — see MoveAgent). Only the container's own entries — the guide,
// the private config home, the installed skills — are known; anything that is
// some session's sandbox is kept, and the dir itself is removed only if that
// leaves it empty.
func removeContainerFiles(db *store.DB, s store.Session) {
	if spec, ok := agent.HarnessFor(s.Agent).Config(s); ok {
		cfghome.Forget(spec)
	}
	dir := s.Dir
	if dir == "" {
		dir = store.RootDir(s.ID)
	}
	keep := map[string]bool{}
	if all, err := db.AllSessions(); err == nil {
		for _, o := range all {
			if o.ID != s.ID && o.Dir != "" {
				keep[filepath.Clean(o.Dir)] = true
			}
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() && underAny(p, keep) {
			continue
		}
		_ = os.RemoveAll(p)
	}
	_ = os.Remove(dir) // non-recursive: only if nothing else lives here
}

// underAny reports whether p is, or contains, a kept sandbox path.
func underAny(p string, keep map[string]bool) bool {
	p = filepath.Clean(p)
	for k := range keep {
		if k == p || strings.HasPrefix(k, p+string(filepath.Separator)) {
			return true
		}
	}
	return false
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
