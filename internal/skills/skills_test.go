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

// Installed coordinator guidance must separate archive confirmation from task
// completion, even when the same skill lets task-mode members retire themselves.
func TestInstalledCreatePRRequiresCoordinatorArchiveConfirmation(t *testing.T) {
	dest := t.TempDir()
	if err := Install(dest); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dest, "create-pr", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(b)), " ")
	for _, want := range []string{
		"after explicit user confirmation",
		"goal completion and a merged PR are not confirmation",
		"`amux agent done`",
		"archives its own workgroup and all its direct members",
		"Restoration is host-only",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("installed coordinator policy missing %q", want)
		}
	}
	if strings.Contains(text, "coordinator **never** archives") {
		t.Fatal("installed skill still prohibits confirmed coordinator archive")
	}
}

// The shipped PR skills must end the lifecycle with the PR marked ready for
// review: agents had read "the user merges" as "keep the PR draft".
func TestInstalledPRSkillsMarkPRReady(t *testing.T) {
	dest := t.TempDir()
	if err := Install(dest); err != nil {
		t.Fatal(err)
	}
	for skill, wants := range map[string][]string{
		"create-pr": {
			"**Mark it ready for review** — `gh pr ready <n>` — as soon as the code is complete and CI is green",
			"**Keep babysitting after it's ready**",
			"**The final step is the merge.**",
			"It **never** means \"keep the PR draft\"",
			"open, green, **ready-for-review (non-draft) PR**",
		},
		"babysit-pr": {
			"**`gh pr ready <n>`** and report it ready for review → keep babysitting",
			"**merge, the final step**",
			"Babysitting is done when **the PR is merged**",
			"it does **not** mean \"keep the PR draft\"",
			"confirming it is marked ready for review — not draft",
		},
	} {
		b, err := os.ReadFile(filepath.Join(dest, skill, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		text := strings.Join(strings.Fields(string(b)), " ")
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing %q", skill, want)
			}
		}
	}
}
