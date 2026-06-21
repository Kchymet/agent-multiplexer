package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHookStateRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if _, ok := HookState("missing"); ok {
		t.Fatal("unknown session should report no state")
	}
	if err := WriteHookState("", "running", "/x"); err != nil {
		t.Fatalf("blank id: %v", err)
	}
	if err := WriteHookState("uuid-1", "running", "/home/u/proj"); err != nil {
		t.Fatal(err)
	}

	rec, ok := HookState("uuid-1")
	if !ok || rec.State != "running" || rec.Cwd != "/home/u/proj" || rec.Updated == 0 {
		t.Fatalf("round-trip mismatch: %+v ok=%v", rec, ok)
	}

	all := AllHookStates()
	if _, ok := all["uuid-1"]; !ok {
		t.Fatalf("AllHookStates missing uuid-1: %v", all)
	}
	if _, ok := all[""]; ok {
		t.Fatal("AllHookStates should not contain a blank-id record")
	}

	// A bare state word (older format) is still readable.
	if err := os.WriteFile(filepath.Join(HookStateDir(), "legacy"), []byte("ready\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rec, ok := HookState("legacy"); !ok || rec.State != "ready" {
		t.Fatalf("legacy bare-word record: %+v ok=%v", rec, ok)
	}
}
