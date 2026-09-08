package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testGit(t *testing.T, dir string, args ...string) string {
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

func testRemote(t *testing.T) string {
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
	if err := AddCheckout(ctx, source, path, branch, staging, filepath.Dir(path), layout); err != nil {
		t.Fatal(err)
	}
	return layout
}

func TestAddCheckoutCreatesIndependentRepositories(t *testing.T) {
	ctx := context.Background()
	remote := testRemote(t)
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	staging := filepath.Join(root, "staging")
	testAddCheckout(t, ctx, remote, a, "amux/root-a", staging)
	testAddCheckout(t, ctx, remote, b, "amux/root-b", staging)
	for path, branch := range map[string]string{a: "amux/root-a", b: "amux/root-b"} {
		if err := validateIndependentCheckout(ctx, path); err != nil {
			t.Fatalf("%s is not independent: %v", path, err)
		}
		if got := testGit(t, path, "branch", "--show-current"); got != branch {
			t.Fatalf("%s branch = %q, want %q", path, got, branch)
		}
		if got := testGit(t, path, "remote", "get-url", "origin"); got != remote {
			t.Fatalf("%s origin = %q, want %q", path, got, remote)
		}
		if data, err := os.ReadFile(filepath.Join(path, ".git", "objects", "info", "alternates")); err == nil && strings.TrimSpace(string(data)) != "" {
			t.Fatalf("%s has object alternate %q", path, data)
		}
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

	// Removing the source proves neither checkout depends on shared object bytes.
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{a, b} {
		if got := testGit(t, repo, "show", "--format=%s", "--no-patch", "HEAD"); got == "" {
			t.Fatalf("%s lost its history with the source", repo)
		}
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
		t.Fatalf("single-branch clone copied sibling-only object %s", secretCommit)
	}
	if cmd := exec.Command("git", "-C", checkout, "show-ref", "--verify", "refs/remotes/origin/amux/sibling-unpublished"); cmd.Run() == nil {
		t.Fatal("single-branch clone copied sibling unpublished ref")
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
	err := AddCheckout(ctx, "ext::"+helper, filepath.Join(root, "session"), "amux/root-own",
		filepath.Join(t.TempDir(), "staging"), root, filepath.Join(t.TempDir(), "layout"))
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
	err := AddCheckout(context.Background(), remote, filepath.Join(managed, "session", "repo"), "amux/own",
		filepath.Join(t.TempDir(), "staging"), managed, filepath.Join(t.TempDir(), "layout"))
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
	err := AddCheckout(context.Background(), remote, destination, "amux/own",
		filepath.Join(t.TempDir(), "staging"), managed, filepath.Join(t.TempDir(), "layout"))
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
	if err := AddCheckout(ctx, remote, checkout, "amux/own", staging, managed, layout); err != nil {
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

func TestAssignedBranchRefspecSupportsPlainPull(t *testing.T) {
	ctx := context.Background()
	remote := testRemote(t)
	checkout := filepath.Join(t.TempDir(), "session")
	testAddCheckout(t, ctx, remote, checkout, "amux/own", filepath.Join(t.TempDir(), "staging"))
	testGit(t, checkout, "commit", "-q", "--allow-empty", "-m", "own work")
	testGit(t, checkout, "push", "-q", "-u", "origin", "HEAD")
	testGit(t, checkout, "pull", "--ff-only")
	refspecs := strings.Split(testGit(t, checkout, "config", "--get-all", "remote.origin.fetch"), "\n")
	if len(refspecs) != 2 || refspecs[1] != "+refs/heads/amux/own:refs/remotes/origin/amux/own" {
		t.Fatalf("assigned fetch refspecs = %v", refspecs)
	}
}
