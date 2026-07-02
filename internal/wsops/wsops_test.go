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

// writeRollout drops a fake Codex rollout for uuid recorded under cwd into
// $CODEX_HOME/sessions/2026/01/01, mirroring Codex's rollout-<ts>-<uuid>.jsonl
// naming and the session_meta cwd line codexcfg reads. Returns the rollout path.
func writeRollout(t *testing.T, codexHome, uuid, cwd string) string {
	t.Helper()
	dir := filepath.Join(codexHome, "sessions", "2026", "01", "01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-01-01T00-00-00-"+uuid+".jsonl")
	if err := os.WriteFile(path, []byte(`{"cwd":`+quote(cwd)+`}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `\`, `\\`) + `"` }

// TestAgentCommandCodexFresh: a codex agent with no pinned id and no rollout on
// disk launches fresh — the sandbox flag from agent.Argv plus the prompt as a
// positional, and no `resume` subcommand.
func TestAgentCommandCodexFresh(t *testing.T) {
	isolateStore(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	dir := filepath.Join(t.TempDir(), "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s := store.Session{ID: "a", RootID: "r", Agent: "codex", Dir: dir, Prompt: "do the thing"}

	_, _, argv, err := AgentCommand(s)
	if err != nil {
		t.Fatal(err)
	}
	if hasFlag(argv, "resume") {
		t.Fatalf("fresh codex launch should not resume, got %v", argv)
	}
	if !hasFlag(argv, "--sandbox") {
		t.Fatalf("codex launch missing --sandbox flag, got %v", argv)
	}
	if argv[len(argv)-1] != "do the thing" {
		t.Fatalf("fresh codex launch should end with the prompt positional, got %v", argv)
	}
}

// TestAgentCommandCodexAdopt: a codex agent with no pinned id but a rollout
// already recorded under its dir adopts that id — resuming it and persisting the
// id onto the store record so later launches find it.
func TestAgentCommandCodexAdopt(t *testing.T) {
	isolateStore(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	dir := filepath.Join(t.TempDir(), "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	uuid := "33333333-3333-4333-8333-333333333333"
	writeRollout(t, os.Getenv("CODEX_HOME"), uuid, dir)

	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	s := store.Session{ID: "a", RootID: "r", Agent: "codex", Dir: dir, Prompt: "hi"}
	if err := db.PutSession(s); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, _, argv, err := AgentCommand(s)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlag(argv, "resume") || argv[len(argv)-1] != uuid {
		t.Fatalf("codex adopt should resume the discovered id, got %v", argv)
	}
	db, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, ok, _ := db.GetSession("a")
	if !ok || got.ClaudeID != uuid {
		t.Fatalf("adopted id should persist to the store, got %q (ok=%v)", got.ClaudeID, ok)
	}
}

// TestAgentCommandCodexResumePinned: a codex agent whose pinned id has a matching
// rollout resumes it via `codex resume <id>`.
func TestAgentCommandCodexResumePinned(t *testing.T) {
	isolateStore(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	dir := filepath.Join(t.TempDir(), "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	uuid := "44444444-4444-4444-8444-444444444444"
	writeRollout(t, os.Getenv("CODEX_HOME"), uuid, dir)
	s := store.Session{ID: "a", RootID: "r", Agent: "codex", Dir: dir, ClaudeID: uuid}

	_, _, argv, err := AgentCommand(s)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlag(argv, "resume") || argv[len(argv)-1] != uuid {
		t.Fatalf("pinned codex resume expected `resume %s`, got %v", uuid, argv)
	}
}

// TestAgentCommandCodexPinnedResumeFails: a codex agent with a pinned id whose
// rollout is missing clears the id (so the next launch adopts a real one), warns
// on the rail, and starts fresh with the prompt.
func TestAgentCommandCodexPinnedResumeFails(t *testing.T) {
	isolateStore(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	dir := filepath.Join(t.TempDir(), "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	uuid := "55555555-5555-4555-8555-555555555555"
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	s := store.Session{ID: "a", RootID: "r", Agent: "codex", Dir: dir, ClaudeID: uuid, Prompt: "resume me"}
	if err := db.PutSession(s); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, _, argv, err := AgentCommand(s)
	if err != nil {
		t.Fatal(err)
	}
	if hasFlag(argv, "resume") {
		t.Fatalf("a failed pinned resume should launch fresh, got %v", argv)
	}
	if argv[len(argv)-1] != "resume me" {
		t.Fatalf("fresh fallback should end with the prompt, got %v", argv)
	}
	if core.Notice(uuid) == "" {
		t.Fatal("a failed pinned resume should record a rail notice")
	}
	db, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, ok, _ := db.GetSession("a")
	if !ok || got.ClaudeID != "" {
		t.Fatalf("a failed pinned resume should clear the stored id, got %q", got.ClaudeID)
	}
}

// TestAgentCommandCodexResumeIgnoresRecordedCwd: `codex resume <id>` locates a
// session by uuid regardless of the cwd its rollout recorded, so a pinned id must
// resume even when the recorded cwd doesn't match any of amux's candidate dirs
// (e.g. the workdir convention changed under it).
func TestAgentCommandCodexResumeIgnoresRecordedCwd(t *testing.T) {
	isolateStore(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	dir := filepath.Join(t.TempDir(), "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	uuid := "66666666-6666-4666-8666-666666666666"
	writeRollout(t, os.Getenv("CODEX_HOME"), uuid, "/somewhere/unrelated")
	s := store.Session{ID: "a", RootID: "r", Agent: "codex", Dir: dir, ClaudeID: uuid}

	_, _, argv, err := AgentCommand(s)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlag(argv, "resume") || argv[len(argv)-1] != uuid {
		t.Fatalf("pinned resume must not depend on the rollout's recorded cwd, got %v", argv)
	}
}

// TestAgentCommandCodexLostPinAdoptsNewest: when the pinned rollout is gone but
// another conversation is recorded under the agent's dir, the launch adopts that
// one (rather than dropping to a fresh session), persists the new pin, and keys
// the rail notice under the adopted id — the id the rail reads notices by.
func TestAgentCommandCodexLostPinAdoptsNewest(t *testing.T) {
	isolateStore(t)
	t.Setenv("CODEX_HOME", t.TempDir())

	dir := filepath.Join(t.TempDir(), "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lost := "77777777-7777-4777-8777-777777777777"
	kept := "88888888-8888-4888-8888-888888888888"
	writeRollout(t, os.Getenv("CODEX_HOME"), kept, dir)

	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	s := store.Session{ID: "a", RootID: "r", Agent: "codex", Dir: dir, ClaudeID: lost}
	if err := db.PutSession(s); err != nil {
		t.Fatal(err)
	}
	db.Close()

	_, _, argv, err := AgentCommand(s)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFlag(argv, "resume") || argv[len(argv)-1] != kept {
		t.Fatalf("a lost pin should adopt the newest rollout under the dir, got %v", argv)
	}
	if got := getSession(t, "a"); got.ClaudeID != kept {
		t.Fatalf("adopted id should replace the lost pin in the store, got %q", got.ClaudeID)
	}
	if core.Notice(kept) == "" {
		t.Fatal("the fallback notice should be keyed under the adopted id the rail reads")
	}
}

// TestAddAgentRejectsUnknownKind: a typo'd harness kind must fail at creation —
// nothing can edit a session's kind afterwards, so persisting it would mint an
// agent that errors on every launch.
func TestAddAgentRejectsUnknownKind(t *testing.T) {
	isolateStore(t)
	ctx := context.Background()

	rootID, err := CreateWorkspace(ctx, "ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyResult(ctx, core.Action{Action: "add-agent", ID: rootID, Fields: map[string]string{"agent": "codx"}}); err == nil {
		t.Fatal("add-agent with an unknown kind should error, not persist")
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	kids, _ := db.Children(rootID)
	if len(kids) != 0 {
		t.Fatalf("a rejected agent must not be persisted, found %d children", len(kids))
	}
}

// TestApplyResultHonorsAgentField: add-agent with Fields["agent"]="codex" creates
// a codex agent (not the hardcoded claude), while an absent field still defaults
// to claude — the back-compat contract for older clients.
func TestApplyResultHonorsAgentField(t *testing.T) {
	isolateStore(t)
	ctx := context.Background()

	rootID, err := CreateWorkspace(ctx, "ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	codexID, err := ApplyResult(ctx, core.Action{Action: "add-agent", ID: rootID, Fields: map[string]string{"agent": "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	claudeID, err := ApplyResult(ctx, core.Action{Action: "add-agent", ID: rootID, Fields: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}

	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cx, _, _ := db.GetSession(codexID)
	if cx.Agent != "codex" {
		t.Errorf("add-agent agent=codex made a %q agent", cx.Agent)
	}
	if cx.ClaudeID != "" {
		t.Errorf("a codex agent should not pin a claude id up front, got %q", cx.ClaudeID)
	}
	cl, _, _ := db.GetSession(claudeID)
	if cl.Agent != "claude" {
		t.Errorf("add-agent with no agent field should default to claude, got %q", cl.Agent)
	}
	if cl.ClaudeID == "" {
		t.Errorf("a claude agent should pin a session id up front")
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
		// The guide must steer agents to merge, not rebase: rebasing a pushed
		// branch forces a force-push to update its PR, which needs a human to
		// unblock. Merging keeps every push a fast-forward.
		if !strings.Contains(string(b), "git merge --no-edit origin/HEAD") {
			t.Errorf("kind %q: %s should tell agents to merge the remote", tc.kind, tc.file)
		}
		if strings.Contains(string(b), "git rebase") {
			t.Errorf("kind %q: %s should not tell agents to run git rebase (forces a force-push on a pushed PR)", tc.kind, tc.file)
		}
		if _, err := os.Stat(filepath.Join(dir, tc.other)); err == nil {
			t.Errorf("kind %q: unexpectedly wrote %s too", tc.kind, tc.other)
		}
	}
}
