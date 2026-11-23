package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/store"
	"amux/internal/wsops"
)

// coordinatorWithArchivedMembers stores workgroup wg1 (a Claude coordinator,
// stopped) whose only members are archived, plus one live member when live is
// set — the shape of 5a3b1a and e06ffd on 2026-11-23.
func coordinatorWithArchivedMembers(t *testing.T, live bool) {
	t.Helper()
	coordinatorDir := store.CoordinatorDir("wg1")
	if err := os.MkdirAll(coordinatorDir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows := []store.Session{
		{ID: "wg1", Name: "web login", Scope: store.ScopeWork, Agent: "claude", Dir: coordinatorDir, ClaudeID: convID("wg1"), Created: 1},
		{ID: "old1", RootID: "wg1", Agent: "claude", Dir: t.TempDir(), ClaudeID: convID("old1"), Created: 2, Archived: true},
		{ID: "old2", RootID: "wg1", Agent: "claude", Dir: t.TempDir(), ClaudeID: convID("old2"), Created: 3, Archived: true},
	}
	if live {
		rows = append(rows, store.Session{ID: "a1", RootID: "wg1", Agent: "claude", Dir: t.TempDir(), ClaudeID: convID("a1"), Created: 4})
	}
	for _, s := range rows {
		if err := db.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
}

// ackOnInput plays the runtime's part: once text reaches the instance for id,
// the Claude UserPromptSubmit hook reports a turn.
func ackOnInput(t *testing.T, eng *fakeEngine, id string) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			eng.mu.Lock()
			in := eng.insts[engine.Key{AgentID: id, Tab: panespec.TabAgent}]
			eng.mu.Unlock()
			if in != nil && in.written() != "" {
				_ = core.WriteSessionHookState(id, convID(id), core.StateRunning, "")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

func promptAction(id, text string) core.Action {
	return core.Action{Action: core.ActionSteer, ID: id, Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: text}}
}

// (a) A prompt to a stopped coordinator whose members are all archived starts
// the coordinator and delivers the text; "ok" arrives only after the runtime
// took the prompt. (c) An interject afterwards succeeds: the agent is running.
func TestPromptStartsStoppedCoordinatorAndDelivers(t *testing.T) {
	d, eng := steerDaemon(t)
	d.agentsUnder = wsops.AgentIDsUnder
	coordinatorWithArchivedMembers(t, false)

	if r := d.handle(context.Background(), core.Action{Action: core.ActionSteer, ID: "wg1", Fields: map[string]string{
		core.SteerVerb: core.SteerInterject, core.SteerText: "hi"}}); r.OK || !strings.Contains(r.Error, "not running") {
		t.Fatalf("interject to a stopped coordinator = %+v, want not running", r)
	}

	ackOnInput(t, eng, "wg1")
	if r := d.handle(context.Background(), promptAction("wg1", "status?")); !r.OK {
		t.Fatalf("prompt to a stopped coordinator: %s", r.Error)
	}
	in, ok := eng.Lookup(engine.Key{AgentID: "wg1", Tab: panespec.TabAgent})
	if !ok || !in.Alive() {
		t.Fatal("prompt reported ok but the coordinator is not running")
	}
	if got := in.(*fakeInstance).written(); !strings.Contains(got, "status?") {
		t.Fatalf("prompt reported ok but the text never reached the runtime: %q", got)
	}
	for _, k := range eng.ensuredKeys() {
		if k.AgentID == "old1" || k.AgentID == "old2" {
			t.Fatalf("a prompt to the coordinator started archived member %s", k.AgentID)
		}
	}

	markBusy(t, "wg1")
	if r := d.handle(context.Background(), core.Action{Action: core.ActionSteer, ID: "wg1", Fields: map[string]string{
		core.SteerVerb: core.SteerInterject, core.SteerText: "and the PR?"}}); !r.OK {
		t.Fatalf("interject after the prompt started the coordinator: %s", r.Error)
	}
	if got := in.(*fakeInstance).written(); !strings.Contains(got, "and the PR?") {
		t.Fatalf("interject not delivered: %q", got)
	}
}

// A prompt never answers "ok" without effect: a start that fails, a runtime
// that exits while booting, and one that never acknowledges each return an
// error saying why.
func TestPromptToStoppedAgentFailsLoudly(t *testing.T) {
	t.Run("start fails", func(t *testing.T) {
		d, eng := steerDaemon(t)
		putSession(t, "a1", "claude")
		eng.ensureErr = errors.New("sandbox refused")
		r := d.handle(context.Background(), promptAction("a1", "go"))
		if r.OK || !strings.Contains(r.Error, "sandbox refused") {
			t.Fatalf("result = %+v, want the start error", r)
		}
	})
	t.Run("exits while booting", func(t *testing.T) {
		d, eng := steerDaemon(t)
		putSession(t, "a1", "claude")
		eng.ensurePublished = func(in engine.Instance) {
			go func() {
				time.Sleep(20 * time.Millisecond)
				f := in.(*fakeInstance)
				f.mu.Lock()
				f.dead = true
				f.mu.Unlock()
			}()
		}
		r := d.handle(context.Background(), promptAction("a1", "go"))
		if r.OK || !strings.Contains(r.Error, "exited while starting") {
			t.Fatalf("result = %+v, want exited while starting", r)
		}
	})
	t.Run("never acknowledges", func(t *testing.T) {
		d, _ := steerDaemon(t)
		d.steerVerify = 300 * time.Millisecond
		putSession(t, "a1", "claude")
		r := d.handle(context.Background(), promptAction("a1", "go"))
		if r.OK || !strings.Contains(r.Error, "did not acknowledge the prompt") {
			t.Fatalf("result = %+v, want no acknowledgement", r)
		}
	})
}

// A runtime with no status hooks (Codex on the PTY) cannot report a turn; it
// counts as having taken the prompt once it stayed alive past the submit.
func TestPromptToHooklessRuntimeAcceptsLiveness(t *testing.T) {
	d, _ := steerDaemon(t)
	putSession(t, "a1", "codex")
	start := time.Now()
	if r := d.handle(context.Background(), promptAction("a1", "go")); !r.OK {
		t.Fatalf("prompt to a live hookless runtime: %s", r.Error)
	}
	if time.Since(start) < steerAckGrace {
		t.Fatal("hookless prompt acknowledged before its liveness grace")
	}
}

// (b) Starting a workgroup root starts the coordinator and its live members and
// skips archived ones; it no longer fails with "session … is archived".
func TestStartRootSkipsArchivedMembers(t *testing.T) {
	d, eng := steerDaemon(t)
	d.agentsUnder = wsops.AgentIDsUnder
	coordinatorWithArchivedMembers(t, true)

	if r := d.handle(context.Background(), core.Action{Action: core.ActionStart, ID: "wg1"}); !r.OK {
		t.Fatalf("start root with archived members: %s", r.Error)
	}
	started := map[string]bool{}
	for _, k := range eng.ensuredKeys() {
		started[k.AgentID] = true
	}
	if !started["wg1"] || !started["a1"] {
		t.Fatalf("start root started %v, want the coordinator wg1 and live member a1", started)
	}
	if started["old1"] || started["old2"] {
		t.Fatalf("start root started an archived member: %v", started)
	}

	// All members archived: the coordinator alone still starts.
	d2, eng2 := steerDaemon(t)
	d2.agentsUnder = wsops.AgentIDsUnder
	coordinatorWithArchivedMembers(t, false)
	if r := d2.handle(context.Background(), core.Action{Action: core.ActionStart, ID: "wg1"}); !r.OK {
		t.Fatalf("start root whose members are all archived: %s", r.Error)
	}
	if _, ok := eng2.Lookup(engine.Key{AgentID: "wg1", Tab: panespec.TabAgent}); !ok {
		t.Fatal("the coordinator did not start")
	}
}

// A daemon restart must not lose a session the restore could not bring back:
// the first poll (which runs before restore, with nothing live) leaves the
// persisted live set alone, and a failed restore is journaled on the session
// so its owner sees it is stopped.
func TestRestoreKeepsLiveSetUntilAttemptedAndJournalsFailures(t *testing.T) {
	d, eng := steerDaemon(t)
	putSession(t, "a1", "claude")
	putSession(t, "a2", "claude")
	path := filepath.Join(t.TempDir(), "live-agents.json")
	d.liveAgentsPath = path
	for _, id := range []string{"a1", "a2"} {
		eng.running(id)
	}
	d.persistLiveAgents()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Restart: a fresh engine, the persisted set read back.
	d2, eng2 := steerDaemon(t)
	d2.liveAgentsPath = path
	d2.pendingRestore = d2.readLiveAgents()
	d2.restorePending.Store(len(d2.pendingRestore) > 0)
	d2.mu.Lock()
	d2.sessions = []core.Session{{ID: "a1"}, {ID: "a2"}}
	d2.mu.Unlock()
	d2.persistLiveAgents() // the first poll
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("first poll overwrote the live set before restore:\n%s\nwas\n%s", after, before)
	}
	eng2.ensureFail = func(spec engine.Spec) error {
		if spec.Key.AgentID == "a1" {
			return errors.New("credential is not current")
		}
		return nil
	}
	d2.restoreLiveAgents(context.Background())
	if d2.restorePending.Load() {
		t.Fatal("restore finished but persisting is still held")
	}
	if _, ok := eng2.Lookup(engine.Key{AgentID: "a2", Tab: panespec.TabAgent}); !ok {
		t.Fatal("a2 was not restored")
	}
	found := false
	for _, r := range core.ReadJournal("a1") {
		if strings.Contains(r.Text, "not restored after the daemon restart") && strings.Contains(r.Text, "credential is not current") {
			found = true
		}
	}
	if !found {
		t.Fatalf("failed restore of a1 not journaled: %+v", core.ReadJournal("a1"))
	}
}
