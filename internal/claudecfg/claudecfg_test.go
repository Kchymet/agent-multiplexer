package claudecfg

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSessionLookup verifies the path munging matches Claude Code's project-dir
// naming ('/' and '.' -> '-') and that session detection works.
func TestSessionLookup(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)

	cwd := "/home/u/.local/share/amux/workspaces/abc123"
	const want = "-home-u--local-share-amux-workspaces-abc123" // mirrors real claude munging
	proj := filepath.Join(dir, "projects", want)
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	uuid := "11111111-1111-4111-8111-111111111111"

	if SessionExists(cwd, uuid) {
		t.Fatal("session should not exist before the file is written")
	}
	if AnySession(cwd) {
		t.Fatal("AnySession should be false initially")
	}
	if err := os.WriteFile(filepath.Join(proj, uuid+".jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !SessionExists(cwd, uuid) {
		t.Fatal("session should exist after writing the file (munge mismatch?)")
	}
	if !AnySession(cwd) {
		t.Fatal("AnySession should be true after writing a .jsonl")
	}
}
