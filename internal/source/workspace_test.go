package source

import (
	"context"
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

	rows := (&Workspace{}).untrackedRows(map[string]bool{"tracked-1": true}, map[string]bool{"/home/u/mine": true})

	if len(rows) != 1 {
		t.Fatalf("got %d untracked rows, want 1: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.Title != "proj" || r.State != core.StateRunning || r.Mode != "external" || r.CanAttach {
		t.Fatalf("unexpected untracked row: %+v", r)
	}
	// An untracked (detached) row preserves its runtime identity but is NOT
	// steerable: internal/daemon/steer.go can't resolve an external id, so its caps
	// are an explicit non-nil ALL-FALSE block — the daemon would refuse every verb,
	// so the row must not advertise one (AGE-178 root review).
	if r.Runtime != "claude" {
		t.Errorf("untracked row Runtime = %q, want claude (identity preserved)", r.Runtime)
	}
	if r.Caps == nil {
		t.Fatalf("untracked row Caps must be non-nil (explicit all-false), got nil")
	}
	if *r.Caps != (core.SessionCaps{}) {
		t.Errorf("untracked row Caps = %+v, want every verb false (steer.go rejects external ids)", r.Caps)
	}
}

// TestWithCaps checks the AGE-178 per-session stamp gated on the effective control
// path: a steerable row gets the honest per-kind caps (and keeps its own runtime
// identity — a Codex row is never mislabeled Claude); a non-steerable row keeps
// its identity but gets an explicit non-nil all-false block.
func TestWithCaps(t *testing.T) {
	allOn := core.SessionCaps{Prompt: true, Interject: true, Cancel: true, Permission: true}

	claude := (&Workspace{}).withCaps(core.Session{ID: "a", Kind: "claude"}, true)
	if claude.Runtime != "claude" {
		t.Errorf("claude row Runtime = %q, want claude", claude.Runtime)
	}
	if claude.Caps == nil || *claude.Caps != allOn {
		t.Errorf("steerable claude row Caps = %+v, want all-on", claude.Caps)
	}

	codex := (&Workspace{}).withCaps(core.Session{ID: "b", Kind: "codex"}, true)
	if codex.Runtime != "codex" {
		t.Errorf("codex row Runtime = %q, want codex (identity must not collapse to claude)", codex.Runtime)
	}
	if codex.Caps == nil || !codex.Caps.Permission {
		t.Errorf("steerable codex row Caps = %+v, want Permission on (rollout correlates)", codex.Caps)
	}

	// A non-steerable row of a fully-capable kind still reports every verb false —
	// identity preserved, controls off, because the daemon can't drive it.
	off := (&Workspace{}).withCaps(core.Session{ID: "d", Kind: "claude"}, false)
	if off.Runtime != "claude" {
		t.Errorf("non-steerable row Runtime = %q, want claude (identity preserved)", off.Runtime)
	}
	if off.Caps == nil || *off.Caps != (core.SessionCaps{}) {
		t.Errorf("non-steerable row Caps = %+v, want all-false non-nil", off.Caps)
	}

	// An empty Kind resolves to the default runtime, not the empty string.
	def := (&Workspace{}).withCaps(core.Session{ID: "c"}, true)
	if def.Runtime == "" {
		t.Error("empty-Kind row should resolve Runtime to the default, got empty")
	}
}

// TestControlModeStamp checks the AGE-181 §2.2 stamp: a steerable row with a live
// supervisor is "structured"; without the probe (or when not steerable) it stays
// pty (the empty default), so the field never regresses an existing consumer.
func TestControlModeStamp(t *testing.T) {
	w := &Workspace{controlMode: func(id string) string {
		if id == "sup" {
			return "structured"
		}
		return ""
	}}

	if got := w.withCaps(core.Session{ID: "sup", Kind: "codex"}, true).ControlMode; got != "structured" {
		t.Errorf("supervised steerable row ControlMode = %q, want structured", got)
	}
	if got := w.withCaps(core.Session{ID: "plain", Kind: "codex"}, true).ControlMode; got != "" {
		t.Errorf("unsupervised row ControlMode = %q, want empty (pty)", got)
	}
	// A non-steerable row is never structured even if a stale probe said so.
	if got := w.withCaps(core.Session{ID: "sup", Kind: "codex"}, false).ControlMode; got != "" {
		t.Errorf("non-steerable row ControlMode = %q, want empty", got)
	}
	// No probe installed ⇒ every row is pty (no regression).
	if got := (&Workspace{}).withCaps(core.Session{ID: "sup", Kind: "codex"}, true).ControlMode; got != "" {
		t.Errorf("no-probe row ControlMode = %q, want empty", got)
	}
}

// TestPollCapsByControlPath is the AGE-178 root-review regression: over a real
// Poll, a tracked/active agent advertises its full control caps, while an
// archived row and a detached/external row advertise an explicit non-nil
// all-false block (identity preserved) — because internal/daemon/steer.go can
// drive only the former. This is the end-to-end guard that the fix reflects the
// effective control path, not the runtime name.
func TestPollCapsByControlPath(t *testing.T) {
	// Isolate both stores from the developer's real data: the DB (XDG_DATA_HOME)
	// and the hook-state journal (HOME).
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("HOME", t.TempDir())

	db, err := store.Open()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// A work-scoped root with two children: one active, one archived.
	root := store.Session{ID: "wg1", RootID: "", Name: "payments", Mode: store.ModeTask, Scope: store.ScopeWork, Created: 1}
	active := store.Session{ID: "ag-active", RootID: "wg1", Agent: "claude", Mode: store.ModeTask, Created: 2}
	archived := store.Session{ID: "ag-archived", RootID: "wg1", Agent: "claude", Mode: store.ModeTask, Created: 3}
	for _, s := range []store.Session{root, active, archived} {
		if err := db.PutSession(s); err != nil {
			t.Fatalf("put %s: %v", s.ID, err)
		}
	}
	if err := db.SetArchivedFlag("ag-archived", true, 100); err != nil {
		t.Fatalf("archive: %v", err)
	}
	db.Close()

	// A detached/external Claude session (hook activity, no store row).
	if err := core.WriteHookState("ext-1", core.StateRunning, "/home/u/elsewhere"); err != nil {
		t.Fatalf("hook state: %v", err)
	}

	rows, err := NewWorkspace().Poll(context.Background())
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	byID := map[string]core.Session{}
	for _, r := range rows {
		byID[r.ID] = r
	}

	allOn := core.SessionCaps{Prompt: true, Interject: true, Cancel: true, Permission: true}
	off := core.SessionCaps{}

	// Tracked, active agent — full caps.
	if a, ok := byID["ag-active"]; !ok {
		t.Fatal("active agent row missing from Poll")
	} else if a.Caps == nil || *a.Caps != allOn {
		t.Errorf("active agent Caps = %+v, want all-on", a.Caps)
	}

	// Archived agent — identity preserved, every verb off.
	if a, ok := byID["ag-archived"]; !ok {
		t.Fatal("archived agent row missing from Poll")
	} else {
		if a.Runtime != "claude" {
			t.Errorf("archived agent Runtime = %q, want claude", a.Runtime)
		}
		if a.Caps == nil || *a.Caps != off {
			t.Errorf("archived agent Caps = %+v, want all-false non-nil", a.Caps)
		}
	}

	// Detached/external session — identity preserved, every verb off.
	if a, ok := byID["ext-1"]; !ok {
		t.Fatal("external session row missing from Poll")
	} else {
		if a.Runtime != "claude" {
			t.Errorf("external session Runtime = %q, want claude", a.Runtime)
		}
		if a.Caps == nil || *a.Caps != off {
			t.Errorf("external session Caps = %+v, want all-false non-nil", a.Caps)
		}
	}
}

func TestPollReconcilesRuntimeModel(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "data"))
	t.Setenv("HOME", t.TempDir())

	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	root := store.Session{ID: "wg", Scope: store.ScopeWork, Agent: "claude", Model: "opus", Created: 1}
	agentRow := store.Session{ID: "agent", RootID: "wg", Agent: "claude", Model: "sonnet", ClaudeID: "runtime-id", Created: 2}
	for _, s := range []store.Session{root, agentRow} {
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	if err := core.WriteRuntimeModel("runtime-id", "claude-opus-4-7"); err != nil {
		t.Fatal(err)
	}

	var observed string
	w := NewWorkspace()
	w.SetModelObserved(func(id, kind, model string) {
		if id == "agent" && kind == "claude" {
			observed = model
		}
	})
	rows, err := w.Poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var published string
	for _, row := range rows {
		if row.ID == "agent" {
			published = row.Model
		}
	}
	db, err = store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, ok, err := db.GetSession("agent")
	if err != nil || !ok {
		t.Fatalf("GetSession: %v, %v", ok, err)
	}
	if got.Model != "claude-opus-4-7" || observed != got.Model || published != got.Model {
		t.Fatalf("stored = %q, callback = %q, published = %q; want runtime selection", got.Model, observed, published)
	}
}

