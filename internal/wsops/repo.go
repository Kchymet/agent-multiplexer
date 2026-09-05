package wsops

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"amux/internal/core"
	"amux/internal/gh"
	"amux/internal/git"
	"amux/internal/store"
)

// AddRepoSource tracks a repository from a single source string — a GitHub
// owner/name, a git URL, or a local path — by cloning it bare into the amux
// repos dir and registering it in the store. It is the non-interactive core
// shared by the CLI's `repo add <src>` and the native TUI's "Add repo" form
// (the fzf/gh owner browser stays in the CLI, which has a real TTY). Tracking an
// already-known repo is a no-op that returns the existing record.
func AddRepoSource(ctx context.Context, source string) (store.Repo, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return store.Repo{}, fmt.Errorf("no repo source given")
	}
	db, err := store.Open()
	if err != nil {
		return store.Repo{}, err
	}
	defer db.Close()

	name := git.NameFromSource(source)
	if name == "" {
		return store.Repo{}, fmt.Errorf("could not derive a repo name from %q", source)
	}
	if existing, ok, _ := db.Repo(name); ok {
		return existing, nil // already tracked
	}
	if err := os.MkdirAll(core.ReposDir(), 0o755); err != nil {
		return store.Repo{}, err
	}
	gitDir := filepath.Join(core.ReposDir(), name+".git")

	clone := func() error {
		if looksLikeGHRepo(source) {
			return gh.CloneBare(ctx, source, gitDir)
		}
		src := expandHome(source)
		if git.LooksLocal(src) {
			abs, _ := filepath.Abs(src)
			if !git.IsGitRepo(ctx, abs) {
				return fmt.Errorf("%s is not a git repository", abs)
			}
			src = abs
		}
		return git.CloneBare(ctx, src, gitDir)
	}
	if err := clone(); err != nil {
		return store.Repo{}, err
	}
	r := store.Repo{Name: name, Source: source, GitDir: gitDir}
	if err := db.PutRepo(r); err != nil {
		return store.Repo{}, err
	}
	// The repo's home session (store.RoleRepo) exists from the moment the repo is
	// tracked, so the rail's repo row is openable right away. Best-effort: a
	// failure here is logged and ResolveSession creates it on first open.
	if _, err := ensureRepoHome(db, name); err != nil {
		log.Printf("amux: creating repo %s's home session: %v", name, err)
	}
	return r, nil
}

// RemoveRepo untracks a repository: it refuses if any agent's worktree still
// uses it (those worktrees live inside the bare clone we'd delete), then removes
// the clone from disk and the store record. It's the daemon-side core of the
// CLI's `repo rm`, so the CLI never opens the store to untrack a repo.
func RemoveRepo(name string) error {
	db, err := store.Open()
	if err != nil {
		return err
	}
	defer db.Close()
	r, ok, err := db.Repo(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no such repo %q\n  %s", name, trackedRepos(db))
	}
	if users, err := repoUsers(db, name); err != nil {
		return err
	} else if len(users) > 0 {
		return fmt.Errorf("repo %q is in use by: %s\n  delete those first (amux workgroup rm <id>)",
			name, strings.Join(users, ", "))
	}
	removeRepoHome(db, name)
	_ = os.RemoveAll(r.GitDir)
	return db.DeleteRepo(name)
}

// trackedRepos renders the tracked-repo names for a "no such repo" error, so a
// typo answers with the list to pick from instead of sending the user off to run
// `amux repo ls`. With nothing tracked it says how to track the first one. Any
// read failure degrades to a bare pointer — the error being reported is the
// repo that isn't there, not our failure to enumerate.
func trackedRepos(db *store.DB) string {
	repos, err := db.Repos()
	if err != nil {
		return "see `amux repo ls` for the tracked repos"
	}
	if len(repos) == 0 {
		return "no repos are tracked yet — add one with `amux repo add <url|path|OWNER/REPO>`"
	}
	names := make([]string, 0, len(repos))
	for _, r := range repos {
		names = append(names, r.Name)
	}
	return "tracked repos: " + strings.Join(names, ", ")
}

// repoUsers returns the ids of agents (sub-sessions) whose worktrees include repo.
func repoUsers(db *store.DB, repo string) ([]string, error) {
	sessions, err := db.AllSessions()
	if err != nil {
		return nil, err
	}
	var users []string
	for _, s := range sessions {
		if s.IsRoot() {
			continue
		}
		for _, r := range store.SplitRepos(s.Repo) {
			if r == repo {
				users = append(users, s.ID)
				break
			}
		}
	}
	return users, nil
}

// looksLikeGHRepo reports whether s is a bare "owner/name" GitHub slug (cloned
// via gh) rather than a URL or local path.
func looksLikeGHRepo(s string) bool {
	if strings.Contains(s, "://") || strings.Contains(s, "@") {
		return false
	}
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") {
		return false
	}
	if _, err := os.Stat(s); err == nil {
		return false
	}
	parts := strings.Split(s, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
