package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"amux/internal/core"
)

func TestValidateStoredAgentDirAllowsMovedAgentWithoutDerivingRootID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	stored := filepath.Join(core.SessionsDir(), "old-root", "agent-a")
	if err := os.MkdirAll(stored, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateStoredAgentDir(stored); err != nil {
		t.Fatalf("stored pre-move directory rejected: %v", err)
	}
}

func TestValidateStoredAgentDirRejectsContainersCoordinatorsAndSymlinks(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	root := filepath.Join(core.SessionsDir(), "wg")
	coordinator := filepath.Join(root, "coordinator")
	outside := filepath.Join(t.TempDir(), "outside")
	for _, dir := range []string{coordinator, outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(root, "linked-agent")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"container":   root,
		"coordinator": coordinator,
		"symlink":     link,
		"outside":     outside,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateStoredAgentDir(dir); err == nil {
				t.Fatalf("unsafe directory %q accepted", dir)
			}
		})
	}
}
