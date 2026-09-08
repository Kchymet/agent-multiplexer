// Package git wraps the handful of git operations amux needs: bare clones into
// the host repo cache and independent checkouts for sessions.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	legacyGitTimeout = 2 * time.Second
	layoutVersion    = "independent-v1\n"
)

func run(ctx context.Context, dir string, args ...string) (string, error) {
	return runEnv(ctx, dir, nil, args...)
}

func runEnv(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Never block on an interactive credential/host prompt (the daemon has no
	// usable TTY) — fail fast instead so callers can fall back.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -oBatchMode=yes")
	cmd.Env = append(cmd.Env, extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// runUntrustedRepo is for the narrow legacy operations that still have to read
// a formerly session-writable shared Git directory. It ignores host config and
// overrides repository settings that can invoke external helpers. New session
// clones are never passed here (or to any host-side Git command after launch).
func runUntrustedRepo(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, legacyGitTimeout)
	defer cancel()
	safe := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "credential.helper=",
		"-c", "core.sshCommand=/bin/false",
		"-c", "protocol.ext.allow=never",
		"-c", "protocol.file.allow=never",
		"-c", "uploadpack.packObjectsHook=/bin/false",
		"-c", "core.pager=cat",
	}
	safe = append(safe, args...)
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_COUNT=0",
		"GIT_CONFIG_PARAMETERS=",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_EXTERNAL_DIFF=",
		"GIT_PAGER=cat",
		"GIT_SSH_COMMAND=/bin/false",
		"GIT_PROTOCOL_FROM_USER=0",
		"PAGER=cat",
	}
	return runEnv(ctx, dir, env, safe...)
}

// ListBranchesE returns the local branch names in gitDir matching the glob
// pattern under refs/heads (e.g. "amux/*"), for reconciliation. The error form
// distinguishes an empty repository from a hostile or unreadable legacy one.
func ListBranchesE(ctx context.Context, gitDir, pattern string) ([]string, error) {
	out, err := runUntrustedRepo(ctx, gitDir, "for-each-ref", "--format=%(refname:short)", "refs/heads/"+pattern)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		if b := strings.TrimSpace(line); b != "" {
			branches = append(branches, b)
		}
	}
	return branches, nil
}

