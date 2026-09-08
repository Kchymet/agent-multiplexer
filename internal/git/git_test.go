package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testGit(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=amux test", "GIT_AUTHOR_EMAIL=amux@example.invalid",
		"GIT_COMMITTER_NAME=amux test", "GIT_COMMITTER_EMAIL=amux@example.invalid",
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func testRemote(t testing.TB) string {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	remote := filepath.Join(root, "upstream.git")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, src, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, src, "add", "README.md")
	testGit(t, src, "commit", "-q", "-m", "base")
	testGit(t, "", "clone", "-q", "--bare", src, remote)
	return remote
}

func testAddCheckout(t *testing.T, ctx context.Context, source, path, branch, staging string) string {
	t.Helper()
	layout := filepath.Join(t.TempDir(), "layout")
	if err := AddCheckout(ctx, testCheckoutRequest(source, path, branch, staging, filepath.Dir(path), layout)); err != nil {
		t.Fatal(err)
	}
	return layout
}

func testCheckoutRequest(source, path, branch, staging, managed, layout string) CheckoutRequest {
	return CheckoutRequest{
		Source: source, Path: path, Branch: branch, RepoKey: SourceKey(source),
		PoolRoot: filepath.Join(filepath.Dir(staging), "pool"), StagingRoot: staging,
		ManagedRoot: managed, LayoutPath: layout, AllowLocalSource: true,
	}
}

