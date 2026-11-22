package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"amux/internal/agent"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/harnessbin"
	"amux/internal/hostprep"
	"amux/internal/panespec"
	"amux/internal/wsops"
)

// Moving running sessions onto a newly activated harness version.
//
// A launch resolves a managed harness to its concrete version directory, so a
// session keeps the version it started with. When a newer version becomes
// active (automatic update, amux harness update/install/rollforward), the
// daemon restarts each running session that is on an older managed version,
// one session at a time, at a safe point: the agent is idle between turns (not
// mid-turn, not waiting on a permission prompt or question, no prompt or steer
// just delivered or still being typed), and it has a saved conversation the
// relaunch resumes. The relaunch is the agent-restart path (replaceSession):
// the restart intent captured from the old runtime, the same conversation id,
// the harness's resume launch (claude --resume, codex resume, App Server
// thread/resume). A turn is never interrupted: idleness is observed over a
// quiet window and re-checked under the session's admission lock — the lock
// every steer, start and restart of that session takes — right before the old
// process is stopped, so a prompt that arrives meanwhile either lands first
// (and the restart waits for the next idle window) or waits for the new
// runtime.
//
// If the new version fails to launch, the session is relaunched on the version
// it ran before (same resume path), and the update is retried with backoff. A
// replacement that dies within harnessVerifyAfter is treated the same way.
// harnesses.restart_sessions off disables all of this; sessions then pick up
// the new version at their next restart.

const (
	// harnessRestartPoll is how often running sessions are compared with the
	// active managed versions. The pass reads in-memory state and hook files.
	harnessRestartPoll = 20 * time.Second
	// harnessIdleQuiet is how long a session must have been observed idle,
	// and how long since a prompt or steer was delivered to it, before it is
	// restarted. It spans hook-state latency and typed-input settling.
	harnessIdleQuiet = 30 * time.Second
	// harnessVerifyAfter is how long a replacement must stay alive to count as
	// a successful update.
	harnessVerifyAfter = 20 * time.Second
)

// harnessRestartBackoff is the wait before retrying after the n-th failure.
var harnessRestartBackoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour}

func backoffAfter(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	if failures > len(harnessRestartBackoff) {
		failures = len(harnessRestartBackoff)
	}
	return harnessRestartBackoff[failures-1]
}

// harnessRun is the managed harness version one session's live runtime was
// launched from ("" version: not a managed install).
type harnessRun struct {
	kind, version string
	runtime       any // engine.Instance or *codexapp.Supervisor
}

// harnessRestartTrack is one session's pending move onto a newer version.
type harnessRestartTrack struct {
	kind, from, to string
	reason         string
	idleSince      time.Time
	failures       int
	nextAttempt    time.Time
	lastError      string
	// verify is set after a restart onto `to`, until the replacement has
	// stayed up for harnessVerifyAfter.
	verify *harnessVerify
}

type harnessVerify struct {
	runtime any
	at      time.Time
}

type harnessRestartState struct {
	mu        sync.Mutex
	runs      map[string]harnessRun
	tracks    map[string]*harnessRestartTrack
	lastSteer map[string]time.Time
	kick      chan struct{}

	// Seams for tests.
	now       func() time.Time
	settings  func() (harnessbin.Settings, error)
	active    func(kind string, s harnessbin.Settings) (version string, ok bool)
	resumable func(id string) (bool, string)
	quiet     time.Duration
	verify    time.Duration
	// beforeLock runs just before a restart takes the session lock (tests
	// inject a racing prompt there).
	beforeLock func(id string)
}

func (st *harnessRestartState) clock() time.Time {
	if st.now != nil {
		return st.now()
	}
	return time.Now()
}

func (st *harnessRestartState) quietWindow() time.Duration {
	if st.quiet > 0 {
		return st.quiet
	}
	return harnessIdleQuiet
}

func (st *harnessRestartState) verifyWindow() time.Duration {
	if st.verify > 0 {
		return st.verify
	}
	return harnessVerifyAfter
}

// managedArgvVersion finds the managed harness executable in a (possibly
// sandbox-wrapped) launch argv and returns its kind and version.
func managedArgvVersion(argv []string) (kind, version string, ok bool) {
	for _, arg := range argv {
		root, managed := harnessbin.ManagedVersionRoot(arg)
		if !managed {
			continue
		}
		k := filepath.Base(filepath.Dir(root))
		if arg == filepath.Join(root, "bin", k) {
			return k, filepath.Base(root), true
		}
	}
	return "", "", false
}

