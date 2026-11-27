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
	"sync/atomic"
	"time"

	"amux/internal/amuxcfg"
	"amux/internal/buildinfo"
	"amux/internal/core"
	"amux/internal/panespec"
	"amux/internal/selfupdate"
	"amux/internal/store"
)

// selfUpdatePoll is how often the daemon re-evaluates self-update: whether a
// release check is due (at most every selfupdate.DefaultCheckInterval, measured
// from the persisted last check) and whether a pending update can be loaded.
const selfUpdatePoll = 5 * time.Minute

// selfUpdateState is the daemon's in-memory self-update bookkeeping.
type selfUpdateState struct {
	// enabled is set only by Default (the host daemon).
	enabled bool
	mu      sync.Mutex
	safe    selfupdate.SafePoint
	pending string // release installed/staged but not loaded
	reason  string
	// restart is set when the daemon shut itself down to load an update; the
	// process entrypoint then re-executes the (replaced) binary.
	restart atomic.Bool
	// newUpdater and live are seams for tests.
	newUpdater func() (*selfupdate.Updater, error)
	live       func() int
	blockers   func() (ids, blocking []string)
}

// CurrentManifest is this build's version and compatibility contracts — what
// `amux version --manifest` prints into each release's amux-release.json.
func CurrentManifest() selfupdate.Manifest {
	return selfupdate.Manifest{
		Version:           buildinfo.Version,
		Commit:            buildinfo.Commit,
		DaemonProtocol:    buildinfo.DaemonProtocol,
		DatabaseMinSchema: store.MinSchemaVersion,
		DatabaseMaxSchema: store.CurrentSchemaVersion,
	}
}

// UpdateStore is the shared self-update state directory.
func UpdateStore() selfupdate.Store {
	return selfupdate.Store{Dir: filepath.Join(core.StateDir(), "update")}
}

// ShimPath is where the shell shim is installed.
func ShimPath() string { return filepath.Join(core.ConfigDir(), "amux.sh") }

// NewUpdater builds the updater for the running executable.
func NewUpdater() (*selfupdate.Updater, error) {
	target, err := selfupdate.ResolveTarget()
	if err != nil {
		return nil, err
	}
	return &selfupdate.Updater{
		Source:   selfupdate.DefaultSource(),
		Store:    UpdateStore(),
		Running:  CurrentManifest(),
		Target:   target,
		ShimPath: ShimPath(),
	}, nil
}

// RestartForUpdate reports whether Run returned because the daemon stopped
// itself to load an installed update; the caller re-executes the binary.
func (d *Daemon) RestartForUpdate() bool { return d.selfUpdate.restart.Load() }

// hostedAgentCount is how many agent instances (PTY panes and supervised App
// Server sessions) this daemon hosts. A daemon restart stops all of them.
func (d *Daemon) hostedAgentCount() int {
	if d.selfUpdate.live != nil {
		return d.selfUpdate.live()
	}
	n := 0
	if d.engine != nil {
		n += len(d.engine.Live())
	}
	if d.codex != nil {
		n += len(d.codex.Live())
	}
	return n
}

func (d *Daemon) updatePending() (string, string) {
	d.selfUpdate.mu.Lock()
	defer d.selfUpdate.mu.Unlock()
	return d.selfUpdate.pending, d.selfUpdate.reason
}

func (d *Daemon) setUpdatePending(version, reason string) {
	d.selfUpdate.mu.Lock()
	d.selfUpdate.pending, d.selfUpdate.reason = version, reason
	d.selfUpdate.mu.Unlock()
}

// selfUpdateLoop is the daemon's automatic self-update. It checks for a new
// release at a bounded rate and installs it without ever stopping a running
// agent: a protocol-compatible release replaces the binary on disk at once
// (new CLI invocations use it; this daemon keeps its code), an incompatible one
// is staged, and the daemon restarts itself into the new binary only once every
// hosted agent has been idle between turns for consecutive polls. Until then the pending update is
// reported by `amux version`, `amux doctor` and `amux ls`.
func (d *Daemon) selfUpdateLoop(ctx context.Context) {
	t := time.NewTimer(3 * time.Minute) // let startup and agent restore settle
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		d.selfUpdateTick(ctx, time.Now())
		t.Reset(selfUpdatePoll)
	}
}

