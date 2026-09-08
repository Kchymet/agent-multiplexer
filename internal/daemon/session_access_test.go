package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"amux/internal/access"
	"amux/internal/console"
	"amux/internal/core"
)

func TestSessionAccessMaterializesConsoleOnlyAtLaunchBoundary(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	d := New("", nil, 0)
	var err error
	d.authority, err = access.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.authority.Close()
	if _, err := os.Stat(console.Dir()); !os.IsNotExist(err) {
		t.Fatalf("console unexpectedly exists before launch: %v", err)
	}
	session, grant, err := d.sessionAccessForLaunch(context.Background(), console.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Dir != console.Dir() || grant.SubjectID != console.ID {
		t.Fatalf("session=%+v grant=%+v", session, grant)
	}
	if info, err := os.Stat(console.Dir()); err != nil || !info.IsDir() {
		t.Fatalf("console not materialized at launch: info=%v err=%v", info, err)
	}
}

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