// noteHarnessLaunch records which managed version a newly published agent
// runtime runs. Every agent launch path (start, restore, restart, auth reload,
// App Server) reaches it through ensurePane or the App Server launch.
func (d *Daemon) noteHarnessLaunch(id string, runtime any, argv []string) {
	if _, pane := runtime.(engine.Instance); pane && d.codex != nil {
		if _, structured := d.codex.Get(id); structured {
			// The `--remote` attach pane of an App Server session: the
			// supervisor is the runtime that holds the conversation, and
			// restarting it replaces the attach pane too.
			return
		}
	}
	st := &d.harnessRestart
	st.mu.Lock()
	defer st.mu.Unlock()
	if prev, ok := st.runs[id]; ok && prev.runtime == runtime {
		return // Ensure returned the runtime already recorded
	}
	if st.runs == nil {
		st.runs = map[string]harnessRun{}
	}
	kind, version, _ := managedArgvVersion(argv)
	st.runs[id] = harnessRun{kind: kind, version: version, runtime: runtime}
}

// noteSteer records that a prompt, steer or permission answer was just
// delivered to id: the session is not at a safe point until the quiet window
// has passed (the keystrokes may still be queued, or the hook state not yet
// updated).
func (d *Daemon) noteSteer(id string) {
	st := &d.harnessRestart
	st.mu.Lock()
	if st.lastSteer == nil {
		st.lastSteer = map[string]time.Time{}
	}
	st.lastSteer[id] = st.clock()
	st.mu.Unlock()
}

// kickHarnessRestart asks the restart loop for a pass now (after an update).
func (d *Daemon) kickHarnessRestart() {
	if ch := d.harnessRestart.kick; ch != nil {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (d *Daemon) harnessRestartLoop(ctx context.Context) {
	t := time.NewTicker(harnessRestartPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-d.harnessRestart.kick:
		}
		d.harnessRestartTick(ctx)
	}
}

// currentRuntime returns id's live agent runtime: the App Server supervisor
// for a structured session, else the agent pane.
func (d *Daemon) currentRuntime(id string) (any, bool) {
	if d.codex != nil {
		if sup, ok := d.codex.Get(id); ok {
			return sup, true
		}
	}
	if d.engine != nil {
		if in, ok := d.engine.Lookup(engine.Key{AgentID: id, Tab: panespec.TabAgent}); ok {
			return in, true
		}
	}
	return nil, false
}

func runtimeAlive(rt any) bool {
	if in, ok := rt.(engine.Instance); ok {
		return in.Alive()
	}
	return rt != nil
}

// activeManagedVersion is the managed version a new launch of kind would run,
// or ok=false when an explicit override (AMUX_<KIND>_BIN,
// harnesses.<kind>.bin) or the absence of a managed install means a restart
// would not move the session onto a managed version.
func activeManagedVersion(kind string, s harnessbin.Settings) (string, bool) {
	if os.Getenv("AMUX_"+strings.ToUpper(kind)+"_BIN") != "" || s.Kind(kind).Bin != "" {
		return "", false
	}
	_, v, ok := harnessbin.ActiveBin(kind)
	return v, ok
}

// harnessResumable reports whether relaunching id resumes its conversation.
func (d *Daemon) harnessResumable(id string) (bool, string) {
	if f := d.harnessRestart.resumable; f != nil {
		return f(id)
	}
	s, ok, err := lookupSession(id)
	if err != nil || !ok {
		return false, "session not found"
	}
	if s.Archived {
		return false, "archived"
	}
	if d.structuredControl(s) {
		if codexapp.CanResume(id, s.ClaudeID) {
			return true, ""
		}
		return false, "no saved thread yet (it has not run a turn)"
	}
	root, err := hostprep.OpenSession(s.Dir)
	if err != nil {
		return false, "cannot open its session directory: " + err.Error()
	}
	defer root.Close()
	return agent.ConversationSaved(root, s, wsops.AcceptedLaunchCwds(s))
}

// sessionAtSafePoint reports whether id's agent is idle between turns: no
// prompt or steer delivered within the quiet window, and, for an App Server
// session, no active turn, starting prompt or open approval; for a PTY
// session, a harness turn state of idle/ready (not running, not waiting on a
// permission prompt) and no question selector open.
func (d *Daemon) sessionAtSafePoint(id string) (bool, string) {
	st := &d.harnessRestart
	st.mu.Lock()
	last, steered := st.lastSteer[id]
	now := st.clock()
	st.mu.Unlock()
	if steered && now.Sub(last) < st.quietWindow() {
		return false, "a prompt was just delivered"
	}
	if d.codex != nil {
		if sup, ok := d.codex.Get(id); ok {
			return sup.AtSafePoint()
		}
	}
	if d.engine == nil {
		return false, "not running"
	}
	k := engine.Key{AgentID: id, Tab: panespec.TabAgent}
	in, ok := d.engine.Lookup(k)
	if !ok || !in.Alive() {
		return false, "not running"
	}
	s, found, err := lookupSession(id)
	if err != nil || !found {
		return false, "session not found"
	}
	h := agent.HarnessFor(s.Agent)
	switch h.Activity(s) {
	case engine.ActivitySafe:
	case engine.ActivityBusy:
		return false, "mid-turn or waiting on a permission prompt"
	default:
		return false, "turn state unknown"
	}
	if d.questionOpen(h, s) {
		return false, "a question is waiting for an answer"
	}
	return true, ""
}

// harnessRestartTick is one pass over the running sessions.
func (d *Daemon) harnessRestartTick(ctx context.Context) {
	st := &d.harnessRestart
	load := st.settings
	if load == nil {
		load = harnessbin.LoadSettings
	}
	settings, err := load()
	if err != nil {
		log.Printf("harness restarts: %v", err)
		return
	}
	active := st.active
	if active == nil {
		active = activeManagedVersion
	}
	now := st.clock()

	st.mu.Lock()
	ids := make([]string, 0, len(st.runs))
	for id := range st.runs {
		ids = append(ids, id)
	}
	st.mu.Unlock()
	sort.Strings(ids)

	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		st.mu.Lock()
		run := st.runs[id]
		tr := st.tracks[id]
		st.mu.Unlock()
		rt, live := d.currentRuntime(id)

		if tr != nil && tr.verify != nil {
			if !d.verifyHarnessRestart(ctx, id, tr, rt, live, now) {
				continue
			}
		}
		if !live || rt != run.runtime {
			// Stopped, or replaced by a launch recorded under its own runtime.
			if !live {
				d.forgetHarnessRun(id, run.runtime)
			}
			continue
		}
		if run.version == "" {
			d.dropTrack(id)
			continue
		}
		to, ok := active(run.kind, settings)
		if !ok || harnessbin.CompareVersions(to, run.version) <= 0 {
			d.dropTrack(id)
			continue
		}
		tr = d.track(id, run.kind, run.version, to)
		d.considerHarnessRestart(ctx, id, run, tr, settings, now)
	}
}