func (d *Daemon) selfUpdateTick(ctx context.Context, now time.Time) {
	newUpdater := d.selfUpdate.newUpdater
	if newUpdater == nil {
		newUpdater = NewUpdater
	}
	u, err := newUpdater()
	if err != nil {
		log.Printf("self-update: %v", err)
		return
	}
	st, err := u.Store.Load()
	if err != nil {
		log.Printf("self-update: %v", err)
	}
	enabled, cfgErr := amuxcfg.ResolveAutoUpdate()
	if cfgErr != nil {
		log.Printf("self-update: %v", cfgErr)
		enabled = false
	}
	due, _ := selfupdate.AutoCheck(selfupdate.AutoInput{
		Running:       u.Running.Version,
		ConfigEnabled: enabled,
		EnvDisabled:   selfupdate.EnvDisabled(os.Getenv),
		LastCheck:     st.LastCheck,
		Now:           now,
	})
	if due {
		d.autoInstall(ctx, u, now)
	}
	d.loadPendingAtSafePoint(ctx, u)
}

// autoInstall checks the latest release and installs or stages it. It holds
// the update lock for the whole operation; a CLI update in progress makes it
// skip this round.
func (d *Daemon) autoInstall(ctx context.Context, u *selfupdate.Updater, now time.Time) {
	unlock, err := u.Store.Lock(time.Second)
	if err != nil {
		if !errors.Is(err, selfupdate.ErrLocked) {
			log.Printf("self-update: %v", err)
		}
		return
	}
	defer unlock()
	st, _ := u.Store.Load()
	// Re-check under the lock: another process may just have checked.
	if !st.LastCheck.IsZero() && !st.LastCheck.After(now) && now.Sub(st.LastCheck) < selfupdate.DefaultCheckInterval {
		return
	}
	st.LastCheck = now
	defer func() {
		if err := u.Store.Save(st); err != nil {
			log.Printf("self-update: save state: %v", err)
		}
	}()
	m, err := u.Source.Latest(ctx)
	if err != nil {
		st.LastError = err.Error()
		log.Printf("self-update: check: %v", err)
		return
	}
	st.Latest = m.Version
	installed, err := u.InstalledVersion(ctx)
	if err != nil {
		st.LastError = fmt.Sprintf("identify installed binary %s: %v", u.Target, err)
		log.Printf("self-update: %s", st.LastError)
		return
	}
	action, why := selfupdate.PlanInstall(m, u.Running, installed)
	switch action {
	case selfupdate.ActionNone:
		st.LastError = ""
		return
	case selfupdate.ActionRefuse:
		st.LastError = why
		log.Printf("self-update: not installing %s: %s", m.Version, why)
		return
	}
	if st.Staged != nil && st.Staged.Version == m.Version {
		return // already staged and verified
	}
	if err := selfupdate.Writable(u.Target); err != nil {
		st.LastError = err.Error()
		log.Printf("self-update: %s available but %v", m.Version, err)
		return
	}
	staged, err := u.Fetch(ctx, m)
	if err != nil {
		st.LastError = err.Error()
		log.Printf("self-update: fetch %s: %v", m.Version, err)
		return
	}
	switch action {
	case selfupdate.ActionSwap:
		if err := u.Apply(&st, staged); err != nil {
			st.LastError = err.Error()
			log.Printf("self-update: install %s: %v", m.Version, err)
			return
		}
		st.Staged = nil
		log.Printf("self-update: installed amux %s at %s; this daemon (%s) restarts into it once every agent is idle between turns", m.Version, u.Target, u.Running.Version)
	case selfupdate.ActionStage:
		st.Staged = staged
		log.Printf("self-update: staged amux %s: %s", m.Version, why)
	}
	st.LastError = ""
}

