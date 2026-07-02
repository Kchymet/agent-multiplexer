package wsops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
)

// isolateStore points the store + session dirs at a fresh temp HOME so a test can
// exercise real store round-trips without touching the user's data.
func isolateStore(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "off")
}

func TestResumeCwds(t *testing.T) {
	base := filepath.Join("sessions", "root1", "agent1")
	tests := []struct {
		name string
		repo string
		want []string
	}{
		{"single repo checks the workspace root then the worktree", "acme/api",
			[]string{base, filepath.Join(base, "acme/api")}},
		{"multi repo has only the agent dir", "acme/api,acme/web", []string{base}},
		{"no repo has only the agent dir", "", []string{base}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resumeCwds(store.Session{Dir: base, Repo: tt.repo})
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("resumeCwds(repo=%q) = %v, want %v", tt.repo, got, tt.want)
			}
		})
	}
}

// TestAgentCommandResume exercises the whole resume-vs-fresh decision under the
// workspace-root launch convention: a pinned id with no transcript anywhere falls
// back to a fresh --session-id while surfacing a rail notice; a transcript written
// only under the LEGACY worktree convention still resumes, with the launch cwd
// moved down to the worktree so Claude's munge matches; and a transcript under the
// workspace root (the current convention) is preferred, keeping resume in the root
// even when a legacy worktree copy also exists.
func TestAgentCommandResume(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data")) // isolate the store
	t.Setenv("AMUX_JAIL", "off")

	agentDir := filepath.Join(home, "sessions", "root", "agent")
	repoDir := filepath.Join(agentDir, "acme/api")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	uuid := "11111111-1111-4111-8111-111111111111"
	s := store.Session{ID: "agent", RootID: "root", Repo: "acme/api", Dir: agentDir, ClaudeID: uuid}

	// No transcript: falls back to a fresh session, launched in the workspace root,
	// and records a rail notice.
	dir, _, argv, err := AgentCommand(s)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlag(argv, "--session-id") || hasFlag(argv, "--resume") {
		t.Fatalf("no transcript: expected --session-id, got %v", argv)
	}
	if dir != agentDir {
		t.Fatalf("no transcript: launch dir = %q, want the workspace root %q", dir, agentDir)
	}
	if core.Notice(uuid) == "" {
		t.Fatal("no transcript: expected a rail notice about the failed resume")
	}

	// Transcript only under the LEGACY worktree convention: resume, with the launch
	// cwd moved down to the worktree (so Claude's munge matches), notice cleared.
	writeTranscript(t, home, munge(repoDir), uuid)
	dir, _, argv, err = AgentCommand(s)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlag(argv, "--resume") {
		t.Fatalf("legacy transcript: expected --resume, got %v", argv)
	}
	if dir != repoDir {
		t.Fatalf("legacy transcript: launch dir = %q, want the worktree %q", dir, repoDir)
	}
	if core.Notice(uuid) != "" {
		t.Fatal("legacy transcript: notice should be cleared on a successful resume")
	}

	// Transcript also under the workspace root (current convention): prefer it, so
	// resume lands in the root even though a legacy worktree copy exists too.
	writeTranscript(t, home, munge(agentDir), uuid)
	dir, _, _, err = AgentCommand(s)
	if err != nil {
		t.Fatal(err)
	}
	if dir != agentDir {
		t.Fatalf("root transcript: launch dir = %q, want the workspace root %q", dir, agentDir)
	}
}

