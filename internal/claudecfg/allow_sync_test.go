package claudecfg

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"amux/internal/cfghome"
)

func readAllow(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return allowRules(b)
}

// A permissions.allow rule the user adds to ~/.claude/settings.json after an
// agent was seeded reaches that existing agent at its next launch (Seed on an
// existing home). An unedited copy is refreshed wholesale and stays clean in
// drift; an edited copy gains only the missing rule and keeps its own edits.
func TestUserAllowRulesReachExistingSandboxes(t *testing.T) {
	isolateAuth(t)
	user := User().Dir
	writeSettingsFile(t, filepath.Join(user, "settings.json"), `{"permissions":{"allow":["Bash(gh pr view:*)"]}}`)

	clean := Template("clean", t.TempDir())
	edited := Template("edited", t.TempDir())
	for _, sp := range []cfghome.Spec{clean, edited} {
		if _, err := cfghome.Seed(sp); err != nil {
			t.Fatal(err)
		}
	}
	// The "edited" agent changed its own settings (a local rule and a model).
	writeSettingsFile(t, filepath.Join(edited.Dir, "settings.json"),
		`{"model":"opus","permissions":{"allow":["Bash(make test)","Bash(gh pr view:*)"]}}`)

	// The user adds the rule a coordinator needs for self-archive.
	writeSettingsFile(t, filepath.Join(user, "settings.json"),
		`{"permissions":{"allow":["Bash(gh pr view:*)","Bash(amux agent done)"]}}`)
	for _, sp := range []cfghome.Spec{clean, edited} {
		if fresh, err := cfghome.Seed(sp); err != nil || fresh {
			t.Fatalf("relaunch seed of %s: fresh=%v err=%v", sp.AgentID, fresh, err)
		}
	}

	if got, want := readAllow(t, filepath.Join(clean.Dir, "settings.json")), []string{"Bash(gh pr view:*)", "Bash(amux agent done)"}; !slices.Equal(got, want) {
		t.Fatalf("clean copy allow = %v, want %v", got, want)
	}
	if changes, err := cfghome.Scan(clean); err != nil || len(changes) != 0 {
		t.Fatalf("refreshed clean copy reports drift: %v %v", changes, err)
	}

	got := readAllow(t, filepath.Join(edited.Dir, "settings.json"))
	if want := []string{"Bash(make test)", "Bash(gh pr view:*)", "Bash(amux agent done)"}; !slices.Equal(got, want) {
		t.Fatalf("edited copy allow = %v, want %v (own rules first, missing user rule appended)", got, want)
	}
	b, _ := os.ReadFile(filepath.Join(edited.Dir, "settings.json"))
	if root, ok := decodeObject(b); !ok || root["model"] != "opus" {
		t.Fatalf("edited copy lost its own settings: %s", b)
	}
	// Idempotent: another launch changes nothing.
	if _, err := cfghome.Seed(edited); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(edited.Dir, "settings.json")); string(again) != string(b) {
		t.Fatalf("second sync rewrote the copy:\n%s\nvs\n%s", again, b)
	}
}

// mergeAllowRules never clobbers a copy it cannot read as settings.
func TestMergeAllowRulesLeavesUnparseableCopies(t *testing.T) {
	tmpl := []byte(`{"permissions":{"allow":["Bash(amux agent done)"]}}`)
	for _, cp := range []string{`not json`, `{"permissions":"weird"}`, `{"permissions":{"allow":"weird"}}`} {
		if _, ok := mergeAllowRules(cfghome.Spec{}, []byte(cp), tmpl); ok {
			t.Errorf("merged into %q", cp)
		}
	}
	if _, ok := mergeAllowRules(cfghome.Spec{}, []byte(`{"permissions":{"allow":["Bash(amux agent done)"]}}`), tmpl); ok {
		t.Error("reported a merge when nothing was missing")
	}
}
