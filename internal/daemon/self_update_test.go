package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/selfupdate"
	"amux/internal/selfupdate/releasetest"
)

type selfUpdateFixture struct {
	d      *Daemon
	repo   *releasetest.Repo
	target string
	store  selfupdate.Store
	live   int
}

func probeFake(_ context.Context, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(string(b), "bin:"), nil
}

func newSelfUpdateFixture(t *testing.T, running string) *selfUpdateFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv(selfupdate.DisableEnv, "")
	f := &selfUpdateFixture{repo: releasetest.New(t)}
	f.target = filepath.Join(home, "bin", "amux")
	if err := os.MkdirAll(filepath.Dir(f.target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.target, []byte("bin:"+running), 0o755); err != nil {
		t.Fatal(err)
	}
	f.store = selfupdate.Store{Dir: filepath.Join(home, "state", "update")}
	f.d = New("", nil, time.Second)
	f.d.selfUpdate.newUpdater = func() (*selfupdate.Updater, error) {
		return &selfupdate.Updater{
			Source:  f.repo.Source(),
			Store:   f.store,
			Running: selfupdate.Manifest{Version: running, DaemonProtocol: 2, DatabaseMaxSchema: 3},
			Target:  f.target,
			GOOS:    "linux", GOARCH: "amd64",
			Probe: probeFake,
		}, nil
	}
	f.d.selfUpdate.live = func() int { return f.live }
	return f
}

func (f *selfUpdateFixture) publish(v string, protocol int) {
	f.repo.Publish(selfupdate.Manifest{Version: v, DaemonProtocol: protocol, DatabaseMaxSchema: 3},
		map[string][]byte{"amux-linux-amd64": []byte("bin:" + v)}, nil)
}

func (f *selfUpdateFixture) onDisk(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(f.target)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(string(b), "bin:")
}

func (f *selfUpdateFixture) restarted() bool {
	select {
	case <-f.d.shutdown:
		return f.d.RestartForUpdate()
	default:
		return false
	}
}

func TestSelfUpdateSwapsCompatibleReleaseButWaitsForAgents(t *testing.T) {
	f := newSelfUpdateFixture(t, "0.1.300")
	f.publish("0.1.301", 2)
	f.live = 2
	now := time.Now()
	f.d.selfUpdateTick(t.Context(), now)
	if got := f.onDisk(t); got != "0.1.301" {
		t.Fatalf("on disk = %s, want the compatible release swapped in", got)
	}
	for i := 0; i < 3; i++ {
		f.d.selfUpdateTick(t.Context(), now.Add(time.Duration(i+1)*time.Minute))
	}
	if f.restarted() {
		t.Fatal("daemon restarted while agents were running")
	}
	v, reason := f.d.updatePending()
	if v != "0.1.301" || !strings.Contains(reason, "2 running agent") {
		t.Fatalf("pending = %q %q", v, reason)
	}
	if st, _ := f.store.Load(); !strings.Contains(st.Pending, "2 running agent") {
		t.Fatalf("pending not persisted for the CLI: %+v", st)
	}

	f.live = 0
	f.d.selfUpdateTick(t.Context(), now.Add(10*time.Minute))
	if f.restarted() {
		t.Fatal("one idle poll must not restart")
	}
	f.d.selfUpdateTick(t.Context(), now.Add(15*time.Minute))
	if !f.restarted() {
		t.Fatal("daemon did not restart into the update after consecutive idle polls")
	}
}

func TestSelfUpdateStagesProtocolChangeUntilSafePoint(t *testing.T) {
	f := newSelfUpdateFixture(t, "0.1.300")
	f.publish("0.1.302", 3)
	f.live = 1
	now := time.Now()
	f.d.selfUpdateTick(t.Context(), now)
	if got := f.onDisk(t); got != "0.1.300" {
		t.Fatalf("protocol-changing release swapped under a running daemon: %s", got)
	}
	st, _ := f.store.Load()
	if st.Staged == nil || st.Staged.Version != "0.1.302" {
		t.Fatalf("not staged: %+v", st)
	}
	f.live = 0
	f.d.selfUpdateTick(t.Context(), now.Add(5*time.Minute))
	f.d.selfUpdateTick(t.Context(), now.Add(10*time.Minute))
	if got := f.onDisk(t); got != "0.1.302" {
		t.Fatalf("staged release not installed at the safe point: %s", got)
	}
	if !f.restarted() {
		t.Fatal("daemon did not restart after installing the staged release")
	}
}