func TestAddCheckoutCreatesPrivateCommonLinkedWorktrees(t *testing.T) {
	ctx := context.Background()
	remote := testRemote(t)
	root := t.TempDir()
	a := filepath.Join(root, "a", "repo")
	b := filepath.Join(root, "b", "repo")
	if err := os.MkdirAll(filepath.Dir(a), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(b), 0o700); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(root, "staging")
	layouts := map[string]string{
		a: testAddCheckout(t, ctx, remote, a, "amux/root-a", staging),
		b: testAddCheckout(t, ctx, remote, b, "amux/root-b", staging),
	}
	commons := map[string]string{}
	for path, branch := range map[string]string{a: "amux/root-a", b: "amux/root-b"} {
		if err := ValidateCheckoutLayout(layouts[path], path, filepath.Dir(path)); err != nil {
			t.Fatalf("%s layout is invalid: %v", path, err)
		}
		common := testGit(t, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
		commons[path] = common
		if common == filepath.Join(path, ".git") {
			t.Fatalf("%s is a standalone clone, want linked worktree", path)
		}
		if got := testGit(t, path, "branch", "--show-current"); got != branch {
			t.Fatalf("%s branch = %q, want %q", path, got, branch)
		}
		if got := testGit(t, path, "remote", "get-url", "origin"); got != remote {
			t.Fatalf("%s origin = %q, want %q", path, got, remote)
		}
		if data, err := os.ReadFile(filepath.Join(common, "objects", "info", "alternates")); err != nil || strings.TrimSpace(string(data)) == "" {
			t.Fatalf("%s missing immutable pool alternate: %q, %v", path, data, err)
		}
	}
	if commons[a] == commons[b] {
		t.Fatal("sessions unexpectedly share a writable Git common directory")
	}

	// Session-local config and commits cannot affect the sibling or source.
	testGit(t, a, "config", "--local", "core.hooksPath", filepath.Join(a, "hooks"))
	if cmd := exec.Command("git", "-C", b, "config", "--local", "--get", "core.hooksPath"); cmd.Run() == nil {
		t.Fatal("session B inherited session A's local Git config")
	}
	if err := os.WriteFile(filepath.Join(a, "private.txt"), []byte("session a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, a, "add", "private.txt")
	testGit(t, a, "commit", "-q", "-m", "private")
	privateCommit := testGit(t, a, "rev-parse", "HEAD")
	for _, repo := range []string{b, remote} {
		if cmd := exec.Command("git", "-C", repo, "cat-file", "-e", privateCommit+"^{commit}"); cmd.Run() == nil {
			t.Fatalf("private commit %s leaked into %s", privateCommit, repo)
		}
	}

	// Removing the authoritative source proves both worktrees use the retained
	// immutable pool rather than the legacy source repository.
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{a, b} {
		if got := testGit(t, repo, "show", "--format=%s", "--no-patch", "HEAD"); got == "" {
			t.Fatalf("%s lost its history with the source", repo)
		}
	}
}

func TestLayoutValidationRejectsBlockingPointerFilesWithoutWaiting(t *testing.T) {
	tests := []struct {
		name   string
		target func(string, CheckoutLayout) string
	}{
		{"checkout-dot-git", func(checkout string, _ CheckoutLayout) string { return filepath.Join(checkout, ".git") }},
		{"admin-gitdir", func(_ string, layout CheckoutLayout) string { return filepath.Join(layout.AdminDir, "gitdir") }},
		{"private-alternates", func(_ string, layout CheckoutLayout) string {
			return filepath.Join(layout.CommonDir, "objects", "info", "alternates")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remote := testRemote(t)
			root := t.TempDir()
			checkout, layout := pooledTestCheckout(t, root, remote, SourceKey(remote), "session")
			target := tt.target(checkout, layout)
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(target, 0o600); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			if err := ValidateCheckoutLayout(filepath.Join(root, "session.json"), checkout, root); err == nil {
				t.Fatal("validation accepted a FIFO pointer")
			}
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Fatalf("validation blocked on FIFO for %s", elapsed)
			}
		})
	}
}

func TestLayoutValidationBoundsPointerSizes(t *testing.T) {
	tests := []struct {
		name   string
		limit  int
		target func(string, CheckoutLayout) string
	}{
		{"checkout-dot-git", gitPointerMaxBytes, func(checkout string, _ CheckoutLayout) string { return filepath.Join(checkout, ".git") }},
		{"admin-gitdir", gitPointerMaxBytes, func(_ string, layout CheckoutLayout) string { return filepath.Join(layout.AdminDir, "gitdir") }},
		{"private-alternates", gitAlternatesMaxBytes, func(_ string, layout CheckoutLayout) string {
			return filepath.Join(layout.CommonDir, "objects", "info", "alternates")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			remote := testRemote(t)
			root := t.TempDir()
			checkout, layout := pooledTestCheckout(t, root, remote, SourceKey(remote), "session")
			target := tt.target(checkout, layout)
			if err := os.WriteFile(target, make([]byte, tt.limit+1), 0o600); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			err := ValidateCheckoutLayout(filepath.Join(root, "session.json"), checkout, root)
			if err == nil || !strings.Contains(err.Error(), "limit") {
				t.Fatalf("oversized pointer error = %v", err)
			}
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Fatalf("oversized pointer validation took %s", elapsed)
			}
		})
	}
}

func TestAnchoredReadNeverFollowsRacingFinalSymlink(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "session")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "pointer")
	outside := filepath.Join(t.TempDir(), "secret")
	const safe = "expected\n"
	const secret = "outside-secret\n"
	if err := os.WriteFile(outside, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(safe), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	finished := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			default:
			}
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				errCh <- err
				return
			}
			if err := os.Symlink(outside, target); err != nil && !os.IsExist(err) {
				errCh <- err
				return
			}
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				errCh <- err
				return
			}
			if err := os.WriteFile(target, []byte(safe), 0o600); err != nil {
				errCh <- err
				return
			}
		}
	}()
	var escaped []byte
	for i := 0; i < 1000; i++ {
		b, err := readRegularFileAnchored(root, target, gitPointerMaxBytes)
		if err == nil && string(b) == secret {
			escaped = b
			break
		}
	}
	close(done)
	<-finished
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}
	if escaped != nil {
		t.Fatalf("anchored read escaped to racing target: %q", escaped)
	}
}

