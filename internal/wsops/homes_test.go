package wsops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/console"
	"amux/internal/store"
)

// envOf returns the value of key in an AgentEnv slice.
func envOf(env []string, key string) string {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return strings.TrimPrefix(kv, key+"=")
		}
	}
	return ""
}

func TestResolveSessionConsole(t *testing.T) {
	isolateStore(t)
	s, ok, err := ResolveSession(console.ID)
	if err != nil || !ok {
		t.Fatalf("ResolveSession(console) = ok=%v err=%v", ok, err)
	}
	if s.Role() != store.RoleConsole || s.Dir != console.Dir() {
		t.Fatalf("console session = %+v, want role console in %s", s, console.Dir())
	}
	if _, err := os.Stat(console.Dir()); err != nil {
		t.Fatalf("console dir not created: %v", err)
	}
	if _, ok, _ := ResolveSession("nope"); ok {
		t.Fatal("an unknown id resolved")
	}
}

// A workgroup created before default sessions has no sandbox dir and no pinned
// conversation; resolving it as a coordinator fills both in, persistently, so
// the launch that follows resumes durably.
func TestResolveSessionBackfillsCoordinator(t *testing.T) {
	isolateStore(t)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutSession(store.Session{ID: "wg1", Name: "payments", Scope: store.ScopeWork, Mode: store.ModeTask, Created: 1}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, ok, err := ResolveSession("wg1")
	if err != nil || !ok {
		t.Fatalf("ResolveSession(wg1) = ok=%v err=%v", ok, err)
	}
	if s.Role() != store.RoleCoordinator {
		t.Fatalf("role = %q, want coordinator", s.Role())
	}
	if want := store.RootDir("wg1"); s.Dir != want {
		t.Fatalf("Dir = %q, want %q", s.Dir, want)
	}
	if _, err := os.Stat(s.Dir); err != nil {
		t.Fatalf("container dir not created: %v", err)
	}
	if s.ClaudeID == "" {
		t.Fatal("no conversation id pinned")
	}
	db, _ = store.Open()
	defer db.Close()
	got, _, _ := db.GetSession("wg1")
	if got.Dir != s.Dir || got.ClaudeID != s.ClaudeID {
		t.Fatalf("backfill not persisted: %+v", got)
	}
	// Resolving again is stable: same dir, same pinned id.
	again, _, _ := ResolveSession("wg1")
	if again.ClaudeID != s.ClaudeID || again.Dir != s.Dir {
		t.Fatalf("second resolve changed the session: %+v vs %+v", again, s)
	}
}

// A tracked repo's home session is created on first resolve (for repos tracked
// before default sessions), keyed by the repo name.
func TestResolveSessionCreatesRepoHome(t *testing.T) {
	isolateStore(t)
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutRepo(store.Repo{Name: "api", Source: "octo/api", GitDir: filepath.Join(t.TempDir(), "api.git")}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, ok, err := ResolveSession("api")
	if err != nil || !ok {
		t.Fatalf("ResolveSession(api) = ok=%v err=%v", ok, err)
	}
	if s.Role() != store.RoleRepo || s.ID != store.RepoHomeID("api") || s.Repo != "api" || s.Scope != store.ScopeRepo {
		t.Fatalf("repo home = %+v", s)
	}
	if s.Dir != store.RootDir("api") || s.ClaudeID == "" {
		t.Fatalf("repo home not materialized: %+v", s)
	}
	if _, err := os.Stat(s.Dir); err != nil {
		t.Fatalf("home dir not created: %v", err)
	}
	// It is a single startable session, and it renders as the repo row — never
	// as a workgroup with members.
	ids, err := AgentIDsUnder("api")
	if err != nil || len(ids) != 1 || ids[0] != "api" {
		t.Fatalf("AgentIDsUnder(api) = %v, %v; want [api]", ids, err)
	}
	// The home is idempotent across resolves and EnsureRepoHome.
	again, _ := EnsureRepoHome("api")
	if again.ClaudeID != s.ClaudeID {
		t.Fatalf("EnsureRepoHome re-minted the conversation: %q vs %q", again.ClaudeID, s.ClaudeID)
	}
	if _, err := EnsureRepoHome("nope"); err == nil {
		t.Fatal("EnsureRepoHome accepted an untracked repo")
	}
}

// A new workgroup IS its coordinator: the root row has a sandbox (the container
// dir) and a pinned conversation from creation. `start <root>` still means the
// members; the coordinator is opened or prompted directly.
func TestCreateWorkspaceIsCoordinator(t *testing.T) {
	isolateStore(t)
	ctx := context.Background()
	rootID, err := CreateWorkspace(ctx, "payments", &AgentSpec{Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	db, _ := store.Open()
	root, _, _ := db.GetSession(rootID)
	kids, _ := db.Children(rootID)
	db.Close()
	if root.Role() != store.RoleCoordinator {
		t.Fatalf("role = %q, want coordinator", root.Role())
	}
	if root.Dir != store.RootDir(rootID) || root.ClaudeID == "" || root.Agent == "" {
		t.Fatalf("root not a session: %+v", root)
	}
	if _, err := os.Stat(root.Dir); err != nil {
		t.Fatalf("container dir missing: %v", err)
	}
	ids, err := AgentIDsUnder(rootID)
	if err != nil || len(ids) != 1 || ids[0] != kids[0].ID {
		t.Fatalf("AgentIDsUnder(root) = %v, %v; want the member only", ids, err)
	}
	// Every pane of the coordinator runs in the container dir (it has no worktree,
	// whatever repos its members work on).
	if wd := AgentWorkdir(store.Session{ID: rootID, Scope: store.ScopeWork, Repo: "api", Dir: root.Dir}); wd != root.Dir {
		t.Fatalf("AgentWorkdir(coordinator) = %q, want %q", wd, root.Dir)
	}
	// Launching the coordinator regenerates its guide in the container dir and
	// runs it there, with the coordinator role exported.
	dir, env, _, err := AgentCommand(root)
	if err != nil {
		t.Fatal(err)
	}
	if dir != root.Dir {
		t.Fatalf("launch dir = %q, want %q", dir, root.Dir)
	}
	if envOf(env, "AMUX_ROLE") != store.RoleCoordinator || envOf(env, "AMUX_SCOPE") != store.ScopeWork {
		t.Fatalf("env = %v, want AMUX_ROLE=coordinator AMUX_SCOPE=work", env)
	}
	b, err := os.ReadFile(filepath.Join(root.Dir, "CLAUDE.md"))
	if err != nil {
		t.Fatal(err)
	}
	guide := string(b)
	for _, want := range []string{"coordinator", "payments", kids[0].ID, "amux do add-agent " + rootID, "never edit an agent's worktree"} {
		if !strings.Contains(guide, want) {
			t.Errorf("coordinator guide missing %q", want)
		}
	}
	if strings.Contains(guide, "git merge --no-edit origin/HEAD") {
		t.Error("coordinator guide carries the member branch workflow")
	}
}

func TestGuidesByRole(t *testing.T) {
	isolateStore(t)
	ctx := context.Background()
	db, _ := store.Open()
	if err := db.PutRepo(store.Repo{Name: "api", Source: "octo/api", GitDir: bareRepoWithCommit(t)}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	rootID, err := CreateWorkspace(ctx, "payments", &AgentSpec{Agent: "claude", Prompt: "fix the idempotency bug"})
	if err != nil {
		t.Fatal(err)
	}
	oneOff, err := CreateRepoWorkgroup(ctx, "api", AgentSpec{Agent: "claude", Prompt: "review open PRs"})
	if err != nil {
		t.Fatal(err)
	}

	// Console: the whole inventory, and the operating vocabulary.
	c, _, _ := ResolveSession(console.ID)
	writeGuide(c)
	b, _ := os.ReadFile(filepath.Join(c.Dir, "CLAUDE.md"))
	for _, want := range []string{"amux console", "payments", rootID, "fix the idempotency bug", "octo/api", oneOff.ID, "amux do steer", "amux do new-workgroup", "amux agent sessions"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("console guide missing %q", want)
		}
	}

	// Repo home: this repo's one-off sessions and how to dispatch more.
	home, _, _ := ResolveSession("api")
	writeGuide(home)
	b, _ = os.ReadFile(filepath.Join(home.Dir, "CLAUDE.md"))
	for _, want := range []string{"home session", "octo/api", oneOff.ID, "review open PRs", "amux do new-repo-agent api", "git -C "} {
		if !strings.Contains(string(b), want) {
			t.Errorf("repo guide missing %q", want)
		}
	}
	if strings.Contains(string(b), rootID) {
		t.Error("repo guide lists a workgroup agent that isn't on this repo")
	}

	// Env exports the role and scope for each.
	if got := envOf(AgentEnv(c), "AMUX_ROLE"); got != store.RoleConsole {
		t.Errorf("console AMUX_ROLE = %q", got)
	}
	if got := envOf(AgentEnv(c), "AMUX_SCOPE"); got != "global" {
		t.Errorf("console AMUX_SCOPE = %q", got)
	}
	if got := envOf(AgentEnv(home), "AMUX_ROLE"); got != store.RoleRepo {
		t.Errorf("repo home AMUX_ROLE = %q", got)
	}
	if got := envOf(AgentEnv(oneOff), "AMUX_ROLE"); got != "" {
		t.Errorf("agent AMUX_ROLE = %q, want empty", got)
	}
	if got := envOf(AgentEnv(oneOff), "AMUX_SCOPE"); got != store.ScopeRepo {
		t.Errorf("repo agent AMUX_SCOPE = %q", got)
	}
}

// Deleting a workgroup removes the coordinator's own files but never an agent
// sandbox that still lives under the container (an agent moved out keeps its
// dir there — move is DB-only).
func TestDeleteWorkgroupKeepsMovedAgentDir(t *testing.T) {
	isolateStore(t)
	ctx := context.Background()
	rootID, err := CreateWorkspace(ctx, "old", &AgentSpec{Agent: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	db, _ := store.Open()
	root, _, _ := db.GetSession(rootID)
	kids, _ := db.Children(rootID)
	db.Close()
	agentDir := kids[0].Dir
	writeGuide(root) // the coordinator's own file in the container dir
	guide := filepath.Join(root.Dir, "CLAUDE.md")
	if _, err := os.Stat(guide); err != nil {
		t.Fatal(err)
	}
	if err := MoveAgent(ctx, kids[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	// MoveAgent drops an emptied *repo-scoped* root only; a work-scoped one stays.
	if err := DeleteByID(ctx, rootID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(agentDir); err != nil {
		t.Fatalf("moved agent's sandbox was deleted with its old workgroup: %v", err)
	}
	if _, err := os.Stat(guide); err == nil {
		t.Fatal("coordinator guide survived the workgroup's deletion")
	}
	db, _ = store.Open()
	defer db.Close()
	if _, ok, _ := db.GetSession(rootID); ok {
		t.Fatal("root row survived")
	}
	// A repo home can't be deleted as a workgroup; it goes with its repo.
	if err := db.PutRepo(store.Repo{Name: "api", Source: "octo/api", GitDir: filepath.Join(t.TempDir(), "api.git")}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	home, _, _ := ResolveSession("api")
	if err := DeleteByID(ctx, home.ID); err == nil {
		t.Fatal("deleted a repo home as if it were a workgroup")
	}
	if err := RemoveRepo("api"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home.Dir); err == nil {
		t.Fatal("repo home dir survived untracking the repo")
	}
	if _, ok, _ := ResolveSession("api"); ok {
		t.Fatal("repo home resolved after its repo was untracked")
	}
}
