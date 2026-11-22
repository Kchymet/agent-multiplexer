package selfupdate

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReplaceFileAtomicallySwapsContentAndMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "amux")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A process holding the old file keeps reading the old inode.
	held, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	src := filepath.Join(t.TempDir(), "new")
	if err := os.WriteFile(src, []byte("new binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(target, src, 0o755, digest("new binary")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(target)
	if string(b) != "new binary" {
		t.Fatalf("target = %q", b)
	}
	info, _ := os.Stat(target)
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
	}
	old := make([]byte, 3)
	if _, err := held.ReadAt(old, 0); err != nil || string(old) != "old" {
		t.Fatalf("held handle reads %q (%v), want the old inode", old, err)
	}
	assertNoTemps(t, dir)
}

func TestReplaceFileChecksumMismatchLeavesTargetUntouched(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "amux")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "new")
	if err := os.WriteFile(src, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ReplaceFile(target, src, 0o755, digest("expected"))
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "old" {
		t.Fatalf("target changed to %q after a failed verify", b)
	}
	assertNoTemps(t, dir)
}

func TestReplaceFileKeepsSymlinkResolvedTarget(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real-amux")
	link := filepath.Join(dir, "amux")
	if err := os.WriteFile(real, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(resolved, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(link); string(b) != "new" {
		t.Fatalf("through link = %q", b)
	}
}

func TestWritable(t *testing.T) {
	dir := t.TempDir()
	if err := Writable(filepath.Join(dir, "amux")); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ro := filepath.Join(dir, "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if err := Writable(filepath.Join(ro, "amux")); err == nil {
		t.Fatal("read-only directory reported writable")
	}
}

func TestProbeVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fixture")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	if err := os.WriteFile(good, []byte("#!/bin/sh\n[ \"$1 $2\" = \"version --short\" ] && echo 0.1.42\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if v, err := ProbeVersion(t.Context(), good); err != nil || v != "0.1.42" {
		t.Fatalf("ProbeVersion = %q, %v", v, err)
	}
	// An old binary that ignores --short prints the multi-line report.
	old := filepath.Join(dir, "old")
	if err := os.WriteFile(old, []byte("#!/bin/sh\necho 'cli       0.1.0 (protocol 2)'\necho 'daemon    offline'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if v, err := ProbeVersion(t.Context(), old); err == nil {
		t.Fatalf("multi-line output accepted as version %q", v)
	}
	if _, err := ProbeVersion(t.Context(), filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing binary probed")
	}
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".update-") {
			t.Fatalf("temporary file left behind: %s", e.Name())
		}
	}
}