func (d *Daemon) track(id, kind, from, to string) *harnessRestartTrack {
	st := &d.harnessRestart
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.tracks == nil {
		st.tracks = map[string]*harnessRestartTrack{}
	}
	tr := st.tracks[id]
	if tr == nil || tr.to != to || tr.kind != kind {
		// A different target version is a fresh attempt.
		tr = &harnessRestartTrack{kind: kind, from: from, to: to}
		st.tracks[id] = tr
	}
	tr.from = from
	return tr
}

func (d *Daemon) dropTrack(id string) {
	st := &d.harnessRestart
	st.mu.Lock()
	delete(st.tracks, id)
	st.mu.Unlock()
}

func (d *Daemon) forgetHarnessRun(id string, runtime any) {
	st := &d.harnessRestart
	st.mu.Lock()
	if run, ok := st.runs[id]; ok && run.runtime == runtime {
		delete(st.runs, id)
		delete(st.tracks, id)
	}
	st.mu.Unlock()
}

func (d *Daemon) setTrack(tr *harnessRestartTrack, f func(*harnessRestartTrack)) {
	d.harnessRestart.mu.Lock()
	f(tr)
	d.harnessRestart.mu.Unlock()
}

func (d *Daemon) considerHarnessRestart(ctx context.Context, id string, run harnessRun, tr *harnessRestartTrack, settings harnessbin.Settings, now time.Time) {
	if !settings.RestartSessionsEnabled() {
		d.setTrack(tr, func(tr *harnessRestartTrack) {
			tr.reason, tr.idleSince = "harnesses.restart_sessions is off; restart the agent to load it", time.Time{}
		})
		return
	}
	if now.Before(tr.nextAttempt) {
		d.setTrack(tr, func(tr *harnessRestartTrack) {
			tr.reason = fmt.Sprintf("retrying at %s after a failed restart (%s)", tr.nextAttempt.Format(time.Kitchen), tr.lastError)
		})
		return
	}
	if ok, why := d.harnessResumable(id); !ok {
		d.setTrack(tr, func(tr *harnessRestartTrack) {
			tr.reason, tr.idleSince = "not restarted: "+why, time.Time{}
		})
		return
	}
	if ok, why := d.sessionAtSafePoint(id); !ok {
		d.setTrack(tr, func(tr *harnessRestartTrack) {
			tr.reason, tr.idleSince = "waiting for the agent to be idle ("+why+")", time.Time{}
		})
		return
	}
	var wait bool
	d.setTrack(tr, func(tr *harnessRestartTrack) {
		if tr.idleSince.IsZero() {
			tr.idleSince = now
		}
		wait = now.Sub(tr.idleSince) < d.harnessRestart.quietWindow()
		if wait {
			tr.reason = "agent idle; confirming it stays idle"
		}
	})
	if wait {
		return
	}
	d.restartOntoActive(ctx, id, run, tr, now)
}

