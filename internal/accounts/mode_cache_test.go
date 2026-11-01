package accounts

import (
	"os"
	"testing"
	"time"

	"amux/internal/amuxcfg"
)

// Mode caches the decoded store, but any replacement or permission change of
// the file must still go through load's validation.
func TestModeCacheRevalidatesChangedStore(t *testing.T) {
	isolated(t)
	if err := Bind("a1", "claude"); err != nil {
		t.Fatal(err)
	}
	if got := Mode("a1", "claude"); got != amuxcfg.AuthCentral {
		t.Fatalf("Mode = %q", got)
	}
	if got := Mode("a1", "claude"); got != amuxcfg.AuthCentral {
		t.Fatalf("cached Mode = %q", got)
	}
	if err := os.Chmod(Path(), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Mode("a1", "claude"); got != "blocked" {
		t.Fatalf("Mode over an insecure store = %q, want blocked", got)
	}
	if err := os.Chmod(Path(), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Mode("a1", "claude"); got != amuxcfg.AuthCentral {
		t.Fatalf("Mode after repair = %q", got)
	}
	// A replaced (renamed-over) store is a new file: corrupt content blocks.
	tmp := Path() + ".tmp"
	if err := os.WriteFile(tmp, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, Path()); err != nil {
		t.Fatal(err)
	}
	if got := Mode("a1", "claude"); got != "blocked" {
		t.Fatalf("Mode over a corrupt store = %q, want blocked", got)
	}
}

// An in-place rewrite that keeps size and mtime (as on a coarse-mtime
// filesystem) still moves the change time, so the cache misses.
func TestSameStoreFileComparesChangeTime(t *testing.T) {
	isolated(t)
	if err := Bind("a1", "claude"); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(Path())
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(Path(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(Path(), before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(Path())
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || !os.SameFile(before, after) {
		t.Fatal("fixture did not preserve inode, size and mtime")
	}
	if sameStoreFile(before, after) {
		t.Fatal("an in-place rewrite with the same size and mtime hit the cache")
	}
	if !sameStoreFile(after, after) {
		t.Fatal("an unchanged file missed the cache")
	}
}
