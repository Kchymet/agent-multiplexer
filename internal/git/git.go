// Package git wraps the handful of git operations amux needs: bare clones into
// the legacy host repo inventory and isolated linked worktrees for sessions.
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
)

func run(ctx context.Context, dir string, args ...string) (string, error) {
	return runEnv(ctx, dir, nil, args...)
}

func runEnv(ctx context.Context, dir string, extraEnv []string, args ...string) (string, error) {
	return runEnvInput(ctx, dir, extraEnv, nil, args...)
}

func runEnvInput(ctx context.Context, dir string, extraEnv []string, input []byte, args ...string) (string, error) {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND=ssh -oBatchMode=yes")
	env = append(env, extraEnv...)
	return runExactEnvInput(ctx, dir, env, input, args...)
}

func runExactEnvInput(ctx context.Context, dir string, env []string, input []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// Never block on an interactive credential/host prompt (the daemon has no
	// usable TTY) — fail fast instead so callers can fall back.
	cmd.Env = env
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
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

// CloneBare creates the legacy tracked-repository inventory at gitDir. New
// session worktrees never use this mutable directory as an object pool or common
// directory.
func CloneBare(ctx context.Context, source, gitDir string) error {
	if _, err := run(ctx, "", "clone", "--bare", source, gitDir); err != nil {
		return err
	}
	_, _ = run(ctx, "", "--git-dir", gitDir, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
	_, _ = run(ctx, "", "--git-dir", gitDir, "fetch", "--prune", "origin")
	_, _ = run(ctx, "", "--git-dir", gitDir, "remote", "set-head", "origin", "-a")
	return nil
}

// InitBareInventory creates the small compatibility repository retained for
// legacy diagnostics. It has an origin but fetches no refs or objects; new
// worktrees use PrepareObjectPool instead.
func InitBareInventory(ctx context.Context, source, gitDir string) error {
	if _, err := os.Lstat(gitDir); err == nil {
		return fmt.Errorf("Git inventory path already exists: %s", gitDir)
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := runPoolGit(ctx, "", source, "init", "--bare", "--initial-branch=main", gitDir); err != nil {
		return err
	}
	_, err := runPoolGit(ctx, "", source, "--git-dir", gitDir, "config", "remote.origin.url", source)
	return err
}

// CheckoutRequest contains daemon-authoritative inputs for one isolated linked
// worktree. AllowLocalSource is an explicit trust acknowledgement for a local
// source that no untrusted session can modify.
type CheckoutRequest struct {
	Source           string
	Path             string
	Branch           string
	RepoKey          string
	PoolRoot         string
	StagingRoot      string
	ManagedRoot      string
	LayoutPath       string
	AllowLocalSource bool
}

// AddCheckout creates a genuine linked worktree with a session-private common
// directory. Its base objects come from an immutable authorized pool generation
// and are not copied, hardlinked, repacked, or transferred per session.
func AddCheckout(ctx context.Context, req CheckoutRequest) error {
	if req.Branch == "" {
		return fmt.Errorf("create pooled worktree: empty branch")
	}
	for label, value := range map[string]string{
		"source": req.Source, "repository key": req.RepoKey, "pool root": req.PoolRoot,
		"staging root": req.StagingRoot, "managed root": req.ManagedRoot, "layout record path": req.LayoutPath,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("create pooled worktree: empty %s", label)
		}
	}
	pool, err := objectPoolForCheckout(ctx, req.PoolRoot, req.RepoKey, req.Source, req.AllowLocalSource)
	if err != nil {
		return fmt.Errorf("prepare Git object pool: %w", err)
	}
	if len(pool.Mounts) == 0 {
		return fmt.Errorf("prepare Git object pool: empty generation closure")
	}
	if err := os.MkdirAll(req.StagingRoot, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(req.StagingRoot, "worktree-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	stagedCommon := filepath.Join(tmp, "common.git")
	stagedCheckout := filepath.Join(tmp, "checkout")
	commonDir := filepath.Join(filepath.Dir(req.Path), ".amux", "git", req.RepoKey+".git")
	commonParent := filepath.Dir(commonDir)
	if err := mkdirAllAnchored(req.ManagedRoot, commonParent, 0o700); err != nil {
		return fmt.Errorf("prepare private Git metadata directory: %w", err)
	}
	if _, err := os.Lstat(commonDir); err == nil {
		return fmt.Errorf("private Git common directory already exists: %s", commonDir)
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := runPoolGit(ctx, "", req.Source, "init", "--bare", "--initial-branch=main", stagedCommon); err != nil {
		return err
	}
	poolObjects := make([]string, 0, len(pool.Mounts))
	for _, mount := range pool.Mounts {
		poolObjects = append(poolObjects, mount.ObjectsMountDir)
	}
	if err := writeAlternates(filepath.Join(stagedCommon, "objects"), poolObjects); err != nil {
		return err
	}
	if _, err := runPoolGit(ctx, "", req.Source, "--git-dir", stagedCommon, "config", "remote.origin.url", req.Source); err != nil {
		return err
	}
	start := pool.BaseOID
	if start == "" {
		// Git cannot add a linked worktree to an unborn bare HEAD. Seed only this
		// private common directory with a deterministic empty root commit.
		tree, hashErr := runPoolGitInput(ctx, "", req.Source, nil, []byte{}, "--git-dir", stagedCommon, "mktree")
		if hashErr != nil {
			return hashErr
		}
		env := []string{
			"GIT_AUTHOR_NAME=amux", "GIT_AUTHOR_EMAIL=amux@example.invalid", "GIT_AUTHOR_DATE=@0 +0000",
			"GIT_COMMITTER_NAME=amux", "GIT_COMMITTER_EMAIL=amux@example.invalid", "GIT_COMMITTER_DATE=@0 +0000",
		}
		start, err = runPoolGitInput(ctx, "", req.Source, env, []byte("initialize empty repository\n"), "--git-dir", stagedCommon, "commit-tree", tree)
		if err != nil {
			return err
		}
	}
	if _, err := runPoolGit(ctx, "", req.Source, "--git-dir", stagedCommon, "update-ref", "refs/heads/"+req.Branch, start); err != nil {
		return err
	}
	if _, err := runPoolGit(ctx, "", req.Source, "--git-dir", stagedCommon, "symbolic-ref", "HEAD", "refs/heads/"+req.Branch); err != nil {
		return err
	}
	if _, err := runPoolGit(ctx, "", req.Source, "--git-dir", stagedCommon, "worktree", "add", "--", stagedCheckout, req.Branch); err != nil {
		return err
	}
	adminDir, err := linkedAdminDir(tmp, stagedCheckout)
	if err != nil {
		return err
	}
	if err := validateStagedWorktree(ctx, req, stagedCommon, stagedCheckout, adminDir, poolObjects); err != nil {
		return err
	}
	adminName := filepath.Base(adminDir)
	finalAdmin := filepath.Join(commonDir, "worktrees", adminName)
	if err := os.WriteFile(filepath.Join(stagedCheckout, ".git"), []byte("gitdir: "+finalAdmin+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(adminDir, "gitdir"), []byte(filepath.Join(req.Path, ".git")+"\n"), 0o644); err != nil {
		return err
	}
	if err := publishCheckout(stagedCommon, commonDir, req.ManagedRoot); err != nil {
		return fmt.Errorf("publish private Git metadata (StateDir staging and session storage must share a filesystem): %w", err)
	}
	if err := publishCheckout(stagedCheckout, req.Path, req.ManagedRoot); err != nil {
		cleanupErr := removeTreeAnchored(commonDir, req.ManagedRoot, req.StagingRoot)
		return errors.Join(fmt.Errorf("publish linked checkout (StateDir staging and session storage must share a filesystem): %w", err), cleanupErr)
	}
	layout := CheckoutLayout{
		Version: pooledWorktreeLayoutVersion, RepoKey: req.RepoKey, CheckoutPath: filepath.Clean(req.Path),
		CommonDir: filepath.Clean(commonDir), AdminDir: filepath.Clean(finalAdmin), Branch: req.Branch,
		Source: req.Source, DefaultRef: pool.DefaultRef, BaseOID: pool.BaseOID,
		SourceBoundary: pool.SourcePolicy, ObjectMounts: pool.Mounts,
	}
	if err := writeCheckoutLayout(req.LayoutPath, layout); err != nil {
		checkoutErr := removeTreeAnchored(req.Path, req.ManagedRoot, req.StagingRoot)
		commonErr := removeTreeAnchored(commonDir, req.ManagedRoot, req.StagingRoot)
		return errors.Join(fmt.Errorf("record pooled worktree layout: %w", err), checkoutErr, commonErr)
	}
	return nil
}

// RemoveCheckout removes a pooled/private or prior independent checkout using
// its trusted record, without invoking session-controlled Git. A recordless
// legacy worktree retains bounded compatibility cleanup.
func RemoveCheckout(ctx context.Context, gitDir, path, branch, managedRoot, stagingRoot, layoutPath string) error {
	layout, independent, err := readCheckoutLayout(layoutPath, path)
	if err != nil {
		return err
	}
	if err := removeTreeAnchored(path, managedRoot, stagingRoot); err != nil {
		return err
	}
	if layout != nil {
		if err := removeTreeAnchored(layout.CommonDir, managedRoot, stagingRoot); err != nil {
			return fmt.Errorf("remove private Git common directory: %w", err)
		}
		if err := os.Remove(layoutPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove pooled worktree layout record: %w", err)
		}
		return nil
	}
	if independent {
		if err := os.Remove(layoutPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove independent checkout layout record: %w", err)
		}
		return nil
	}
	return removeLegacyWorktreeMetadata(ctx, gitDir, branch)
}

func linkedAdminDir(managedRoot, checkout string) (string, error) {
	b, err := readRegularFileAnchored(managedRoot, filepath.Join(checkout, ".git"), gitPointerMaxBytes)
	if err != nil {
		return "", err
	}
	const prefix = "gitdir: "
	s := strings.TrimSpace(string(b))
	if !strings.HasPrefix(s, prefix) || !filepath.IsAbs(strings.TrimPrefix(s, prefix)) {
		return "", fmt.Errorf("linked worktree .git has invalid target")
	}
	return filepath.Clean(strings.TrimPrefix(s, prefix)), nil
}

func validateStagedWorktree(ctx context.Context, req CheckoutRequest, common, checkout, adminDir string, poolObjects []string) error {
	commonReal, err := filepath.EvalSymlinks(common)
	if err != nil {
		return err
	}
	adminReal, err := filepath.EvalSymlinks(adminDir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(commonReal, adminReal)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("linked worktree administration escapes private common directory")
	}
	alt, err := os.ReadFile(filepath.Join(common, "objects", "info", "alternates"))
	if err != nil || strings.TrimSpace(string(alt)) != strings.Join(poolObjects, "\n") {
		return fmt.Errorf("private common object alternates do not match trusted pool closure")
	}
	branch, err := runPoolGit(ctx, checkout, req.Source, "branch", "--show-current")
	if err != nil {
		return err
	}
	if branch != req.Branch {
		return fmt.Errorf("linked worktree branch %q does not match %q", branch, req.Branch)
	}
	origin, err := runPoolGit(ctx, checkout, req.Source, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if origin != req.Source {
		return fmt.Errorf("linked worktree origin %q does not match authoritative source", origin)
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
