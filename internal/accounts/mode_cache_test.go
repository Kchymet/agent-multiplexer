package accounts

import (
	"os"
	"testing"

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