// restartOntoActive replaces id's runtime with one on the active version,
// falling back to run.version if the new one does not launch.
func (d *Daemon) restartOntoActive(ctx context.Context, id string, run harnessRun, tr *harnessRestartTrack, now time.Time) {
	if f := d.harnessRestart.beforeLock; f != nil {
		f(id)
	}
	release := d.admitSession(id)
	defer release()
	// Re-check under the session's admission lock: a steer, start or restart
	// of this session holds it, so none can begin between this check and the
	// replacement. One that ran just before is visible here as a busy turn
	// state or a recent steer.
	if rt, ok := d.currentRuntime(id); !ok || rt != run.runtime {
		return
	}
	if ok, why := d.sessionAtSafePoint(id); !ok {
		d.setTrack(tr, func(tr *harnessRestartTrack) {
			tr.reason, tr.idleSince = "waiting for the agent to be idle ("+why+")", time.Time{}
		})
		return
	}
	log.Printf("harness restarts: restarting %s onto %s %s (was %s); resuming its conversation", id, run.kind, tr.to, run.version)
	journal(id, core.JournalInfo, fmt.Sprintf("restarting onto %s %s (was %s) for a harness update", run.kind, tr.to, run.version))
	published, err := d.replaceSessionWith(ctx, id, replaceOptions{
		fallback: func(argv []string) ([]string, error) {
			return retargetHarnessArgv(argv, run.kind, tr.to, run.version)
		},
	})
	var fell *fallbackLaunchError
	switch {
	case err == nil:
		d.setTrack(tr, func(tr *harnessRestartTrack) {
			tr.reason = "restarted; confirming the new version stays up"
			tr.verify = &harnessVerify{runtime: published, at: now.Add(d.harnessRestart.verifyWindow())}
		})
	case errors.As(err, &fell):
		log.Printf("harness restarts: %s: %s %s did not launch (%v); resumed on %s", id, run.kind, tr.to, fell.err, run.version)
		journal(id, core.JournalError, fmt.Sprintf("%s %s did not launch; resumed on %s", run.kind, tr.to, run.version))
		d.harnessRestartFailed(tr, fell.err, now)
	default:
		log.Printf("harness restarts: %s: restart onto %s %s failed: %v", id, run.kind, tr.to, err)
		d.harnessRestartFailed(tr, err, now)
		if _, live := d.currentRuntime(id); !live {
			// Nothing runs: bring the conversation back on whatever launches.
			if serr := d.startAgent(ctx, id); serr != nil {
				log.Printf("harness restarts: %s: relaunch after failed restart: %v", id, serr)
			}
		}
	}
}

func (d *Daemon) harnessRestartFailed(tr *harnessRestartTrack, err error, now time.Time) {
	d.setTrack(tr, func(tr *harnessRestartTrack) {
		tr.failures++
		tr.lastError = err.Error()
		tr.nextAttempt = now.Add(backoffAfter(tr.failures))
		tr.idleSince = time.Time{}
		tr.verify = nil
		tr.reason = fmt.Sprintf("restart failed (%v); retrying at %s", err, tr.nextAttempt.Format(time.Kitchen))
	})
}

