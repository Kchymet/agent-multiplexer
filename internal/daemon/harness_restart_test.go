package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/harnessbin"
	"amux/internal/panespec"
)

// harnessRestartFixture is a daemon with a fake engine, a claude session
// running on managed version 1.0.0, and a clock the test advances.
type harnessRestartFixture struct {
	t        *testing.T
	d        *Daemon
	e        *fakeEngine
	now      time.Time
	launch   string // the version a launch resolves (the current link)
	active   string // the version the restart loop sees as active
	settings harnessbin.Settings
	resume   bool
}

func installFakeVersion(t *testing.T, kind, version string) {
	t.Helper()
	bin := harnessbin.New().BinPath(kind, version)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeLaunchArgv is what a sandboxed managed launch looks like: the version
// directory is mounted and its binary executed, with the resume flag.
func fakeLaunchArgv(kind, version, conv string) []string {
	m := harnessbin.New()
	root := m.VersionDir(kind, version)
	return []string{"bwrap", "--ro-bind", root, root, "--", m.BinPath(kind, version), "--resume", conv}
}

func newHarnessRestartFixture(t *testing.T, kind string) *harnessRestartFixture {
	t.Helper()
	d, e := steerDaemon(t)
	f := &harnessRestartFixture{t: t, d: d, e: e, now: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC), launch: "1.0.0", active: "1.0.0", resume: true}
	installFakeVersion(t, kind, "1.0.0")
	installFakeVersion(t, kind, "1.1.0")
	putSession(t, "a1", kind)
	d.resolve = func(spec panespec.LaunchSpec, tab int) (string, []string, []string, error) {
		if spec.Session.ClaudeID != convID("a1") {
			t.Fatalf("relaunch changed the conversation: %q", spec.Session.ClaudeID)
		}
		return "", nil, fakeLaunchArgv(kind, f.launch, spec.Session.ClaudeID), nil
	}
	d.harnessRestart.now = func() time.Time { return f.now }
	d.harnessRestart.settings = func() (harnessbin.Settings, error) { return f.settings, nil }
	d.harnessRestart.active = func(string, harnessbin.Settings) (string, bool) { return f.active, true }
	d.harnessRestart.resumable = func(string) (bool, string) {
		if f.resume {
			return true, ""
		}
		return false, "no saved transcript yet (it has not run a turn)"
	}
	if err := d.startAgent(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	f.idle()
	return f
}

func (f *harnessRestartFixture) idle() {
	f.t.Helper()
	if err := core.WriteSessionHookState("a1", convID("a1"), core.StateReady, ""); err != nil {
		f.t.Fatal(err)
	}
}

func (f *harnessRestartFixture) busy() { f.t.Helper(); markBusy(f.t, "a1") }

func (f *harnessRestartFixture) tick(advance time.Duration) {
	f.now = f.now.Add(advance)
	f.d.harnessRestartTick(context.Background())
}

func (f *harnessRestartFixture) pane() *fakeInstance {
	f.t.Helper()
	in, ok := f.e.Lookup(engine.Key{AgentID: "a1", Tab: panespec.TabAgent})
	if !ok {
		f.t.Fatal("agent pane is not running")
	}
	return in.(*fakeInstance)
}

func (f *harnessRestartFixture) lastArgv() []string {
	f.e.mu.Lock()
	defer f.e.mu.Unlock()
	return f.e.specs[len(f.e.specs)-1].Argv
}

func (f *harnessRestartFixture) launches() int {
	f.e.mu.Lock()
	defer f.e.mu.Unlock()
	return len(f.e.specs)
}

func (f *harnessRestartFixture) pending() []core.HarnessRestart { return f.d.harnessRestartsPending() }

func (f *harnessRestartFixture) runVersion() string {
	f.d.harnessRestart.mu.Lock()
	defer f.d.harnessRestart.mu.Unlock()
	return f.d.harnessRestart.runs["a1"].version
}

func (f *harnessRestartFixture) wantReason(sub string) {
	f.t.Helper()
	p := f.pending()
	if len(p) != 1 || !strings.Contains(p[0].Reason, sub) {
		f.t.Fatalf("pending = %+v, want one reason containing %q", p, sub)
	}
}

func runsVersion(argv []string, kind, version string) bool {
	return slices.Contains(argv, harnessbin.New().BinPath(kind, version))
}

func TestManagedArgvVersionAndRetarget(t *testing.T) {
	isolateHome(t)
	installFakeVersion(t, "claude", "1.0.0")
	argv := fakeLaunchArgv("claude", "1.1.0", "c")
	kind, v, ok := managedArgvVersion(argv)
	if !ok || kind != "claude" || v != "1.1.0" {
		t.Fatalf("managedArgvVersion = %q %q %v", kind, v, ok)
	}
	if _, _, ok := managedArgvVersion([]string{"/usr/local/bin/claude", "--resume", "c"}); ok {
		t.Fatal("a PATH binary counted as managed")
	}
	// A seatbelt-style profile names the directory inside a larger string.
	root := harnessbin.New().VersionDir("claude", "1.1.0")
	argv = append(argv, `(allow file-read* (subpath "`+root+`"))`, root+"0/unrelated")
	out, err := retargetHarnessArgv(argv, "claude", "1.1.0", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	old := harnessbin.New().VersionDir("claude", "1.0.0")
	want := []string{"bwrap", "--ro-bind", old, old, "--", harnessbin.New().BinPath("claude", "1.0.0"), "--resume", "c",
		`(allow file-read* (subpath "` + old + `"))`, root + "0/unrelated"}
	if !slices.Equal(out, want) {
		t.Fatalf("retarget =\n %q\nwant\n %q", out, want)
	}
	if _, err := retargetHarnessArgv(argv, "claude", "1.1.0", "0.9.0"); err == nil {
		t.Fatal("retargeted onto a version that is not installed")
	}
}

// The full flow: a newer version becomes active; the session is restarted
// onto it only after it has been idle between turns for the quiet window, and
// the relaunch resumes the same conversation on the new binary.
func TestHarnessRestartMovesIdleSessionOntoNewVersionWithResume(t *testing.T) {
	f := newHarnessRestartFixture(t, "claude")
	old := f.pane()
	if f.runVersion() != "1.0.0" {
		t.Fatalf("launch recorded version %q", f.runVersion())
	}
	f.tick(time.Minute)
	if len(f.pending()) != 0 {
		t.Fatalf("pending with no newer version: %+v", f.pending())
	}

	f.launch, f.active = "1.1.0", "1.1.0"
	f.busy()
	f.tick(time.Second)
	f.wantReason("waiting for the agent to be idle (mid-turn")
	if p := f.pending()[0]; p.Session != "a1" || p.Kind != "claude" || p.From != "1.0.0" || p.To != "1.1.0" {
		t.Fatalf("pending = %+v", p)
	}
	f.idle()
	f.tick(time.Second)
	f.wantReason("confirming it stays idle")
	f.tick(20 * time.Second)
	if f.pane() != old {
		t.Fatal("restarted before the session had been idle for the quiet window")
	}
	// A prompt delivered meanwhile restarts the quiet window.
	f.d.noteSteer("a1")
	f.tick(15 * time.Second)
	f.wantReason("a prompt was just delivered")
	f.tick(31 * time.Second) // steer quiet window over: idle observed again
	f.tick(31 * time.Second) // idle for the quiet window: restart
	cur := f.pane()
	if cur == old || old.Alive() {
		t.Fatal("session was not restarted")
	}
	argv := f.lastArgv()
	if !runsVersion(argv, "claude", "1.1.0") || !slices.Contains(argv, "--resume") || !slices.Contains(argv, convID("a1")) {
		t.Fatalf("relaunch argv %q: want the new binary resuming %s", argv, convID("a1"))
	}
	f.wantReason("confirming the new version stays up")
	f.tick(harnessVerifyAfter + time.Second)
	if len(f.pending()) != 0 || f.runVersion() != "1.1.0" || f.pane() != cur {
		t.Fatalf("after verify: pending %+v version %q", f.pending(), f.runVersion())
	}
}

// Idleness is re-checked under the session lock right before the restart: a
// prompt that lands between the observation and the restart wins, reaches the
// old runtime, and the restart waits for the next idle window.
func TestHarnessRestartRechecksIdlenessUnderSessionLock(t *testing.T) {
	f := newHarnessRestartFixture(t, "claude")
	old := f.pane()
	f.launch, f.active = "1.1.0", "1.1.0"
	raced := false
	f.d.harnessRestart.beforeLock = func(id string) {
		if raced {
			return
		}
		raced = true
		r := f.d.handle(context.Background(), core.Action{Action: core.ActionSteer, ID: id, Fields: map[string]string{
			core.SteerVerb: core.SteerPrompt, core.SteerText: "run the tests",
		}})
		if !r.OK {
			t.Fatalf("racing prompt: %+v", r)
		}
	}
	f.tick(time.Second)
	f.tick(31 * time.Second) // would restart now, but the prompt races in
	if !raced {
		t.Fatal("restart never reached the lock")
	}
	if f.pane() != old || !old.Alive() {
		t.Fatal("restart interrupted a session that had just been prompted")
	}
	if !strings.Contains(old.written(), "run the tests") {
		t.Fatalf("racing prompt did not reach the running agent: %q", old.written())
	}
	f.wantReason("a prompt was just delivered")
	// The turn runs and ends; the restart happens at the next idle window.
	f.busy()
	f.tick(time.Minute)
	f.wantReason("mid-turn")
	f.idle()
	f.tick(time.Second)
	f.tick(31 * time.Second)
	if f.pane() == old || !runsVersion(f.lastArgv(), "claude", "1.1.0") {
		t.Fatal("session not restarted at the next idle window")
	}
}

// A new version that fails to launch leaves the session running its previous
// version (resumed), and the update is retried with backoff.
func TestHarnessRestartFallsBackToOldVersionAndBacksOff(t *testing.T) {
	f := newHarnessRestartFixture(t, "claude")
	old := f.pane()
	f.launch, f.active = "1.1.0", "1.1.0"
	broken := true
	f.e.ensureFail = func(spec engine.Spec) error {
		if broken && runsVersion(spec.Argv, "claude", "1.1.0") {
			return fmt.Errorf("exec format error")
		}
		return nil
	}
	f.tick(time.Second)
	f.tick(31 * time.Second)
	cur := f.pane()
	if cur == old || !cur.Alive() {
		t.Fatal("session left dead after a failed restart")
	}
	argv := f.lastArgv()
	if !runsVersion(argv, "claude", "1.0.0") || !slices.Contains(argv, convID("a1")) {
		t.Fatalf("fallback argv %q: want the old binary resuming the conversation", argv)
	}
	if f.runVersion() != "1.0.0" {
		t.Fatalf("recorded version %q after fallback", f.runVersion())
	}
	f.wantReason("retrying at")
	n := f.launches()
	f.tick(30 * time.Second)
	f.tick(25 * time.Second)
	if f.launches() != n {
		t.Fatal("retried before the backoff elapsed")
	}
	broken = false
	f.tick(10 * time.Second) // backoff (1m) over: idle observed
	f.tick(31 * time.Second)
	if !runsVersion(f.lastArgv(), "claude", "1.1.0") {
		t.Fatalf("not retried after backoff: %q", f.lastArgv())
	}
	f.tick(harnessVerifyAfter + time.Second)
	if len(f.pending()) != 0 {
		t.Fatalf("pending after successful retry: %+v", f.pending())
	}
}

// A replacement that exits right after starting is relaunched on the version
// it replaced, with its conversation.
func TestHarnessRestartCrashedReplacementFallsBack(t *testing.T) {
	f := newHarnessRestartFixture(t, "claude")
	f.launch, f.active = "1.1.0", "1.1.0"
	f.tick(time.Second)
	f.tick(31 * time.Second)
	replacement := f.pane()
	if !runsVersion(f.lastArgv(), "claude", "1.1.0") {
		t.Fatal("not restarted")
	}
	replacement.mu.Lock()
	replacement.dead = true // exited; the engine keeps the dead instance
	replacement.mu.Unlock()
	f.tick(harnessVerifyAfter + time.Second)
	cur := f.pane()
	if cur == replacement || !cur.Alive() {
		t.Fatal("crashed replacement not relaunched")
	}
	if argv := f.lastArgv(); !runsVersion(argv, "claude", "1.0.0") || !slices.Contains(argv, convID("a1")) {
		t.Fatalf("crash fallback argv %q", argv)
	}
	f.wantReason("retrying at")
}

func TestHarnessRestartOptOutAndUnresumableSessions(t *testing.T) {
	f := newHarnessRestartFixture(t, "codex")
	old := f.pane()
	f.launch, f.active = "1.1.0", "1.1.0"
	f.settings = harnessbin.Settings{RestartSessions: "off"}
	f.tick(time.Second)
	f.tick(time.Minute)
	f.wantReason("harnesses.restart_sessions is off")
	f.settings = harnessbin.Settings{}
	f.resume = false
	f.tick(time.Second)
	f.tick(time.Minute)
	f.wantReason("not restarted: no saved transcript")
	if f.pane() != old {
		t.Fatal("restarted an opted-out or unresumable session")
	}
	// A rollback (active older than running) never restarts anything.
	f.resume = true
	f.active = "0.9.0"
	f.tick(time.Minute)
	if len(f.pending()) != 0 || f.pane() != old {
		t.Fatalf("rollback moved a running session: %+v", f.pending())
	}
	// A stopped session is forgotten.
	f.e.Kill(engine.Key{AgentID: "a1", Tab: panespec.TabAgent})
	f.tick(time.Minute)
	if f.runVersion() != "" {
		t.Fatal("stopped session still tracked")
	}
}
