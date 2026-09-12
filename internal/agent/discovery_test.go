//go:build linux

package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"amux/internal/claudecfg"
	"amux/internal/codexcfg"
)

func TestDiscoverySkipsHostAliasesAndSpecialFiles(t *testing.T) {
	for _, kind := range []string{"claude", "codex"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
			t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
			t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
			pathFor := func(id string) string {
				if kind == "claude" {
					return claudecfg.User().TranscriptPath("/project", id)
				}
				return codexcfg.UserHome().NewRolloutPath(id)
			}
			paths := []string{
				pathFor("11111111-1111-4111-8111-111111111111"),
				pathFor("22222222-2222-4222-8222-222222222222"),
				pathFor("33333333-3333-4333-8333-333333333333"),
				pathFor("44444444-4444-4444-8444-444444444444"),
			}
			if err := os.MkdirAll(filepath.Dir(paths[0]), 0700); err != nil {
				t.Fatal(err)
			}
			secret := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(secret, []byte(`{"cwd":"HOST-SECRET"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(secret, paths[0]); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(secret, paths[1]); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Mkfifo(paths[2], 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths[3], []byte(`{"cwd":"/valid"}`), 0600); err != nil {
				t.Fatal(err)
			}
			rows := ListSessionRows()
			if len(rows) != 1 || rows[0].Path != paths[3] || rows[0].Cwd != "/valid" {
				t.Fatalf("unsafe or missing discovery: %+v", rows)
			}
			// Moving the whole transcript tree behind a parent alias must not redirect
			// the daemon even though the individual final file is regular.
			parent := filepath.Dir(paths[3])
			external := filepath.Join(t.TempDir(), "transcripts")
			if err := os.Rename(parent, external); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(external, parent); err != nil {
				t.Fatal(err)
			}
			if rows := ListSessionRows(); len(rows) != 0 {
				t.Fatalf("followed parent alias: %+v", rows)
			}
		})
	}
}