func TestSelfUpdateRateLimitsChecks(t *testing.T) {
	f := newSelfUpdateFixture(t, "0.1.300")
	f.publish("0.1.300", 2)
	now := time.Now()
	f.d.selfUpdateTick(t.Context(), now)
	f.d.selfUpdateTick(t.Context(), now.Add(time.Hour))
	if n := countRequests(f.repo, selfupdate.ManifestAsset); n != 1 {
		t.Fatalf("manifest fetched %d times within the interval, want 1", n)
	}
	f.d.selfUpdateTick(t.Context(), now.Add(selfupdate.DefaultCheckInterval+time.Minute))
	if n := countRequests(f.repo, selfupdate.ManifestAsset); n != 2 {
		t.Fatalf("manifest fetched %d times after the interval, want 2", n)
	}
}

func TestSelfUpdateNeverTouchesDevBuildsOrOptOuts(t *testing.T) {
	t.Run("dev daemon", func(t *testing.T) {
		f := newSelfUpdateFixture(t, "0.1.300-dev+abc")
		f.publish("0.1.400", 2)
		f.d.selfUpdateTick(t.Context(), time.Now())
		if len(f.repo.Requests) != 0 || f.onDisk(t) != "0.1.300-dev+abc" {
			t.Fatalf("dev build checked/replaced: %v", f.repo.Requests)
		}
	})
	t.Run("dev build installed under a release daemon", func(t *testing.T) {
		f := newSelfUpdateFixture(t, "0.1.300")
		if err := os.WriteFile(f.target, []byte("bin:0.1.350-dev+abc"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.publish("0.1.400", 2)
		f.d.selfUpdateTick(t.Context(), time.Now())
		if got := f.onDisk(t); got != "0.1.350-dev+abc" {
			t.Fatalf("hand-installed dev build replaced: %s", got)
		}
	})
	t.Run("env opt-out", func(t *testing.T) {
		f := newSelfUpdateFixture(t, "0.1.300")
		t.Setenv(selfupdate.DisableEnv, "1")
		f.publish("0.1.400", 2)
		f.d.selfUpdateTick(t.Context(), time.Now())
		if len(f.repo.Requests) != 0 {
			t.Fatalf("checked despite %s: %v", selfupdate.DisableEnv, f.repo.Requests)
		}
	})
	t.Run("config opt-out", func(t *testing.T) {
		f := newSelfUpdateFixture(t, "0.1.300")
		cfg := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "amux")
		if err := os.MkdirAll(cfg, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg, "config.json"), []byte(`{"update":{"auto":false}}`), 0o644); err != nil {
			t.Fatal(err)
		}
		f.publish("0.1.400", 2)
		f.d.selfUpdateTick(t.Context(), time.Now())
		if len(f.repo.Requests) != 0 {
			t.Fatalf("checked despite update.auto off: %v", f.repo.Requests)
		}
	})
}