func TestAddCheckoutTransfersOnlyDefaultBranchReachability(t *testing.T) {
	ctx := context.Background()
	remote := testRemote(t)
	writer := filepath.Join(t.TempDir(), "legacy-writer")
	testGit(t, "", "clone", "-q", remote, writer)
	testGit(t, writer, "checkout", "-q", "-b", "amux/sibling-unpublished")
	if err := os.WriteFile(filepath.Join(writer, "sibling-secret"), []byte("not authorized\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGit(t, writer, "add", "sibling-secret")
	testGit(t, writer, "commit", "-q", "-m", "sibling private")
	secretCommit := testGit(t, writer, "rev-parse", "HEAD")
	testGit(t, writer, "push", "-q", "origin", "HEAD:refs/heads/amux/sibling-unpublished")

	checkout := filepath.Join(t.TempDir(), "session")
	testAddCheckout(t, ctx, remote, checkout, "amux/root-own", filepath.Join(t.TempDir(), "staging"))
	if cmd := exec.Command("git", "-C", checkout, "cat-file", "-e", secretCommit+"^{commit}"); cmd.Run() == nil {
		t.Fatalf("authorized pool exposed sibling-only object %s", secretCommit)
	}
	if cmd := exec.Command("git", "-C", checkout, "show-ref", "--verify", "refs/remotes/origin/amux/sibling-unpublished"); cmd.Run() == nil {
		t.Fatal("pooled worktree copied sibling unpublished ref")
	}
}

func TestAddCheckoutSupportsEmptyRepository(t *testing.T) {
	ctx := context.Background()
	remote := filepath.Join(t.TempDir(), "empty.git")
	testGit(t, "", "init", "-q", "--bare", "--initial-branch=main", remote)
	checkout := filepath.Join(t.TempDir(), "session")
	testAddCheckout(t, ctx, remote, checkout, "amux/root-empty", filepath.Join(t.TempDir(), "staging"))
	if got := testGit(t, checkout, "branch", "--show-current"); got != "amux/root-empty" {
		t.Fatalf("empty checkout branch = %q", got)
	}
}

func TestAddCheckoutRejectsExtTransport(t *testing.T) {
	ctx := context.Background()
	marker := filepath.Join(t.TempDir(), "executed")
	helper := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf invoked >\"$AMUX_TEST_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUX_TEST_MARKER", marker)
	root := t.TempDir()
	staging := filepath.Join(t.TempDir(), "staging")
	err := AddCheckout(ctx, testCheckoutRequest("ext::"+helper, filepath.Join(root, "session"), "amux/root-own",
		staging, root, filepath.Join(t.TempDir(), "layout")))
	if err == nil {
		t.Fatal("ext transport unexpectedly allowed")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("ext transport executed a source command: %v", statErr)
	}
}

func TestRemoveCheckoutDoesNotRunSessionGitConfiguration(t *testing.T) {
	ctx := context.Background()
	remote := testRemote(t)
	checkout := filepath.Join(t.TempDir(), "session")
	staging := filepath.Join(t.TempDir(), "staging")
	layout := testAddCheckout(t, ctx, remote, checkout, "amux/root-own", staging)
	marker := filepath.Join(t.TempDir(), "executed")
	helper := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf invoked >\"$AMUX_TEST_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{"core.fsmonitor", helper},
		{"core.hooksPath", filepath.Dir(helper)},
		{"credential.helper", "!" + helper},
		{"uploadpack.packObjectsHook", helper},
	} {
		testGit(t, checkout, "config", "--local", kv[0], kv[1])
	}
	t.Setenv("AMUX_TEST_MARKER", marker)
	if err := RemoveCheckout(ctx, remote, checkout, "amux/root-own", filepath.Dir(checkout), staging, layout); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("host cleanup executed session-controlled Git configuration: %v", err)
	}
	if _, err := os.Stat(checkout); !os.IsNotExist(err) {
		t.Fatalf("checkout survived removal: %v", err)
	}
}