// TestAgentCommandGapFillRestore covers the restart gap-fill: when a pinned
// conversation's own Claude transcript is missing but amux captured a backup, the
// launch restores the backup into the resume cwd so the decision flips from a
// fresh --session-id to --resume — and never clobbers a larger real transcript.
func TestAgentCommandGapFillRestore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data")) // isolate the store
	t.Setenv("AMUX_JAIL", "off")

	agentDir := filepath.Join(home, "sessions", "root", "agent")
	repoDir := filepath.Join(agentDir, "acme/api")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	uuid := "22222222-2222-4222-8222-222222222222"
	s := store.Session{ID: "agent", RootID: "root", Repo: "acme/api", Dir: agentDir, ClaudeID: uuid}

	// A captured backup exists (from a hook) but Claude's own transcript is missing
	// under every resume cwd — the mid-turn-kill gap. Gap-fill should restore it and
	// flip the launch from a fresh --session-id to --resume in the workspace root.
	live := filepath.Join(t.TempDir(), "live.jsonl")
	if err := os.WriteFile(live, []byte(`{"backup":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := core.CaptureTranscript(uuid, live, "Stop", ""); err != nil {
		t.Fatal(err)
	}

	dir, _, argv, err := AgentCommand(s)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlag(argv, "--resume") || hasFlag(argv, "--session-id") {
		t.Fatalf("gap-fill: expected --resume from restored backup, got %v", argv)
	}
	if dir != agentDir {
		t.Fatalf("gap-fill: launch dir = %q, want the workspace root %q", dir, agentDir)
	}

	// A real, larger Claude transcript now exists in the launch dir: gap-fill must
	// not shrink it back to the smaller backup (RestoreCapturedTranscript's
	// no-data-loss guard).
	realTranscript := []byte(`{"real":true,"and":"strictly longer than the backup blob"}`)
	claudePath := filepath.Join(home, ".claude", "projects", munge(agentDir), uuid+".jsonl")
	if err := os.WriteFile(claudePath, realTranscript, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := AgentCommand(s); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(claudePath); string(got) != string(realTranscript) {
		t.Fatalf("clobber: gap-fill overwrote the larger real transcript; got %q", got)
	}
}

func hasFlag(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}

// munge mirrors claudecfg's path munge ('/' and '.' -> '-') for placing a test
// transcript where Claude would write it.
func munge(p string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '.' {
			return '-'
		}
		return r
	}, p)
}

func writeTranscript(t *testing.T, home, munged, uuid string) {
	t.Helper()
	proj := filepath.Join(home, ".claude", "projects", munged)
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, uuid+".jsonl"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRenameWorkgroup verifies a workgroup root can be renamed through the same
// "rename" action the CLI (`amux workgroup rename`) and TUI (the `r` key) send:
// the new name lands on the root and surfaces via Display(), which is what the
// rail title and `session ls` render. An empty name clears back to the id, and
// renaming an unknown id is an error rather than a silent no-op.
func TestRenameWorkgroup(t *testing.T) {
	isolateStore(t)
	ctx := context.Background()

	rootID, err := CreateWorkspace(ctx, "old-name", nil)
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	if _, err := ApplyResult(ctx, core.Action{
		Action: "rename", ID: rootID, Fields: map[string]string{"name": "  new-name  "},
	}); err != nil {
		t.Fatalf("rename action: %v", err)
	}

	root := getSession(t, rootID)
	if root.Name != "new-name" {
		t.Errorf("Name = %q, want %q (surrounding whitespace should be trimmed)", root.Name, "new-name")
	}
	if root.Display() != "new-name" {
		t.Errorf("Display() = %q, want %q", root.Display(), "new-name")
	}

	// Clearing the name falls back to the id in the rail label.
	if err := Rename(rootID, "   "); err != nil {
		t.Fatalf("Rename to blank: %v", err)
	}
	if root := getSession(t, rootID); root.Name != "" || root.Display() != rootID {
		t.Errorf("blank name: Name=%q Display()=%q, want name cleared and Display()=%q", root.Name, root.Display(), rootID)
	}

	if err := Rename("no-such-id", "x"); err == nil {
		t.Error("renaming an unknown id should error")
	}
}

// TestRenameAgent verifies the same rename path names an individual agent
// (sub-session), not just a root — the TUI `r` key works on either.
func TestRenameAgent(t *testing.T) {
	isolateStore(t)
	ctx := context.Background()

	rootID, err := CreateWorkspace(ctx, "grp", &AgentSpec{Agent: "claude"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	kids, _ := db.Children(rootID)
	db.Close()
	if len(kids) != 1 {
		t.Fatalf("want 1 agent, got %d", len(kids))
	}

	if err := Rename(kids[0].ID, "api spike"); err != nil {
		t.Fatalf("Rename agent: %v", err)
	}
	if a := getSession(t, kids[0].ID); a.Display() != "api spike" {
		t.Errorf("agent Display() = %q, want %q", a.Display(), "api spike")
	}
}

// getSession reads one session back from the store for an assertion.
func getSession(t *testing.T, id string) store.Session {
	t.Helper()
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s, ok, err := db.GetSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("session %s not found", id)
	}
	return s
}

func TestAgentWorkdir(t *testing.T) {
	base := filepath.Join("sessions", "root1", "agent1")
	tests := []struct {
		name string
		repo string
		want string
	}{
		{"single repo drops into the worktree", "acme/api", filepath.Join(base, "acme/api")},
		{"multi repo stays at the agent dir", "acme/api,acme/web", base},
		{"no repo (e.g. console) stays put", "", base},
		{"blank entries are ignored, still single", " acme/api , ", filepath.Join(base, "acme/api")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AgentWorkdir(store.Session{Dir: base, Repo: tt.repo})
			if got != tt.want {
				t.Errorf("AgentWorkdir(repo=%q) = %q, want %q", tt.repo, got, tt.want)
			}
		})
	}
}

// TestCreateWorkspaceRepoLessAgent verifies a workgroup can be created with a
// repo-less default agent — no repos on the workgroup, none on the agent, and no
// git worktrees needed.
func TestCreateWorkspaceRepoLessAgent(t *testing.T) {
	isolateStore(t)
	ctx := context.Background()

	rootID, err := CreateWorkspace(ctx, "cloud-orchestrator", &AgentSpec{Agent: "claude"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	root, ok, _ := db.GetSession(rootID)
	if !ok {
		t.Fatalf("root %s not found", rootID)
	}
	if root.Repo != "" {
		t.Errorf("workgroup should carry no repos, got %q", root.Repo)
	}
	kids, _ := db.Children(rootID)
	if len(kids) != 1 {
		t.Fatalf("want 1 agent, got %d", len(kids))
	}
	if kids[0].Repo != "" {
		t.Errorf("repo-less agent should have no repos, got %q", kids[0].Repo)
	}
}

// TestSetAgentReposSkipsUntracked verifies re-scoping an agent to an untracked
// repo name is a no-op that never errors (defensive against stale/typo names) —
// the reported "unknown repo" hard-fail is gone.
func TestSetAgentReposSkipsUntracked(t *testing.T) {
	isolateStore(t)
	ctx := context.Background()

	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	rootID := db.NewID()
	agentID := db.NewID()
	if err := db.PutSession(store.Session{ID: rootID, Scope: store.ScopeWork, Mode: store.ModeTask}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(store.Session{ID: agentID, RootID: rootID, Repo: "", Branch: "amux/x-y"}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if err := SetAgentRepos(ctx, agentID, []string{"nope"}); err != nil {
		t.Fatalf("SetAgentRepos with an untracked repo should not error, got %v", err)
	}

	db, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a, ok, _ := db.GetSession(agentID)
	if !ok {
		t.Fatal("agent vanished")
	}
	if a.Repo != "" {
		t.Errorf("untracked repo should have been skipped, agent repos = %q", a.Repo)
	}

	// Re-scoping a workgroup root (not an agent) is an error.
	if err := SetAgentRepos(ctx, rootID, nil); err == nil {
		t.Error("SetAgentRepos on a root should error")
	}
}

// TestWriteAgentGuide checks the guide lands in the file each provider actually
// reads: CLAUDE.md for Claude (incl. the "" default), AGENTS.md for others.
func TestWriteAgentGuide(t *testing.T) {
	for _, tc := range []struct {
		kind, file, other string
	}{
		{"", "CLAUDE.md", "AGENTS.md"},
		{"claude", "CLAUDE.md", "AGENTS.md"},
		{"codex", "AGENTS.md", "CLAUDE.md"},
	} {
		dir := t.TempDir()
		writeAgentGuide(dir, "amux/root-agent", tc.kind)

		b, err := os.ReadFile(filepath.Join(dir, tc.file))
		if err != nil {
			t.Errorf("kind %q: expected %s: %v", tc.kind, tc.file, err)
			continue
		}
		if !strings.Contains(string(b), "amux/root-agent") {
			t.Errorf("kind %q: %s missing the branch name", tc.kind, tc.file)
		}
		if _, err := os.Stat(filepath.Join(dir, tc.other)); err == nil {
			t.Errorf("kind %q: unexpectedly wrote %s too", tc.kind, tc.other)
		}
	}
}
