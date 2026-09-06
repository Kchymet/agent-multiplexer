package panespec

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolvedInstallRoot covers the AGE-181 provisioning fix: a launcher symlink
// into a standalone package in a different home subtree (codex:
// ~/.local/bin/codex → ~/.codex/packages/standalone/<ver>/bin/codex) resolves to
// the real binary and the package install root to bind — narrowly, never the whole
// ~/.codex — while a plain binary and a same-subtree symlink are left alone.
func TestResolvedInstallRoot(t *testing.T) {
	home := t.TempDir()

	// Production-shaped layout: launcher under ~/.local, target under ~/.codex.
	pkg := filepath.Join(home, ".codex", "packages", "standalone", "0.153.4")
	realBin := filepath.Join(pkg, "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(realBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(realBin, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	launcherDir := filepath.Join(home, ".local", "bin")
	if err := os.MkdirAll(launcherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(launcherDir, "codex")
	if err := os.Symlink(realBin, launcher); err != nil {
		t.Fatal(err)
	}

	real, root := resolvedInstallRoot(home, launcher)
	wantReal, _ := filepath.EvalSymlinks(realBin)
	if real != wantReal {
		t.Errorf("real = %q, want %q", real, wantReal)
	}
	if root != pkg {
		t.Errorf("root = %q, want the package dir %q (parent of bin/), never all of ~/.codex", root, pkg)
	}
	// Crucially, the bound root must NOT be the whole ~/.codex (config/auth/sessions).
	if root == filepath.Join(home, ".codex") {
		t.Fatal("install root is the whole ~/.codex — would expose config/auth/sessions")
	}

	// A plain binary (no symlink) → no extra bind.
	plain := filepath.Join(home, ".nvm", "bin", "claude")
	if err := os.MkdirAll(filepath.Dir(plain), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plain, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if r, root := resolvedInstallRoot(home, plain); root != "" || r != "" {
		t.Errorf("plain binary should need no extra bind, got real=%q root=%q", r, root)
	}

	// A SAME-subtree symlink (already covered by the homeSubtree bind) → no extra bind.
	sameTarget := filepath.Join(home, ".nvm", "versions", "codex")
	if err := os.MkdirAll(filepath.Dir(sameTarget), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sameTarget, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	sameLink := filepath.Join(home, ".nvm", "bin", "codexlink")
	if err := os.Symlink(sameTarget, sameLink); err != nil {
		t.Fatal(err)
	}
	if _, root := resolvedInstallRoot(home, sameLink); root != "" {
		t.Errorf("same-subtree symlink should need no extra bind, got root=%q", root)
	}
}

// A nonstandard install must not turn a launcher into a bind of all host
// configuration. Only the binary is needed when its parent would be too broad.
func TestResolvedInstallRootDoesNotExposeHomeOrConfig(t *testing.T) {
	for _, relative := range []string{".codex/codex", ".codex/bin/codex", "bin/codex"} {
		t.Run(relative, func(t *testing.T) {
			home := t.TempDir()
			target := filepath.Join(home, relative)
			launcher := filepath.Join(home, ".local", "bin", "codex")
			for _, p := range []string{target, launcher} {
				if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(target, []byte("executable"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, launcher); err != nil {
				t.Fatal(err)
			}
			real, root := resolvedInstallRoot(home, launcher)
			if real != target || root != target {
				t.Fatalf("non-package target must bind only executable: got real=%q root=%q, want %q", real, root, target)
			}
		})
	}
}
