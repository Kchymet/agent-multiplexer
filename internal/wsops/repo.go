package wsops

import (
	"context"
	"fmt"
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
	return r, db.PutRepo(r)
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
		return fmt.Errorf("no such repo %q", name)
	}
	if users, err := repoUsers(db, name); err != nil {
		return err
	} else if len(users) > 0 {
		return fmt.Errorf("repo %q is in use by: %s\n  delete those first (amux workgroup rm <id>)",
			name, strings.Join(users, ", "))
	}
	_ = os.RemoveAll(r.GitDir)
	return db.DeleteRepo(name)
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

// rootRepos returns the repos attached to a workgroup root, or nil. Used to
// default an added agent to the whole workgroup when the form leaves repos blank.
func rootRepos(rootID string) []string {
	db, err := store.Open()
	if err != nil {
		return nil
	}
	defer db.Close()
	if root, ok, _ := db.GetSession(rootID); ok {
		return store.SplitRepos(root.Repo)
	}
	return nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
