// Package git wraps the handful of git operations amux needs: bare clones into
// the repo store and worktree create/remove for workspaces.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

func run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
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

// CloneBare creates a bare clone of source at gitDir (a worktree source).
func CloneBare(ctx context.Context, source, gitDir string) error {
	_, err := run(ctx, "", "clone", "--bare", source, gitDir)
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

// AddWorktree creates a worktree at path on a new branch from the bare repo.
func AddWorktree(ctx context.Context, gitDir, path, branch string) error {
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