// verifyHarnessRestart finishes a restart once the replacement has stayed up.
// A replacement that died (still present but not alive) is relaunched on the
// version it replaced. It returns whether the tick should go on to evaluate
// the session normally.
func (d *Daemon) verifyHarnessRestart(ctx context.Context, id string, tr *harnessRestartTrack, rt any, live bool, now time.Time) bool {
	v := tr.verify
	if live && rt != v.runtime {
		// Replaced since (a user restart): nothing left to verify.
		d.setTrack(tr, func(tr *harnessRestartTrack) { tr.verify = nil })
		return true
	}
	if !live {
		// Removed (stopped, archived): never resurrect it.
		d.setTrack(tr, func(tr *harnessRestartTrack) { tr.verify = nil })
		return true
	}
	if runtimeAlive(rt) {
		if now.Before(v.at) {
			return false
		}
		d.dropTrack(id)
		log.Printf("harness restarts: %s now runs %s %s", id, tr.kind, tr.to)
		return true
	}
	// The replacement exited on its own: fall back to the version it replaced.
	log.Printf("harness restarts: %s: %s %s exited after the restart; resuming on %s", id, tr.kind, tr.to, tr.from)
	journal(id, core.JournalError, fmt.Sprintf("%s %s exited after starting; resuming on %s", tr.kind, tr.to, tr.from))
	d.harnessRestartFailed(tr, fmt.Errorf("%s %s exited after starting", tr.kind, tr.to), now)
	from, to, kind := tr.from, tr.to, tr.kind
	release := d.admitSession(id)
	defer release()
	if cur, ok := d.currentRuntime(id); !ok || cur != rt {
		return false
	}
	_, err := d.replaceSessionWith(ctx, id, replaceOptions{
		retarget: func(argv []string) ([]string, error) {
			if old, err := retargetHarnessArgv(argv, kind, to, from); err == nil {
				return old, nil
			}
			return argv, nil // the old version is gone: relaunch on the active one
		},
		// If the old version does not launch either, try the active one again
		// rather than leave the session dead.
		fallback: func(argv []string) ([]string, error) { return argv, nil },
	})
	if err != nil {
		log.Printf("harness restarts: %s: resuming on %s: %v", id, from, err)
	}
	return false
}

// retargetHarnessArgv rewrites a resolved launch argv (and its sandbox
// mounts/profile, which name the version directory) from managed version
// `from` to version `to` of kind. It fails if `to` is no longer installed or
// argv does not run `from`.
func retargetHarnessArgv(argv []string, kind, from, to string) ([]string, error) {
	m := harnessbin.New()
	toBin := m.BinPath(kind, to)
	if fi, err := os.Stat(toBin); err != nil || fi.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("%s %s is no longer installed", kind, to)
	}
	fromRoots := []string{m.VersionDir(kind, from)}
	toRoots := []string{m.VersionDir(kind, to)}
	if real, err := filepath.EvalSymlinks(filepath.Dir(m.VersionDir(kind, to))); err == nil && real != filepath.Dir(m.VersionDir(kind, to)) {
		fromRoots = append(fromRoots, filepath.Join(real, from))
		toRoots = append(toRoots, filepath.Join(real, to))
	}
	out := make([]string, len(argv))
	found := false
	for i, arg := range argv {
		for j := range fromRoots {
			arg = replacePathPrefix(arg, fromRoots[j], toRoots[j])
		}
		if arg == toBin {
			found = true
		}
		out[i] = arg
	}
	if !found {
		return nil, fmt.Errorf("launch does not run %s %s", kind, from)
	}
	return out, nil
}

// replacePathPrefix replaces every occurrence of the directory old in s with
// new, where the occurrence ends s or is followed by a character that cannot
// continue a version directory name.
func replacePathPrefix(s, old, new string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, old)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(old)
		if end < len(s) && versionChar(s[end]) {
			b.WriteString(s[:end])
			s = s[end:]
			continue
		}
		b.WriteString(s[:i])
		b.WriteString(new)
		s = s[end:]
	}
}

func versionChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '.' || c == '-' || c == '+' || c == '_'
}

// HarnessRestartsPending lists sessions waiting to move onto a newer harness
// version, for status/doctor.
func (d *Daemon) harnessRestartsPending() []core.HarnessRestart {
	st := &d.harnessRestart
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]core.HarnessRestart, 0, len(st.tracks))
	for id, tr := range st.tracks {
		out = append(out, core.HarnessRestart{Session: id, Kind: tr.kind, From: tr.from, To: tr.to, Reason: tr.reason})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Session < out[j].Session })
	return out
}
