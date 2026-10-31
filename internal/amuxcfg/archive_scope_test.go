package amuxcfg

import (
	"os"
	"testing"

	"amux/internal/core"
)

func TestCoordinatorArchiveScopeDefaultsAndRoundTrip(t *testing.T) {
	isolate(t)
	if v, err := CoordinatorArchiveScope(); err != nil || v != ArchiveScopeMembers || HostArchiveScope() {
		t.Fatalf("default = %q, %v; want members", v, err)
	}
	if err := SetCoordinatorArchiveScope("host"); err != nil {
		t.Fatal(err)
	}
	if !HostArchiveScope() {
		t.Fatal("host scope not effective after set")
	}
	if err := SetCoordinatorArchiveScope("everything"); err == nil {
		t.Fatal("invalid value accepted")
	}
	if err := SetCoordinatorArchiveScope(""); err != nil {
		t.Fatal(err)
	}
	if saved, err := SavedCoordinatorArchiveScope(); err != nil || saved != "" || HostArchiveScope() {
		t.Fatalf("after unset saved=%q err=%v", saved, err)
	}
}

func TestCoordinatorArchiveScopeMalformedFailsClosed(t *testing.T) {
	isolate(t)
	if err := os.MkdirAll(core.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(core.ConfigPath(), []byte(`{"coordinator":{"archive_scope":"HOST!"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := CoordinatorArchiveScope(); err == nil || v != ArchiveScopeMembers {
		t.Fatalf("malformed = %q, %v; want members with error", v, err)
	}
	if HostArchiveScope() {
		t.Fatal("malformed config widened authority")
	}
}
