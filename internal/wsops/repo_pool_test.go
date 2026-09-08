package wsops

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/git"
)

func TestAddRepoSourceBuildsPoolWithoutDuplicatingInventoryObjects(t *testing.T) {
	isolateStore(t)
	t.Setenv("AMUX_GIT_TRUST_LOCAL_SOURCE", "1")
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, source, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(source, "README"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, source, "add", "README")
	gitRun(t, source, "commit", "-q", "-m", "base")

	repo, err := AddRepoSource(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "--git-dir", repo.GitDir, "for-each-ref", "--format=%(refname)")
	if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("compatibility inventory fetched refs or failed: %q, %v", out, err)
	}
	var objectBytes int64
	err = filepath.Walk(filepath.Join(repo.GitDir, "objects"), func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			objectBytes += info.Size()
		}
		return nil
	})
	if err != nil || objectBytes != 0 {
		t.Fatalf("compatibility inventory object bytes = %d, err=%v", objectBytes, err)
	}
	canonical, err := filepath.EvalSymlinks(source)
	if err != nil {
		t.Fatal(err)
	}
	manifests, err := filepath.Glob(filepath.Join(core.StateDir(), "git-pools", "v1", git.SourceKey(canonical), "*", "manifest.json"))
	if err != nil || len(manifests) != 1 {
		t.Fatalf("pool manifests = %v, err=%v", manifests, err)
	}
}

func TestAuthoritativeSourceRejectsLegacyCacheThroughFileURL(t *testing.T) {
	isolateStore(t)
	t.Setenv("AMUX_GIT_TRUST_LOCAL_SOURCE", "1")
	legacy := filepath.Join(core.ReposDir(), "legacy.git")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := authoritativeSource("file://" + legacy); err == nil || !strings.Contains(err.Error(), "legacy amux bare cache") {
		t.Fatalf("file URL legacy-cache source error = %v", err)
	}
}