// loadPendingAtSafePoint restarts the daemon into an installed or staged
// release once, on consecutive polls, every hosted agent is idle between turns
// with a conversation the restart journal resumes (daemonRestartBlockers). It
// never restarts while an agent is mid-turn, waiting on a permission prompt or
// has a prompt in flight: a daemon restart stops the agents it hosts, and the
// journal relaunches them with their conversations.
func (d *Daemon) loadPendingAtSafePoint(ctx context.Context, u *selfupdate.Updater) {
	st, err := u.Store.Load()
	if err != nil {
		return
	}
	pending := st.PendingVersion(u.Running.Version, u.Target)
	if pending == "" {
		d.setUpdatePending("", "")
		return
	}
	if st.Staged == nil {
		// A swapped binary: confirm it is really on disk, so a stale record (a
		// checkout build installed by hand since) cannot cause a restart loop.
		if installed, err := u.InstalledVersion(ctx); err != nil || installed != pending {
			_ = u.Store.Update(func(s *selfupdate.State) {
				if s.Installed == pending {
					s.Installed, s.Pending = "", ""
				}
			})
			d.setUpdatePending("", "")
			return
		}
	}
	ids, blocking := d.daemonRestartBlockers()
	if !d.selfUpdate.safe.Observe(len(blocking)) {
		d.recordPending(u, pending, safePointReason(blocking))
		return
	}
	// Final check under the admission lock of every hosted agent: no steer,
	// start or restart of one can begin between this check and the shutdown,
	// and the restart flag set below refuses steers from then on, so the
	// restart journal captures every agent idle and each relaunches with its
	// conversation.
	release := d.admitSessions(ids...)
	defer release()
	if again, blocking := d.daemonRestartBlockers(); len(blocking) > 0 || !sameMembers(ids, again) {
		d.selfUpdate.safe.Observe(1)
		if len(blocking) == 0 {
			blocking = []string{"the set of running agents changed"}
		}
		d.recordPending(u, pending, safePointReason(blocking))
		return
	}
	if st.Staged != nil {
		version, err := u.ApplyPendingStaged()
		if err != nil {
			log.Printf("self-update: apply staged %s: %v", pending, err)
			d.recordPending(u, pending, "install failed: "+err.Error())
			return
		}
		if version == "" {
			d.setUpdatePending("", "")
			return
		}
	}
	log.Printf("self-update: %d agent(s) hosted, all idle between turns; restarting daemon %s into amux %s (agents resume their conversations)", len(ids), u.Running.Version, pending)
	d.recordPending(u, pending, "restarting now")
	d.selfUpdate.restart.Store(true)
	d.requestShutdown()
}

func safePointReason(blocking []string) string {
	if len(blocking) == 0 {
		return "confirming every agent is idle between turns"
	}
	shown := blocking
	if len(shown) > 3 {
		shown = shown[:3]
	}
	more := ""
	if len(blocking) > len(shown) {
		more = fmt.Sprintf(" and %d more", len(blocking)-len(shown))
	}
	return fmt.Sprintf("waiting for %d running agent(s) to be idle between turns (%s%s); or run amux daemon restart (stops and resumes them)",
		len(blocking), strings.Join(shown, "; "), more)
}

// daemonRestartBlockers lists the hosted agents (ids, sorted) and, for each
// hosted runtime a daemon restart would interrupt or could not bring back,
// why. The restart journal relaunches every agent with its conversation (see
// restartRecord): Claude Code with --resume, Codex with `codex resume`, App
// Server sessions with thread/resume. So an agent blocks only while it is not
// idle between turns (sessionAtSafePoint) or has no saved conversation a
// relaunch would resume (harnessResumable). Editor and terminal panes always
// block: their processes (a shell, an editor buffer) cannot be resumed.
func (d *Daemon) daemonRestartBlockers() (ids, blocking []string) {
	if f := d.selfUpdate.blockers; f != nil {
		return f()
	}
	if d.selfUpdate.live != nil {
		for i := d.selfUpdate.live(); i > 0; i-- {
			blocking = append(blocking, "busy")
		}
		return nil, blocking
	}
	seen := map[string]bool{}
	if d.engine != nil {
		for _, k := range d.engine.Live() {
			if k.Tab != panespec.TabAgent {
				blocking = append(blocking, k.AgentID+": an editor or terminal pane is open")
				continue
			}
			seen[k.AgentID] = true
		}
	}
	if d.codex != nil {
		for id := range d.codex.Live() {
			seen[id] = true
		}
	}
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if ok, why := d.harnessResumable(id); !ok {
			blocking = append(blocking, id+": "+why)
		} else if ok, why := d.sessionAtSafePoint(id); !ok {
			blocking = append(blocking, id+": "+why)
		}
	}
	return ids, blocking
}

func (d *Daemon) recordPending(u *selfupdate.Updater, version, reason string) {
	if v, r := d.updatePending(); v == version && r == reason {
		return
	}
	d.setUpdatePending(version, reason)
	_ = u.Store.Update(func(s *selfupdate.State) {
		s.Pending, s.PendingAt = reason, time.Now()
	})
}