func TestLegacyCleanupIgnoresSharedGitCommandConfiguration(t *testing.T) {
	ctx := context.Background()
	cache := testRemote(t)
	worktree := filepath.Join(t.TempDir(), "legacy-worktree")
	testGit(t, "", "--git-dir", cache, "worktree", "add", "-q", "-b", "amux/legacy", worktree, "main")

	marker := filepath.Join(t.TempDir(), "executed")
	helper := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf invoked >\"$AMUX_TEST_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	included := filepath.Join(t.TempDir(), "included.gitconfig")
	if err := os.WriteFile(included, []byte("[core]\n\tfsmonitor = "+helper+"\n[credential]\n\thelper = !"+helper+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{"include.path", included},
		{"core.fsmonitor", helper},
		{"core.hooksPath", filepath.Dir(helper)},
		{"credential.helper", "!" + helper},
		{"uploadpack.packObjectsHook", helper},
	} {
		testGit(t, "", "--git-dir", cache, "config", "--local", "--add", kv[0], kv[1])
	}
	t.Setenv("AMUX_TEST_MARKER", marker)
	if got := ListBranches(ctx, cache, "amux/*"); len(got) != 1 || got[0] != "amux/legacy" {
		t.Fatalf("hardened branch inventory = %v, want [amux/legacy]", got)
	}
	if err := RemoveCheckout(ctx, cache, worktree, "amux/legacy", filepath.Dir(worktree),
		filepath.Join(t.TempDir(), "staging"), filepath.Join(t.TempDir(), "missing-layout")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("legacy host operation executed shared Git configuration: %v", err)
	}
}

func TestAddCheckoutRejectsSymlinkedSessionAncestor(t *testing.T) {
	remote := testRemote(t)
	managed := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(managed, "session")); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(t.TempDir(), "staging")
	err := AddCheckout(context.Background(), testCheckoutRequest(remote, filepath.Join(managed, "session", "repo"), "amux/own",
		staging, managed, filepath.Join(t.TempDir(), "layout")))
	if err == nil {
		t.Fatal("published through symlinked session ancestor")
	}
	if _, err := os.Lstat(filepath.Join(outside, "repo")); !os.IsNotExist(err) {
		t.Fatalf("outside destination changed: %v", err)
	}
}

func TestAddCheckoutDoesNotReplaceDestinationEntry(t *testing.T) {
	remote := testRemote(t)
	managed := t.TempDir()
	destination := filepath.Join(managed, "repo")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(destination, "keep")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(t.TempDir(), "staging")
	err := AddCheckout(context.Background(), testCheckoutRequest(remote, destination, "amux/own",
		staging, managed, filepath.Join(t.TempDir(), "layout")))
	if err == nil {
		t.Fatal("replaced an existing destination entry")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatalf("existing destination changed: data=%q err=%v", data, err)
	}
}

func TestRemoveCheckoutRejectsSymlinkedSessionAncestor(t *testing.T) {
	managed := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "repo")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keep"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(managed, "session")); err != nil {
		t.Fatal(err)
	}
	err := RemoveCheckout(context.Background(), "unused", filepath.Join(managed, "session", "repo"), "",
		managed, filepath.Join(t.TempDir(), "staging"), filepath.Join(t.TempDir(), "missing-layout"))
	if err == nil {
		t.Fatal("removed through symlinked session ancestor")
	}
	if _, err := os.Stat(filepath.Join(victim, "keep")); err != nil {
		t.Fatalf("outside victim changed: %v", err)
	}
}

func TestRemoveCheckoutUsesTrustedLayoutAfterGitDirectoryRename(t *testing.T) {
	ctx := context.Background()
	remote := testRemote(t)
	root := t.TempDir()
	checkout := filepath.Join(root, "session")
	staging := filepath.Join(t.TempDir(), "staging")
	layout := testAddCheckout(t, ctx, remote, checkout, "amux/own", staging)
	if err := os.Rename(filepath.Join(checkout, ".git"), filepath.Join(checkout, "saved-git")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCheckout(ctx, remote, checkout, "amux/own", root, staging, layout); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(checkout); !os.IsNotExist(err) {
		t.Fatalf("checkout survived trusted cleanup: %v", err)
	}
}

func TestRemoveCheckoutFailsClosedAfterSessionAncestorReplacement(t *testing.T) {
	ctx := context.Background()
	remote := testRemote(t)
	managed := t.TempDir()
	session := filepath.Join(managed, "session")
	if err := os.Mkdir(session, 0o700); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(session, "repo")
	staging := filepath.Join(t.TempDir(), "staging")
	layout := filepath.Join(t.TempDir(), "layout")
	if err := AddCheckout(ctx, testCheckoutRequest(remote, checkout, "amux/own", staging, managed, layout)); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(managed, "original-session")
	if err := os.Rename(session, original); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, session); err != nil {
		t.Fatal(err)
	}
	if err := RemoveCheckout(ctx, remote, checkout, "amux/own", managed, staging, layout); err == nil {
		t.Fatal("cleanup followed replaced session ancestor")
	}
	if _, err := os.Stat(filepath.Join(original, "repo", ".git")); err != nil {
		t.Fatalf("original checkout changed: %v", err)
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("replacement target changed: entries=%v err=%v", entries, err)
	}
}