func TestSelfUpdateStaleInstallRecordDoesNotRestartLoop(t *testing.T) {
	f := newSelfUpdateFixture(t, "0.1.300")
	// State claims 0.1.301 was installed, but the binary on disk is something
	// else (a checkout build installed since): restarting would re-exec the
	// same binary forever.
	if err := f.store.Save(selfupdate.State{Installed: "0.1.301", Path: f.target, LastCheck: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.target, []byte("bin:0.1.299-dev+x"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		f.d.selfUpdateTick(t.Context(), time.Now())
	}
	if f.restarted() {
		t.Fatal("restarted for a release that is not on disk")
	}
	if st, _ := f.store.Load(); st.Installed != "" {
		t.Fatalf("stale install record kept: %+v", st)
	}
}

func countRequests(r *releasetest.Repo, suffix string) int {
	n := 0
	for _, p := range r.Requests {
		if strings.HasSuffix(p, suffix) {
			n++
		}
	}
	return n
}

// selfUpdateWithAgents gives the fixture a fake engine and a store, and drops
// the live-count seam so the real safe-point check (daemonRestartBlockers) runs.
func (f *selfUpdateFixture) withAgents(t *testing.T) *fakeEngine {
	t.Helper()
	home := os.Getenv("HOME")
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("AMUX_JAIL", "off")
	e := newFakeEngine()
	f.d.engine = e
	f.d.selfUpdate.live = nil
	f.d.harnessRestart.resumable = func(string) (bool, string) { return true, "" }
	return e
}

// The daemon restarts into an update while agents run, once every one is idle
// between turns; a busy agent or an open terminal pane holds it back.
func TestSelfUpdateRestartsWhenEveryAgentIsIdle(t *testing.T) {
	f := newSelfUpdateFixture(t, "0.1.300")
	e := f.withAgents(t)
	putSession(t, "a1", "claude")
	putSession(t, "a2", "codex")
	e.running("a1")
	e.running("a2")
	markBusy(t, "a1")
	if err := core.WriteSessionHookState("a2", convID("a2"), core.StateReady, ""); err != nil {
		t.Fatal(err)
	}
	f.publish("0.1.301", 2)
	now := time.Now()
	for i := 0; i < 3; i++ {
		f.d.selfUpdateTick(t.Context(), now.Add(time.Duration(i)*time.Minute))
	}
	if f.restarted() {
		t.Fatal("restarted while an agent was mid-turn")
	}
	if _, reason := f.d.updatePending(); !strings.Contains(reason, "a1: mid-turn") || strings.Contains(reason, "a2") {
		t.Fatalf("reason = %q", reason)
	}
	if err := core.WriteSessionHookState("a1", convID("a1"), core.StateIdle, ""); err != nil {
		t.Fatal(err)
	}
	terminal := engine.Key{AgentID: "a1", Tab: panespec.TabTerminal}
	e.insts[terminal] = &fakeInstance{key: terminal}
	f.d.selfUpdateTick(t.Context(), now.Add(5*time.Minute))
	f.d.selfUpdateTick(t.Context(), now.Add(10*time.Minute))
	if f.restarted() {
		t.Fatal("restarted with a terminal pane open")
	}
	e.Kill(terminal)
	f.d.selfUpdateTick(t.Context(), now.Add(15*time.Minute))
	if f.restarted() {
		t.Fatal("one idle poll must not restart")
	}
	f.d.selfUpdateTick(t.Context(), now.Add(20*time.Minute))
	if !f.restarted() {
		t.Fatal("did not restart with every agent idle between turns")
	}
	// From now on a prompt is refused rather than typed into a process that
	// is about to stop.
	r := f.d.handle(context.Background(), core.Action{Action: core.ActionSteer, ID: "a1", Fields: map[string]string{
		core.SteerVerb: core.SteerPrompt, core.SteerText: "hello",
	}})
	if r.OK || !strings.Contains(r.Error, "restarting to load an amux update") {
		t.Fatalf("steer during update restart = %+v", r)
	}
}

// The final safe-point check runs under the agents' session locks: an agent
// that turned busy after the poll's observation stops the restart.
func TestSelfUpdateRechecksSafePointBeforeRestart(t *testing.T) {
	f := newSelfUpdateFixture(t, "0.1.300")
	f.d.selfUpdate.live = nil
	calls := 0
	f.d.selfUpdate.blockers = func() ([]string, []string) {
		calls++
		if calls == 3 { // the locked re-check of the second (deciding) poll
			return []string{"a1"}, []string{"a1: a prompt was just delivered"}
		}
		return []string{"a1"}, nil
	}
	f.publish("0.1.301", 2)
	now := time.Now()
	f.d.selfUpdateTick(t.Context(), now)
	f.d.selfUpdateTick(t.Context(), now.Add(5*time.Minute))
	if f.restarted() {
		t.Fatal("restarted although the agent turned busy before the restart")
	}
	if _, reason := f.d.updatePending(); !strings.Contains(reason, "prompt was just delivered") {
		t.Fatalf("reason = %q", reason)
	}
	f.d.selfUpdateTick(t.Context(), now.Add(10*time.Minute))
	f.d.selfUpdateTick(t.Context(), now.Add(15*time.Minute))
	if !f.restarted() {
		t.Fatal("did not restart once idle again")
	}
}
