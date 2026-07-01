package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/store"
)

func TestRepoTitle(t *testing.T) {
	cases := []struct {
		name string
		repo store.Repo
		want string
	}{
		{"gh nameWithOwner", store.Repo{Name: "agent-multiplexer", Source: "Kchymet/agent-multiplexer"}, "Kchymet/agent-multiplexer"},
		{"https url", store.Repo{Name: "agent-multiplexer", Source: "https://github.com/Kchymet/agent-multiplexer.git"}, "Kchymet/agent-multiplexer"},
		{"scp url", store.Repo{Name: "agent-multiplexer", Source: "git@github.com:Kchymet/agent-multiplexer.git"}, "Kchymet/agent-multiplexer"},
		{"local abs path", store.Repo{Name: "proj", Source: "/home/u/code/proj"}, "proj"},
		{"local rel path", store.Repo{Name: "proj", Source: "./proj"}, "proj"},
		{"bare name", store.Repo{Name: "thing", Source: "thing"}, "thing"},
	}
	for _, c := range cases {
		if got := repoTitle(c.repo); got != c.want {
			t.Errorf("%s: repoTitle=%q want %q", c.name, got, c.want)
		}
	}
}

func TestUntrackedRows(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	mustWrite := func(id, state, cwd string) {
		if err := core.WriteHookState(id, state, cwd); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("ext-1", core.StateRunning, "/home/u/proj")
	mustWrite("ext-idle", core.StateIdle, "/home/u/done")     // ended → skipped
	mustWrite("tracked-1", core.StateWaiting, "/home/u/mine") // tracked by id
	mustWrite("legacy-x", core.StateRunning, "/home/u/mine")  // tracked by dir (no pinned id)

	// A crashed session whose last event is well past the TTL → skipped.
	stale, _ := json.Marshal(core.HookRecord{
		State:   core.StateRunning,
		Cwd:     "/home/u/old",
		Updated: time.Now().Add(-2 * untrackedTTL).UnixMilli(),
	})
	if err := os.WriteFile(filepath.Join(core.HookStateDir(), "ext-stale"), stale, 0o644); err != nil {
		t.Fatal(err)
	}

	rows := untrackedRows(map[string]bool{"tracked-1": true}, map[string]bool{"/home/u/mine": true})

	if len(rows) != 1 {
		t.Fatalf("got %d untracked rows, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Title != "proj" || r.State != core.StateRunning || r.Mode != "external" || r.CanAttach {
		t.Fatalf("unexpected untracked row: %+v", r)
	}
}

// A leaf agent's rail title — the line you select between agents by — is its
// task summary when it has one, an explicit name when renamed, and the short id
// only for a prompt-less agent.
func TestAgentLabel(t *testing.T) {
	cases := []struct {
		name string
		s    store.Session
		want string
	}{
		{"task summary", store.Session{ID: "abcdef1234567890", Prompt: "Fix the login bug\nmore detail"}, "Fix the login bug"},
		{"explicit name wins", store.Session{ID: "abcdef1234567890", Name: "backend", Prompt: "Fix the login bug"}, "backend"},
		{"short id fallback", store.Session{ID: "abcdef1234567890"}, "abcdef12"},
		{"blank prompt falls back", store.Session{ID: "abcdef1234567890", Prompt: "   \n\t"}, "abcdef12"},
	}
	for _, c := range cases {
		if got := agentLabel(c.s); got != c.want {
			t.Errorf("%s: agentLabel=%q want %q", c.name, got, c.want)
		}
	}
}
