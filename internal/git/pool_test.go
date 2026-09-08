package git

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func loadTestLayout(t testing.TB, path string) CheckoutLayout {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var layout CheckoutLayout
	if err := json.Unmarshal(b, &layout); err != nil {
		t.Fatal(err)
	}
	return layout
}

func directObjectPresent(t testing.TB, objects, oid string) bool {
	t.Helper()
	if len(oid) >= 3 {
		if _, err := os.Stat(filepath.Join(objects, oid[:2], oid[2:])); err == nil {
			return true
		}
	}
	idxs, err := filepath.Glob(filepath.Join(objects, "pack", "*.idx"))
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range idxs {
		cmd := exec.Command("git", "verify-pack", "-v", idx)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("verify pack %s: %v", idx, err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, oid+" ") {
				return true
			}
		}
	}
	return false
}

func objectDataBytes(t testing.TB, objects string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(objects, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && filepath.Base(path) != "alternates" {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return total
}

func pooledTestCheckout(t *testing.T, root, remote, key, name string) (string, CheckoutLayout) {
	t.Helper()
	agent := filepath.Join(root, name)
	if err := os.Mkdir(agent, 0o700); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(agent, "repo")
	layoutPath := filepath.Join(root, name+".json")
	if err := AddCheckout(context.Background(), CheckoutRequest{
		Source: remote, Path: checkout, Branch: "amux/" + name, RepoKey: key,
		PoolRoot: filepath.Join(root, "pool"), StagingRoot: filepath.Join(root, "staging"),
		ManagedRoot: root, LayoutPath: layoutPath, AllowLocalSource: true,
	}); err != nil {
		t.Fatal(err)
	}
	return checkout, loadTestLayout(t, layoutPath)
}

func TestPoolReuseDoesNotDuplicateBaseObjectsPerWorktree(t *testing.T) {
	remote := testRemote(t)
	root := t.TempDir()
	key := SourceKey(remote)
	a, layoutA := pooledTestCheckout(t, root, remote, key, "a")
	b, layoutB := pooledTestCheckout(t, root, remote, key, "b")
	if len(layoutA.ObjectMounts) != 1 || len(layoutB.ObjectMounts) != 1 || layoutA.ObjectMounts[0] != layoutB.ObjectMounts[0] {
		t.Fatalf("pool generation not reused: A=%+v B=%+v", layoutA.ObjectMounts, layoutB.ObjectMounts)
	}
	if layoutA.CommonDir == layoutB.CommonDir {
		t.Fatal("worktrees share writable Git common metadata")
	}
	for _, layout := range []CheckoutLayout{layoutA, layoutB} {
		if got := objectDataBytes(t, filepath.Join(layout.CommonDir, "objects")); got != 0 {
			t.Fatalf("session private common duplicated %d base object bytes", got)
		}
	}
	if got := testGit(t, a, "worktree", "list", "--porcelain"); strings.Contains(got, b) {
		t.Fatalf("session A enumerates session B worktree: %s", got)
	}
}

func TestPoolSuccessfulVerificationRenewsFreshness(t *testing.T) {
	remote := testRemote(t)
	root := t.TempDir()
	poolRoot := filepath.Join(root, "pool")
	key := SourceKey(remote)
	if err := PrepareObjectPool(context.Background(), poolRoot, key, remote, true); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(poolRoot, key, "current.json")
	old := time.Now().Add(-2 * poolRefreshInterval)
	if err := os.Chtimes(current, old, old); err != nil {
		t.Fatal(err)
	}
	if err := PrepareObjectPool(context.Background(), poolRoot, key, remote, true); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(current); err != nil || time.Since(info.ModTime()) >= poolRefreshInterval {
		t.Fatalf("successful verification did not renew current record: info=%v err=%v", info, err)
	}
	if err := os.RemoveAll(remote); err != nil {
		t.Fatal(err)
	}
	if _, err := objectPoolForCheckout(context.Background(), poolRoot, key, remote, true); err != nil {
		t.Fatalf("fresh verified pool unexpectedly contacted removed source: %v", err)
	}
}

func TestCompatibilityInventoryDoesNotDuplicatePoolObjects(t *testing.T) {
	remote := testRemote(t)
	root := t.TempDir()
	key := SourceKey(remote)
	if err := PrepareObjectPool(context.Background(), filepath.Join(root, "pool"), key, remote, true); err != nil {
		t.Fatal(err)
	}
	inventory := filepath.Join(root, "inventory.git")
	if err := InitBareInventory(context.Background(), remote, inventory); err != nil {
		t.Fatal(err)
	}
	if got := objectDataBytes(t, filepath.Join(inventory, "objects")); got != 0 {
		t.Fatalf("compatibility inventory duplicated %d pool object bytes", got)
	}
	if refs := testGit(t, "", "--git-dir", inventory, "for-each-ref", "--format=%(refname)"); refs != "" {
		t.Fatalf("compatibility inventory fetched refs: %s", refs)
	}
}

func TestPoolFastForwardAddsDeltaGenerationAndRetainsOld(t *testing.T) {
	remote := testRemote(t)
	root := t.TempDir()
	key := SourceKey(remote)
	a, oldLayout := pooledTestCheckout(t, root, remote, key, "a")
	oldOID := testGit(t, a, "rev-parse", "HEAD")

	writer := filepath.Join(root, "writer")
	testGit(t, "", "clone", "-q", remote, writer)
	if err := os.WriteFile(filepath.Join(writer, "delta.bin"), []byte(strings.Repeat("delta", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, writer, "add", "delta.bin")
	testGit(t, writer, "commit", "-q", "-m", "fast forward")
	newOID := testGit(t, writer, "rev-parse", "HEAD")
	testGit(t, writer, "push", "-q", "origin", "main")
	if err := PrepareObjectPool(context.Background(), filepath.Join(root, "pool"), key, remote, true); err != nil {
		t.Fatal(err)
	}

	b, newLayout := pooledTestCheckout(t, root, remote, key, "b")
	if len(newLayout.ObjectMounts) != 2 {
		t.Fatalf("fast-forward closure has %d generations, want 2: %+v", len(newLayout.ObjectMounts), newLayout.ObjectMounts)
	}
	if newLayout.ObjectMounts[0] != oldLayout.ObjectMounts[0] {
		t.Fatal("fast-forward generation did not reuse immutable predecessor")
	}
	deltaObjects := newLayout.ObjectMounts[1].ObjectsHostDir
	if directObjectPresent(t, deltaObjects, oldOID) {
		t.Fatalf("fast-forward generation duplicated predecessor commit %s", oldOID)
	}
	if !directObjectPresent(t, deltaObjects, newOID) {
		t.Fatalf("fast-forward generation does not contain new commit %s", newOID)
	}
	if got := testGit(t, b, "rev-parse", "HEAD"); got != newOID {
		t.Fatalf("new worktree base = %s, want %s", got, newOID)
	}
	if err := ValidateCheckoutLayout(filepath.Join(root, "a.json"), a, root); err != nil {
		t.Fatalf("old retained generation stopped serving existing worktree: %v", err)
	}
}

func TestPoolNonFastForwardStartsNewLineageAndRetainsOld(t *testing.T) {
	remote := testRemote(t)
	root := t.TempDir()
	key := SourceKey(remote)
	a, oldLayout := pooledTestCheckout(t, root, remote, key, "a")
	oldOID := testGit(t, a, "rev-parse", "HEAD")
	writer := filepath.Join(root, "writer")
	testGit(t, "", "clone", "-q", remote, writer)
	testGit(t, writer, "checkout", "-q", "--orphan", "replacement")
	if err := os.Remove(filepath.Join(writer, "README.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(writer, "replacement"), []byte("new root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, writer, "add", "-A")
	testGit(t, writer, "commit", "-q", "-m", "replacement root")
	testGit(t, writer, "push", "-q", "--force", "origin", "HEAD:main")
	if err := PrepareObjectPool(context.Background(), filepath.Join(root, "pool"), key, remote, true); err != nil {
		t.Fatal(err)
	}

	b, newLayout := pooledTestCheckout(t, root, remote, key, "b")
	if len(newLayout.ObjectMounts) != 1 {
		t.Fatalf("non-fast-forward session inherited old lineage: %+v", newLayout.ObjectMounts)
	}
	assertObjectMissing(t, b, oldOID)
	if _, err := os.Stat(oldLayout.ObjectMounts[0].ObjectsHostDir); err != nil {
		t.Fatalf("referenced old generation was not retained: %v", err)
	}
	if err := ValidateCheckoutLayout(filepath.Join(root, "a.json"), a, root); err != nil {
		t.Fatalf("old worktree lost retained generation: %v", err)
	}
}

func TestPoolRollbackSelectsCachedLineageWithoutRemovedIntermediate(t *testing.T) {
	remote := testRemote(t)
	root := t.TempDir()
	key := SourceKey(remote)
	a, first := pooledTestCheckout(t, root, remote, key, "a")
	oidA := testGit(t, a, "rev-parse", "HEAD")
	writer := filepath.Join(root, "writer")
	testGit(t, "", "clone", "-q", remote, writer)
	testGit(t, writer, "commit", "-q", "--allow-empty", "-m", "B")
	oidB := testGit(t, writer, "rev-parse", "HEAD")
	testGit(t, writer, "push", "-q", "origin", "main")
	if err := PrepareObjectPool(context.Background(), filepath.Join(root, "pool"), key, remote, true); err != nil {
		t.Fatal(err)
	}
	testGit(t, writer, "reset", "-q", "--hard", oidA)
	testGit(t, writer, "push", "-q", "--force", "origin", "HEAD:main")
	if err := PrepareObjectPool(context.Background(), filepath.Join(root, "pool"), key, remote, true); err != nil {
		t.Fatal(err)
	}
	rolledBack, layout := pooledTestCheckout(t, root, remote, key, "rollback")
	if len(layout.ObjectMounts) != len(first.ObjectMounts) || layout.ObjectMounts[0] != first.ObjectMounts[0] {
		t.Fatalf("rollback did not select cached A lineage: first=%+v rollback=%+v", first.ObjectMounts, layout.ObjectMounts)
	}
	if got := testGit(t, rolledBack, "rev-parse", "HEAD"); got != oidA {
		t.Fatalf("rollback base = %s, want A %s", got, oidA)
	}
	assertObjectMissing(t, rolledBack, oidB)
}

func TestPoolManyFastForwardsUseFlatAlternates(t *testing.T) {
	remote := testRemote(t)
	root := t.TempDir()
	key := SourceKey(remote)
	pooledTestCheckout(t, root, remote, key, "initial")
	writer := filepath.Join(root, "writer")
	testGit(t, "", "clone", "-q", remote, writer)
	const updates = 12
	for i := 0; i < updates; i++ {
		testGit(t, writer, "commit", "-q", "--allow-empty", "-m", "advance "+strconv.Itoa(i))
		testGit(t, writer, "push", "-q", "origin", "main")
		if err := PrepareObjectPool(context.Background(), filepath.Join(root, "pool"), key, remote, true); err != nil {
			t.Fatalf("refresh %d: %v", i, err)
		}
	}
	checkout, layout := pooledTestCheckout(t, root, remote, key, "latest")
	if len(layout.ObjectMounts) != updates+1 {
		t.Fatalf("generation closure has %d entries, want %d", len(layout.ObjectMounts), updates+1)
	}
	for _, mount := range layout.ObjectMounts {
		if _, err := os.Lstat(filepath.Join(mount.ObjectsHostDir, "info", "alternates")); !os.IsNotExist(err) {
			t.Fatalf("pool generation %s retained recursive alternate: %v", mount.Generation, err)
		}
	}
	alternate, err := os.ReadFile(filepath.Join(layout.CommonDir, "objects", "info", "alternates"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(alternate)), "\n")); got != updates+1 {
		t.Fatalf("private common has %d flat alternates, want %d", got, updates+1)
	}
	if got := testGit(t, checkout, "rev-list", "--count", "HEAD"); got != strconv.Itoa(updates+1) {
		t.Fatalf("history through >8 generations has %s commits, want %d", got, updates+1)
	}
	testGit(t, checkout, "fsck", "--connectivity-only")
}

func TestPoolSourceChangeStartsNewLineage(t *testing.T) {
	root := t.TempDir()
	first := testRemote(t)
	second := testRemote(t)
	key := SourceKey("stable tracked repository identity")
	one, err := ensureObjectPool(context.Background(), filepath.Join(root, "pool"), key, first, true)
	if err != nil {
		t.Fatal(err)
	}
	two, err := ensureObjectPool(context.Background(), filepath.Join(root, "pool"), key, second, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(one.Mounts) != 1 || len(two.Mounts) != 1 || one.Mounts[0] == two.Mounts[0] {
		t.Fatalf("source change reused lineage: first=%+v second=%+v", one.Mounts, two.Mounts)
	}
}

func TestPoolDefaultBranchChangeStartsNewLineage(t *testing.T) {
	remote := testRemote(t)
	root := t.TempDir()
	key := SourceKey(remote)
	_, first := pooledTestCheckout(t, root, remote, key, "a")
	writer := filepath.Join(root, "writer")
	testGit(t, "", "clone", "-q", remote, writer)
	testGit(t, writer, "checkout", "-q", "-b", "trunk")
	testGit(t, writer, "commit", "-q", "--allow-empty", "-m", "new default")
	testGit(t, writer, "push", "-q", "origin", "trunk")
	testGit(t, "", "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
	if err := PrepareObjectPool(context.Background(), filepath.Join(root, "pool"), key, remote, true); err != nil {
		t.Fatal(err)
	}

	_, second := pooledTestCheckout(t, root, remote, key, "b")
	if len(first.ObjectMounts) != 1 || len(second.ObjectMounts) != 1 || first.ObjectMounts[0] == second.ObjectMounts[0] {
		t.Fatalf("default-branch change reused lineage: first=%+v second=%+v", first.ObjectMounts, second.ObjectMounts)
	}
}

func TestPoolRefreshNeverRunsSessionGitConfiguration(t *testing.T) {
	remote := testRemote(t)
	root := t.TempDir()
	key := SourceKey(remote)
	a, _ := pooledTestCheckout(t, root, remote, key, "a")
	marker := filepath.Join(root, "executed")
	helper := filepath.Join(root, "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf invoked >\"$AMUX_TEST_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	included := filepath.Join(root, "included.gitconfig")
	if err := os.WriteFile(included, []byte("[core]\n\tfsmonitor = "+helper+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, kv := range [][2]string{
		{"core.fsmonitor", helper},
		{"core.hooksPath", filepath.Dir(helper)},
		{"credential.helper", "!" + helper},
		{"uploadpack.packObjectsHook", helper},
		{"include.path", included},
	} {
		testGit(t, a, "config", "--local", kv[0], kv[1])
	}
	t.Setenv("AMUX_TEST_MARKER", marker)
	writer := filepath.Join(root, "writer")
	testGit(t, "", "clone", "-q", remote, writer)
	testGit(t, writer, "commit", "-q", "--allow-empty", "-m", "advance")
	testGit(t, writer, "push", "-q", "origin", "main")
	if err := PrepareObjectPool(context.Background(), filepath.Join(root, "pool"), key, remote, true); err != nil {
		t.Fatal(err)
	}
	pooledTestCheckout(t, root, remote, key, "b")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("host pool operation executed session Git configuration: %v", err)
	}
}

func TestLayoutValidationRejectsAlternateAndGitdirTampering(t *testing.T) {
	remote := testRemote(t)
	root := t.TempDir()
	checkout, layout := pooledTestCheckout(t, root, remote, SourceKey(remote), "a")
	layoutPath := filepath.Join(root, "a.json")
	alternate := filepath.Join(layout.CommonDir, "objects", "info", "alternates")
	if err := os.WriteFile(alternate, []byte(filepath.Join(root, "sibling-objects")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCheckoutLayout(layoutPath, checkout, root); err == nil {
		t.Fatal("trusted layout accepted a session-repointed alternate")
	}
	if err := os.WriteFile(alternate, []byte(layout.ObjectMounts[len(layout.ObjectMounts)-1].ObjectsMountDir+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".git"), []byte("gitdir: "+filepath.Join(root, "sibling.git")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCheckoutLayout(layoutPath, checkout, root); err == nil {
		t.Fatal("trusted layout accepted a session-repointed .git file")
	}
}

func TestRecordlessPrivateCloneCompatibilityIsConsistent(t *testing.T) {
	root := t.TempDir()
	private := filepath.Join(root, "private")
	testGit(t, "", "init", "-q", private)
	missingRecord := filepath.Join(root, "missing.json")
	if err := ValidateCheckoutLayout(missingRecord, private, root); err != nil {
		t.Fatalf("recordless private clone validation: %v", err)
	}
	if mounts, err := ReadObjectMounts(missingRecord, private, root); err != nil || len(mounts) != 0 {
		t.Fatalf("recordless private clone mounts = %+v, err=%v", mounts, err)
	}
	linked := filepath.Join(root, "linked")
	if err := os.Mkdir(linked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: /tmp/shared.git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCheckoutLayout(missingRecord, linked, root); err == nil {
		t.Fatal("recordless linked worktree unexpectedly passed validation")
	}
	if _, err := ReadObjectMounts(missingRecord, linked, root); err == nil {
		t.Fatal("recordless linked worktree unexpectedly resolved mounts")
	}
}

func TestPoolLocalSourceRequiresExplicitTrust(t *testing.T) {
	remote := testRemote(t)
	_, err := ensureObjectPool(context.Background(), filepath.Join(t.TempDir(), "pool"), SourceKey(remote), remote, false)
	if err == nil || !strings.Contains(err.Error(), "explicit host trust") {
		t.Fatalf("local source denial = %v", err)
	}
}

// BenchmarkWorktreeCreationAgainstLegacy compares steady-state creation after
// both backing stores exist. object-xfer_B/op is object payload copied or
// fetched into the per-session common directory; base-pack-dup_B/op is base
// pack storage duplicated there. Both should remain zero for the pooled layout,
// matching the storage behavior of the old shared-common worktree.
func BenchmarkWorktreeCreationAgainstLegacy(b *testing.B) {
	remote := testRemote(b)
	root := b.TempDir()
	legacy := filepath.Join(root, "legacy.git")
	testGit(b, "", "clone", "-q", "--bare", remote, legacy)
	legacyBaseBytes := objectDataBytes(b, filepath.Join(legacy, "objects"))

	b.Run("legacy-shared-common", func(b *testing.B) {
		var elapsed time.Duration
		for i := 0; i < b.N; i++ {
			parent, err := os.MkdirTemp(root, "legacy-")
			if err != nil {
				b.Fatal(err)
			}
			path := filepath.Join(parent, "repo")
			branch := "amux/legacy-" + filepath.Base(parent) + "-" + strconv.Itoa(i)
			start := time.Now()
			testGit(b, "", "--git-dir", legacy, "worktree", "add", "-q", "-b", branch, path, "main")
			elapsed += time.Since(start)
		}
		b.ReportMetric(0, "object-xfer_B/op")
		b.ReportMetric(0, "base-pack-dup_B/op")
		b.ReportMetric(float64(legacyBaseBytes), "shared-base_B")
		b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N), "create-ns/op")
	})

	b.Run("pooled-private-common", func(b *testing.B) {
		poolRoot := filepath.Join(root, "pool")
		key := SourceKey(remote)
		pool, err := ensureObjectPool(context.Background(), poolRoot, key, remote, true)
		if err != nil {
			b.Fatal(err)
		}
		var sharedBaseBytes int64
		for _, mount := range pool.Mounts {
			sharedBaseBytes += objectDataBytes(b, mount.ObjectsHostDir)
		}
		b.ResetTimer()
		var elapsed time.Duration
		var privateObjectBytes int64
		for i := 0; i < b.N; i++ {
			agent, err := os.MkdirTemp(root, "pooled-")
			if err != nil {
				b.Fatal(err)
			}
			stamp := filepath.Base(agent) + "-" + strconv.Itoa(i)
			layoutPath := filepath.Join(root, "layout-"+stamp+".json")
			start := time.Now()
			if err := AddCheckout(context.Background(), CheckoutRequest{
				Source: remote, Path: filepath.Join(agent, "repo"), Branch: "amux/pooled-" + stamp,
				RepoKey: key, PoolRoot: poolRoot, StagingRoot: filepath.Join(root, "staging"),
				ManagedRoot: root, LayoutPath: layoutPath, AllowLocalSource: true,
			}); err != nil {
				b.Fatal(err)
			}
			elapsed += time.Since(start)
			layout := loadTestLayout(b, layoutPath)
			privateObjectBytes += objectDataBytes(b, filepath.Join(layout.CommonDir, "objects"))
		}
		b.StopTimer()
		perOp := float64(privateObjectBytes) / float64(b.N)
		b.ReportMetric(perOp, "object-xfer_B/op")
		b.ReportMetric(perOp, "base-pack-dup_B/op")
		b.ReportMetric(float64(sharedBaseBytes), "shared-base_B")
		b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N), "create-ns/op")
	})
}
