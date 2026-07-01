// Package git wraps the handful of git operations amux needs: bare clones into
// the repo store and worktree create/remove for workspaces.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Never block on an interactive credential/host prompt (the daemon has no
	// usable TTY) — fail fast instead so callers can fall back.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -oBatchMode=yes")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// NameFromSource derives a short repo name from a URL or local path.
func NameFromSource(source string) string {
	s := strings.TrimSuffix(strings.TrimRight(source, "/"), ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// CloneBare creates a bare clone of source at gitDir (a worktree source) and
// configures it to track the remote's branches under refs/remotes/origin/* so
// later fetches can update them and worktrees can be based on the remote tip.
func CloneBare(ctx context.Context, source, gitDir string) error {
	if _, err := run(ctx, "", "clone", "--bare", source, gitDir); err != nil {
		return err
	}
	_, _ = run(ctx, "", "--git-dir", gitDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	_, _ = run(ctx, "", "--git-dir", gitDir, "fetch", "--prune", "origin")
	_, _ = run(ctx, "", "--git-dir", gitDir, "remote", "set-head", "origin", "-a")
	return nil
}

// Fetch updates the bare repo from origin (best-effort). It ensures the
// remote-tracking refspec exists first, so a plain `--bare` clone starts
// tracking refs/remotes/origin/* too.
func Fetch(ctx context.Context, gitDir string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, _ = run(ctx, "", "--git-dir", gitDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	_, err := run(ctx, "", "--git-dir", gitDir, "fetch", "--prune", "origin")
	_, _ = run(ctx, "", "--git-dir", gitDir, "remote", "set-head", "origin", "-a")
	return err
}

// DefaultBranch returns the bare repo's HEAD branch (e.g. "main").
func DefaultBranch(ctx context.Context, gitDir string) string {
	out, err := run(ctx, "", "--git-dir", gitDir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "HEAD"
	}
	return out
}

// remoteDefaultRef is the ref new work should be based on: origin's default
// branch (e.g. origin/main) when known, else the bare repo's local default.
func remoteDefaultRef(ctx context.Context, gitDir string) string {
	if out, err := run(ctx, "", "--git-dir", gitDir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil && out != "" {
		return out // e.g. "origin/main"
	}
	def := DefaultBranch(ctx, gitDir)
	if _, err := run(ctx, "", "--git-dir", gitDir, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+def); err == nil {
		return "origin/" + def
	}
	return def
}

// AddWorktree fetches the latest from origin, then creates a worktree at path on
// a new branch based on the remote's default branch — so every agent starts from
// the latest remote, not a stale snapshot. Falls back to the bare HEAD when
// there's no reachable remote (e.g. an empty or local-only repo).
func AddWorktree(ctx context.Context, gitDir, path, branch string) error {
	_ = Fetch(ctx, gitDir) // best-effort: stay current with the remote
	start := remoteDefaultRef(ctx, gitDir)
	if _, err := run(ctx, "", "--git-dir", gitDir, "worktree", "add", "-b", branch, path, start); err == nil {
		return nil
	}
	_, err := run(ctx, "", "--git-dir", gitDir, "worktree", "add", "-b", branch, path)
	return err
}

// RemoveWorktree removes a worktree (force, to drop uncommitted changes) and
// then deletes its branch from the bare repo.
func RemoveWorktree(ctx context.Context, gitDir, path, branch string) error {
	if _, err := run(ctx, "", "--git-dir", gitDir, "worktree", "remove", "--force", path); err != nil {
		// Fall back to pruning if the dir is already gone.
		_, _ = run(ctx, "", "--git-dir", gitDir, "worktree", "prune")
	}
	if branch != "" {
		_, _ = run(ctx, "", "--git-dir", gitDir, "branch", "-D", branch)
	}
	return nil
}

// IsGitRepo reports whether path is inside a git working tree.
func IsGitRepo(ctx context.Context, path string) bool {
	out, err := run(ctx, path, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// Exclude adds pattern to the local, uncommitted excludes of the worktree at dir
// (the file `git rev-parse --git-path info/exclude` resolves to — the per-repo
// exclude, never a tracked .gitignore). amux uses it so a settings file it drops
// into an agent's worktree never shows as untracked or gets swept into a commit.
// Idempotent, and best-effort: a non-repo dir or any git error is returned for
// the caller to ignore.
func Exclude(ctx context.Context, dir, pattern string) error {
	rel, err := run(ctx, dir, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	excl := rel
	if !filepath.IsAbs(excl) {
		excl = filepath.Join(dir, rel)
	}
	if b, err := os.ReadFile(excl); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.TrimSpace(line) == pattern {
				return nil // already excluded
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(excl), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(excl, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(pattern + "\n")
	return err
}

// LooksLocal reports whether source is a local filesystem path (vs a URL).
func LooksLocal(source string) bool {
	if strings.Contains(source, "://") {
		return false
	}
	if strings.Contains(source, "@") && strings.Contains(source, ":") {
		return false // scp-like git@host:repo
	}
	return strings.HasPrefix(source, "/") || strings.HasPrefix(source, ".") || strings.HasPrefix(source, "~") || filepath.IsAbs(source)
}