// The ARCHIVED rail section is capped at the most recently archived agents so it
// can't overrun the active rail; overflow is dropped (oldest first).
func TestRecentArchived(t *testing.T) {
	// Fewer than the cap: all returned, newest first.
	few := []store.Session{
		{ID: "a", Created: 10},
		{ID: "b", Created: 30},
		{ID: "c", Created: 20},
	}
	got := recentArchived(few)
	if len(got) != 3 || got[0].ID != "b" || got[1].ID != "c" || got[2].ID != "a" {
		t.Fatalf("few: got %v, want b,c,a", ids(got))
	}
	// The input slice must not be reordered.
	if few[0].ID != "a" {
		t.Fatalf("recentArchived reordered its input: %v", ids(few))
	}

	// More than the cap: only the newest maxArchivedRows survive.
	var many []store.Session
	for i := 0; i < maxArchivedRows+5; i++ {
		many = append(many, store.Session{ID: string(rune('a' + i)), Created: int64(i)})
	}
	got = recentArchived(many)
	if len(got) != maxArchivedRows {
		t.Fatalf("many: got %d rows, want %d", len(got), maxArchivedRows)
	}
	if got[0].Created != int64(maxArchivedRows+4) {
		t.Fatalf("many: newest kept is Created=%d, want %d", got[0].Created, maxArchivedRows+4)
	}
	oldestKept := got[len(got)-1].Created
	if oldestKept != int64(5) { // Created 0..4 dropped
		t.Fatalf("many: oldest kept is Created=%d, want 5", oldestKept)
	}

	// Ordering is by ArchivedAt, not Created: the oldest-created agent archived
	// last sorts to the top. Sessions with an unset ArchivedAt (archived before it
	// was tracked) fall back to Created order and sort below any stamped ones.
	byArchived := []store.Session{
		{ID: "old-created-archived-last", Created: 10, ArchivedAt: 300},
		{ID: "new-created-archived-first", Created: 30, ArchivedAt: 100},
		{ID: "mid", Created: 20, ArchivedAt: 200},
		{ID: "legacy-newest-created", Created: 40, ArchivedAt: 0},
		{ID: "legacy-oldest-created", Created: 5, ArchivedAt: 0},
	}
	got = recentArchived(byArchived)
	want := []string{"old-created-archived-last", "mid", "new-created-archived-first", "legacy-newest-created", "legacy-oldest-created"}
	for i, id := range want {
		if got[i].ID != id {
			t.Fatalf("byArchived: got %v, want %v", ids(got), want)
		}
	}
}

func ids(ss []store.Session) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.ID
	}
	return out
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
