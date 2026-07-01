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
		{"single repo checks worktree then agent dir", "acme/api",
			[]string{filepath.Join(base, "acme/api"), base}},
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

// TestAgentCommandResume exercises the whole resume-vs-fresh decision: a
// transcript written under the OLD (agent-dir) convention resumes even though a
// single-repo agent now launches in its worktree subdir; the launch cwd is moved
// to wherever the transcript lives; and a pinned id with no transcript anywhere
// falls back to a fresh --session-id while surfacing a rail notice instead of
// silently starting over.
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

	// No transcript: falls back to a fresh session and records a rail notice.
	dir, _, argv, err := AgentCommand(s)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlag(argv, "--session-id") || hasFlag(argv, "--resume") {
		t.Fatalf("no transcript: expected --session-id, got %v", argv)
	}
	if dir != repoDir {
		t.Fatalf("no transcript: launch dir = %q, want the worktree %q", dir, repoDir)
	}
	if core.Notice(uuid) == "" {
		t.Fatal("no transcript: expected a rail notice about the failed resume")
	}

	// Transcript under the OLD agent-dir convention: resume, launched under the
	// agent dir (so Claude's munge matches), and the stale notice is cleared.
	writeTranscript(t, home, munge(agentDir), uuid)
	dir, _, argv, err = AgentCommand(s)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlag(argv, "--resume") {
		t.Fatalf("legacy transcript: expected --resume, got %v", argv)
	}
	if dir != agentDir {
		t.Fatalf("legacy transcript: launch dir = %q, want the agent dir %q", dir, agentDir)
	}
	if core.Notice(uuid) != "" {
		t.Fatal("legacy transcript: notice should be cleared on a successful resume")
	}

	// Transcript under the current worktree convention: resume there.
	writeTranscript(t, home, munge(repoDir), uuid)
	dir, _, _, err = AgentCommand(s)
	if err != nil {
		t.Fatal(err)
	}
	if dir != repoDir {
		t.Fatalf("worktree transcript: launch dir = %q, want %q", dir, repoDir)
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
			got := agentWorkdir(store.Session{Dir: base, Repo: tt.repo})
			if got != tt.want {
				t.Errorf("agentWorkdir(repo=%q) = %q, want %q", tt.repo, got, tt.want)
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
