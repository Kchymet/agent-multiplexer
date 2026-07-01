package wsops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
)

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
	// flip the launch from a fresh --session-id to --resume in the worktree.
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
	if dir != repoDir {
		t.Fatalf("gap-fill: launch dir = %q, want the worktree %q", dir, repoDir)
	}

	// A real, larger Claude transcript now exists: gap-fill must not shrink it back
	// to the smaller backup (RestoreCapturedTranscript's no-data-loss guard).
	realTranscript := []byte(`{"real":true,"and":"strictly longer than the backup blob"}`)
	claudePath := filepath.Join(home, ".claude", "projects", munge(repoDir), uuid+".jsonl")
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
