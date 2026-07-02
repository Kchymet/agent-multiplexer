package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallWritesLibrary checks that Install materializes every embedded skill
// into the destination dir with its SKILL.md intact.
func TestInstallWritesLibrary(t *testing.T) {
	dest := t.TempDir()
	if err := Install(dest); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Every skill in the embedded library must land as a SKILL.md on disk.
	for _, skill := range []string{"create-pr", "babysit-pr"} {
		p := filepath.Join(dest, skill, "SKILL.md")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("expected %s: %v", p, err)
			continue
		}
		if !strings.Contains(string(b), "name: "+skill) {
			t.Errorf("%s missing frontmatter name: %q", p, skill)
		}
	}
}

// TestInstallIsIdempotent checks that re-running Install (as happens on every
// launch, tracking the binary) overwrites rather than errors or duplicates.
func TestInstallIsIdempotent(t *testing.T) {
	dest := t.TempDir()
	if err := Install(dest); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	// A stale edit from a previous binary must be replaced by the embedded copy.
	target := filepath.Join(dest, "create-pr", "SKILL.md")
	if err := os.WriteFile(target, []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	if err := Install(dest); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read after reinstall: %v", err)
	}
	if string(b) == "stale" {
		t.Errorf("Install did not overwrite stale skill content")
	}
}

// TestInstallLeavesForeignFilesAlone checks Install only writes the skills it
// ships: a user's own skill sitting in the same destination survives untouched.
func TestInstallLeavesForeignFilesAlone(t *testing.T) {
	dest := t.TempDir()
	userSkill := filepath.Join(dest, "my-own", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(userSkill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userSkill, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Install(dest); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if b, _ := os.ReadFile(userSkill); string(b) != "mine" {
		t.Errorf("Install clobbered a user's own skill")
	}
}