// ListBranches preserves the historical best-effort API. Callers that report
// diagnostics should use ListBranchesE so failures remain visible.
func ListBranches(ctx context.Context, gitDir, pattern string) []string {
	branches, _ := ListBranchesE(ctx, gitDir, pattern)
	return branches
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

// AddCheckout creates an independent repository at path on branch, cloning the
// repository's authoritative source directly. The checkout has its own objects,
// refs, config, hooks namespace and worktree metadata. The host cache remains
// inventory only and is never read while constructing the session clone.
//
// Clone through a temporary sibling and rename it into place so a failed clone
// never leaves a half-initialized session checkout at path. --no-hardlinks is
// essential for local sources: object files must not share writable inodes.
// --single-branch and --no-tags prevent a shared host cache's unpublished
// session refs/objects from being copied even when a legacy local source points
// at one. Only objects reachable from the source's advertised default branch
// enter a newly-created session.
func AddCheckout(ctx context.Context, source, path, branch, stagingRoot, managedRoot, layoutPath string) error {
	if branch == "" {
		return fmt.Errorf("create independent checkout: empty branch")
	}
	if strings.TrimSpace(source) == "" {
		return fmt.Errorf("create independent checkout: empty source")
	}
	if strings.TrimSpace(stagingRoot) == "" {
		return fmt.Errorf("create independent checkout: empty daemon-private staging root")
	}
	if strings.TrimSpace(managedRoot) == "" {
		return fmt.Errorf("create independent checkout: empty managed root")
	}
	if strings.TrimSpace(layoutPath) == "" {
		return fmt.Errorf("create independent checkout: empty layout record path")
	}
	if err := os.MkdirAll(stagingRoot, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(stagingRoot, "checkout-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	checkout := filepath.Join(tmp, "checkout")

	if _, err := run(ctx, "", "-c", "protocol.ext.allow=never", "clone", "--no-local", "--no-hardlinks", "--single-branch", "--no-tags", "--no-checkout", "--", source, checkout); err != nil {
		return err
	}
	start, err := run(ctx, checkout, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		// Empty repositories have no default-branch commit yet. Preserve support
		// by creating the assigned branch as an orphan inside the private clone.
		if _, checkoutErr := run(ctx, checkout, "checkout", "--orphan", branch); checkoutErr != nil {
			return fmt.Errorf("create assigned branch in empty checkout: %w", checkoutErr)
		}
	} else {
		if _, checkoutErr := run(ctx, checkout, "checkout", "-b", branch, start); checkoutErr != nil {
			return checkoutErr
		}
	}
	// Do not retain the clone-created default-branch refspec or install an exact
	// assigned-branch refspec yet. The exact ref does not exist before the first
	// push, so it makes an ordinary `git fetch origin` fail; a wildcard would
	// disclose sibling branches. With no configured fetch refspec, a pre-push
	// fetch gets only the remote HEAD into FETCH_HEAD. After `push -u`, ordinary
	// pull uses the branch's upstream merge ref without widening future fetches.
	if _, err := run(ctx, checkout, "config", "--unset-all", "remote.origin.fetch"); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 5 { // no matching key is already the desired state
			return err
		}
	}
	if err := validateIndependentCheckout(ctx, checkout); err != nil {
		return err
	}
	if err := publishCheckout(checkout, path, managedRoot); err != nil {
		return fmt.Errorf("publish independent checkout (staging and sessions storage must share a filesystem): %w", err)
	}
	if err := writeLayoutRecord(layoutPath, path); err != nil {
		cleanupErr := removeTreeAnchored(path, managedRoot, stagingRoot)
		return errors.Join(fmt.Errorf("record independent checkout layout: %w", err), cleanupErr)
	}
	return nil
}

// RemoveCheckout removes either a new independent checkout or a legacy linked
// worktree. Independent repositories are removed directly without invoking
// their session-controlled Git configuration. Legacy worktrees retain the old
// host-cache cleanup behavior for compatibility.
func RemoveCheckout(ctx context.Context, gitDir, path, branch, managedRoot, stagingRoot, layoutPath string) error {
	independent, err := hasLayoutRecord(layoutPath, path)
	if err != nil {
		return err
	}
	if err := removeTreeAnchored(path, managedRoot, stagingRoot); err != nil {
		return err
	}
	if independent {
		if err := os.Remove(layoutPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove independent checkout layout record: %w", err)
		}
		return nil
	}
	return removeLegacyWorktreeMetadata(ctx, gitDir, branch)
}

func validateIndependentCheckout(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("checkout root is not a real directory: %s", path)
	}
	common, err := run(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	realCommon, err := filepath.EvalSymlinks(common)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(realPath, realCommon)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("checkout Git common directory escapes session repository: %s", common)
	}
	if alt, err := run(ctx, path, "rev-parse", "--git-path", "objects/info/alternates"); err == nil {
		if !filepath.IsAbs(alt) {
			alt = filepath.Join(path, alt)
		}
		if b, readErr := os.ReadFile(alt); readErr == nil && strings.TrimSpace(string(b)) != "" {
			return fmt.Errorf("checkout uses a shared object alternate: %s", alt)
		}
	}
	return nil
}

// removeLegacyWorktreeMetadata cleans the shared bare repository after the
// checkout was detached with filesystem operations. Both failures are returned:
// legacy session-controlled config must never turn cleanup into false success.
func removeLegacyWorktreeMetadata(ctx context.Context, gitDir, branch string) error {
	_, pruneErr := runUntrustedRepo(ctx, "", "--git-dir", gitDir, "worktree", "prune", "--expire", "now")
	var branchErr error
	if branch != "" {
		_, branchErr = runUntrustedRepo(ctx, "", "--git-dir", gitDir, "branch", "-D", branch)
	}
	return errors.Join(pruneErr, branchErr)
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