func TestLegacyInventoryHasInternalDeadlineAndVisibleError(t *testing.T) {
	cache := testRemote(t)
	fifo := filepath.Join(t.TempDir(), "include.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	testGit(t, cache, "config", "include.path", fifo)
	start := time.Now()
	if _, err := ListBranchesE(context.Background(), cache, "amux/*"); err == nil {
		t.Fatal("host inventory hid blocking legacy config error")
	}
	if elapsed := time.Since(start); elapsed > legacyGitTimeout+time.Second {
		t.Fatalf("legacy inventory exceeded internal deadline: %v", elapsed)
	}
}

func TestFetchBeforeFirstPushAndPullAfterPushStayNarrow(t *testing.T) {
	ctx := context.Background()
	remote := testRemote(t)
	checkout := filepath.Join(t.TempDir(), "session")
	testAddCheckout(t, ctx, remote, checkout, "amux/own", filepath.Join(t.TempDir(), "staging"))

	// Advance the default branch and publish a sibling's private branch. A raw
	// fetch before this session's first push must succeed, update only FETCH_HEAD,
	// and leave the sibling commit unavailable even when its object ID is known.
	writer := filepath.Join(t.TempDir(), "writer")
	testGit(t, "", "clone", "-q", remote, writer)
	testGit(t, writer, "commit", "-q", "--allow-empty", "-m", "default advance")
	testGit(t, writer, "push", "-q", "origin", "main")
	testGit(t, writer, "checkout", "-q", "-b", "amux/sibling")
	testGit(t, writer, "commit", "-q", "--allow-empty", "-m", "sibling unpublished work")
	siblingCommit := testGit(t, writer, "rev-parse", "HEAD")
	testGit(t, writer, "push", "-q", "origin", "HEAD")

	assertNoFetchRefspec(t, checkout)
	testGit(t, checkout, "fetch", "origin")
	testGit(t, checkout, "merge", "--ff-only", "FETCH_HEAD")
	if got := testGit(t, checkout, "log", "-1", "--format=%s"); got != "default advance" {
		t.Fatalf("pre-push fetch merged %q, want remote default advance", got)
	}
	assertObjectMissing(t, checkout, siblingCommit)

	// Once the session publishes its own branch, push -u records the precise
	// upstream. A later plain pull follows only that branch and still does not
	// make the sibling commit inspectable.
	testGit(t, checkout, "commit", "-q", "--allow-empty", "-m", "own work")
	testGit(t, checkout, "push", "-q", "-u", "origin", "HEAD")
	assignedWriter := filepath.Join(t.TempDir(), "assigned-writer")
	testGit(t, "", "clone", "-q", "--branch", "amux/own", remote, assignedWriter)
	testGit(t, assignedWriter, "commit", "-q", "--allow-empty", "-m", "assigned advance")
	testGit(t, assignedWriter, "push", "-q", "origin", "HEAD")
	testGit(t, checkout, "pull", "--ff-only")
	if got := testGit(t, checkout, "log", "-1", "--format=%s"); got != "assigned advance" {
		t.Fatalf("post-push pull reached %q, want assigned branch advance", got)
	}
	assertNoFetchRefspec(t, checkout)
	assertObjectMissing(t, checkout, siblingCommit)
	if got := testGit(t, checkout, "for-each-ref", "--format=%(refname)", "refs/remotes/origin/amux/sibling"); got != "" {
		t.Fatalf("sibling remote-tracking ref was created: %s", got)
	}
}

func assertNoFetchRefspec(t *testing.T, checkout string) {
	t.Helper()
	cmd := exec.Command("git", "-C", checkout, "config", "--get-all", "remote.origin.fetch")
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("fetch refspec lookup = %v, output %q; want missing key", err, strings.TrimSpace(string(out)))
	}
}

func assertObjectMissing(t *testing.T, checkout, object string) {
	t.Helper()
	cmd := exec.Command("git", "-C", checkout, "cat-file", "-e", object+"^{commit}")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("sibling object %s is inspectable: %s", object, strings.TrimSpace(string(out)))
	}
}
