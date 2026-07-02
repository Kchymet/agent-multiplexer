package codexcfg

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mkRollout writes a rollout jsonl for uuid under sessions/y/m/d with a
// session_meta line carrying cwd, and returns its path.
func mkRollout(t *testing.T, home, y, m, d, uuid, cwd string) string {
	t.Helper()
	dir := filepath.Join(home, "sessions", y, m, d)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-"+y+"-"+m+"-"+d+"T00-00-00-"+uuid+".jsonl")
	line := `{"type":"session_meta","payload":{"cwd":` + strconv.Quote(cwd) + `}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRolloutDiscoveryAndCwd covers RolloutPath, FindSession's cwd matching, and
// AnySession — the rollout-locating primitives the harness builds on.
func TestRolloutDiscoveryAndCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	cwd := t.TempDir()

	const uuid = "11111111-1111-4111-8111-111111111111"
	path := mkRollout(t, home, "2026", "07", "01", uuid, cwd)

	if got, ok := RolloutPath(uuid); !ok || got != path {
		t.Fatalf("RolloutPath = %q,%v want %q,true", got, ok, path)
	}
	if _, ok := RolloutPath("22222222-2222-4222-8222-222222222222"); ok {
		t.Fatal("RolloutPath matched an unknown uuid")
	}

	// FindSession returns a candidate only when it equals the recorded cwd.
	if c, ok := FindSession(uuid, "/other", cwd); !ok || c != cwd {
		t.Fatalf("FindSession = %q,%v want %q,true", c, ok, cwd)
	}
	if _, ok := FindSession(uuid, "/nope"); ok {
		t.Fatal("FindSession matched a non-candidate cwd")
	}

	if !AnySession(cwd) {
		t.Fatalf("AnySession(%q) = false", cwd)
	}
	if AnySession("/elsewhere") {
		t.Fatal("AnySession(/elsewhere) = true")
	}
}

// TestLatestSession picks the newest rollout recorded under a cwd, by mtime.
func TestLatestSession(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	cwd := t.TempDir()

	const oldID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	const newID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	older := mkRollout(t, home, "2026", "07", "01", oldID, cwd)
	newer := mkRollout(t, home, "2026", "07", "02", newID, cwd)

	// Pin mtimes so ordering doesn't depend on write speed.
	oldT := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	newT := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(older, oldT, oldT); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, newT, newT); err != nil {
		t.Fatal(err)
	}

	if id, ok := LatestSession(cwd); !ok || id != newID {
		t.Fatalf("LatestSession = %q,%v want %q,true", id, ok, newID)
	}
	if _, ok := LatestSession("/unused"); ok {
		t.Fatal("LatestSession matched an unused cwd")
	}
}

// TestListSessions lists rollouts most-recent first, carrying the parsed uuid
// and recorded cwd.
func TestListSessions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	cwd := t.TempDir()

	const oldID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	const newID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	older := mkRollout(t, home, "2026", "07", "01", oldID, cwd)
	newer := mkRollout(t, home, "2026", "07", "02", newID, cwd)
	oldT := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	newT := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	_ = os.Chtimes(older, oldT, oldT)
	_ = os.Chtimes(newer, newT, newT)

	got := ListSessions()
	if len(got) != 2 {
		t.Fatalf("ListSessions len = %d, want 2", len(got))
	}
	if got[0].ID != newID || got[1].ID != oldID {
		t.Fatalf("ListSessions order = %q,%q want newest first", got[0].ID, got[1].ID)
	}
	if got[0].Cwd != cwd {
		t.Fatalf("ListSessions cwd = %q, want %q", got[0].Cwd, cwd)
	}
}

// TestTrustDir marks a dir trusted, preserves unrelated config, reads back the
// top-level model, and is idempotent (a second call changes nothing).
func TestTrustDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	cfg := filepath.Join(home, "config.toml")

	seed := "model = \"gpt-5.5\"\n\n[history]\npersistence = \"save-all\"\n"
	if err := os.WriteFile(cfg, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	abs, _ := filepath.Abs(dir)
	if err := TrustDir(dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(cfg)
	s := string(got)

	if !strings.Contains(s, "[projects."+strconv.Quote(abs)+"]") {
		t.Fatalf("trust table missing:\n%s", s)
	}
	if !strings.Contains(s, `trust_level = "trusted"`) {
		t.Fatalf("trust_level missing:\n%s", s)
	}
	// Unrelated content preserved verbatim.
	for _, want := range []string{`model = "gpt-5.5"`, "[history]", `persistence = "save-all"`} {
		if !strings.Contains(s, want) {
			t.Fatalf("unrelated config %q not preserved:\n%s", want, s)
		}
	}
	if m := PreferredModel(); m != "gpt-5.5" {
		t.Fatalf("PreferredModel = %q, want gpt-5.5", m)
	}

	// Idempotent: trusting again leaves the file byte-for-byte unchanged.
	if err := TrustDir(dir); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(cfg)
	if string(again) != s {
		t.Fatalf("TrustDir not idempotent:\nfirst:\n%s\nsecond:\n%s", s, string(again))
	}
}

// TestTrustDirCreatesConfig verifies TrustDir creates config.toml when absent.
func TestTrustDirCreatesConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	dir := t.TempDir()
	abs, _ := filepath.Abs(dir)
	if err := TrustDir(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatalf("config.toml not created: %v", err)
	}
	if !strings.Contains(string(b), "[projects."+strconv.Quote(abs)+"]") {
		t.Fatalf("trust table missing:\n%s", string(b))
	}
}
