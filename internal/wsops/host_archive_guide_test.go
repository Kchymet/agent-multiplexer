package wsops

import (
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/amuxcfg"
	"amux/internal/store"
)

// The housekeeping section appears only while the host enables
// coordinator.archive_scope=host, so a guide never advertises a denied power.
func TestCoordinatorGuideHostArchiveSection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))
	root := store.Session{ID: "wg1", Name: "payments", Agent: "claude", Dir: "/sandbox/wg1"}
	if strings.Contains(coordinatorGuide(root), "archive_scope") {
		t.Fatal("default guide mentions host-scope archive")
	}
	if err := amuxcfg.SetCoordinatorArchiveScope(amuxcfg.ArchiveScopeHost); err != nil {
		t.Fatal(err)
	}
	guide := coordinatorGuide(root)
	for _, want := range []string{"coordinator.archive_scope=host", "hostSessions", "amux do set-archived <id> -f archived=true", "archive_target_running"} {
		if !strings.Contains(guide, want) {
			t.Errorf("host-scope guide lacks %q", want)
		}
	}
}
