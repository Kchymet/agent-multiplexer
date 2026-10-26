package panespec

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"amux/internal/amuxcfg"
	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/store"
	"amux/internal/wsops"
)

// The retired shared Claude store is never visible to any pane, even with its
// saved setting and an enabled older login; private roots stay hidden too.
func TestRetiredSharedAuthDirectoryIsNeverVisible(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A runtime mount can also expose the canonical spelling of XDG_DATA_HOME.
	realData := filepath.Join(home, "real-data")
	if err := os.MkdirAll(realData, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realData, filepath.Join(home, "data")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	useClaudeAuth(t, amuxcfg.AuthShared)
	t.Setenv("AMUX_JAIL", "on")
	useFakeSecureBwrap(t)
	if err := claudecfg.Login(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"claude", "codex"} {
		for _, tab := range []int{TabAgent, TabTerminal, TabEditor} {
			s := store.Session{ID: "one", Agent: kind, Dir: filepath.Join(home, "agent")}
			spec := testLaunchSpec(t, s)
			argv, err := scope(s.Dir, tab, s, spec.Access, nil, []string{"/usr/bin/true"})
			if err != nil {
				t.Fatal(err)
			}
			bind := -1
			for i := 0; i < len(argv)-2; i++ {
				if slices.Equal(argv[i:i+3], []string{"--bind", claudecfg.SharedAuthDir(), claudecfg.SharedAuthDir()}) {
					bind = i
				}
			}
			joined := strings.Join(argv, "\x00")
			for _, forbidden := range []string{core.DataDir(), core.StateDir(), core.HookStateDir(), core.TranscriptDir()} {
				if strings.Contains(joined, "\x00"+forbidden+"\x00") {
					t.Fatalf("%s tab %d exposes broad/private root %s: %v", kind, tab, forbidden, argv)
				}
			}
			if bind >= 0 {
				t.Fatalf("%s tab %d mounts the retired shared store: %v", kind, tab, argv)
			}
			if env := wsops.AgentEnv(s); slices.Contains(env, claudecfg.SecureStorageEnv+"="+claudecfg.SharedAuthDir()) {
				t.Fatalf("%s selects the retired shared store: %v", kind, env)
			}
		}
	}
}
