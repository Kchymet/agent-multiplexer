package daemon

import (
	"context"
	"sort"
	"sync"
	"time"

	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/store"
)

// keyedLocks is one exclusive lock per key (a session id), created on first
// use and dropped when nobody holds or waits for it. docs/daemon-locking.md
// gives the order: take every key a critical section needs in one call, which
// locks them in ascending order, and never add a key while holding some.
type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

// lock acquires every distinct key in ascending order and returns the release.
func (l *keyedLocks) lock(keys ...string) (unlock func()) {
	keys = append([]string(nil), keys...)
	sort.Strings(keys)
	held := make([]*keyedLock, 0, len(keys))
	names := make([]string, 0, len(keys))
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*keyedLock)
	}
	for i, key := range keys {
		if i > 0 && key == keys[i-1] {
			continue
		}
		entry := l.locks[key]
		if entry == nil {
			entry = &keyedLock{}
			l.locks[key] = entry
		}
		entry.refs++
		held = append(held, entry)
		names = append(names, key)
	}
	l.mu.Unlock()
	for _, entry := range held {
		entry.mu.Lock()
	}
	return func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i].mu.Unlock()
		}
		l.mu.Lock()
		for i, entry := range held {
			if entry.refs--; entry.refs == 0 {
				delete(l.locks, names[i])
			}
		}
		l.mu.Unlock()
	}
}

// admitSession is effect admission for work that reads and writes only one
// session's own state: its store row, runtime generation and per-session
// files. It takes the daemon gate shared, so holders that have not been split
// yet (they take it exclusively) still exclude it, then the session's lock, so
// two sessions proceed in parallel while one session's effects stay ordered.
//
// It skips dispatchMu. That lock exists for the restore drain, which fences
// completions, and session-scoped work never begins one. This is not the
// exclusive gate, so callers do not mark their context withEffectAdmission:
// d.handle would then skip the exclusive gate it still needs.
func (d *Daemon) admitSession(id string) (release func()) {
	d.effectMu.RLock()
	unlock := d.sessionLocks.lock(id)
	return func() {
		unlock()
		d.effectMu.RUnlock()
	}
}

// admitSessions is admitSession for several sessions at once: the gate
// shared, then every named session's lock in one sorted acquisition. Empty
// ids are ignored.
func (d *Daemon) admitSessions(ids ...string) (release func()) {
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" {
			keys = append(keys, id)
		}
	}
	d.effectMu.RLock()
	unlock := d.sessionLocks.lock(keys...)
	return func() {
		unlock()
		d.effectMu.RUnlock()
	}
}

type guardSubjectContextKey struct{}

// withGuardSubject records the session whose authority the access guard on ctx
// checks. Work admitted with only some sessions' locks also locks this one, so
// an archive or delete of the caller cannot cross its guard.
func withGuardSubject(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, guardSubjectContextKey{}, id)
}

func guardSubject(ctx context.Context) string {
	id, _ := ctx.Value(guardSubjectContextKey{}).(string)
	return id
}

// admitShared is admission for work that only reads policy or state: it takes
// the gate shared and no session lock.
func (d *Daemon) admitShared() (release func()) {
	d.effectMu.RLock()
	return d.effectMu.RUnlock
}

// tryLock acquires key only if it is free right now.
func (l *keyedLocks) tryLock(key string) (unlock func(), ok bool) {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = make(map[string]*keyedLock)
	}
	entry := l.locks[key]
	if entry == nil {
		entry = &keyedLock{}
		l.locks[key] = entry
	}
	entry.refs++
	l.mu.Unlock()
	drop := func() {
		l.mu.Lock()
		if entry.refs--; entry.refs == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
	if !entry.mu.TryLock() {
		drop()
		return nil, false
	}
	return func() {
		entry.mu.Unlock()
		drop()
	}, true
}

// tryAdmitSession is admitSession for a caller that must not queue: it fails
// instead of waiting for an exclusive gate holder or the session's lock.
func (d *Daemon) tryAdmitSession(id string) (release func(), ok bool) {
	if !d.effectMu.TryRLock() {
		return nil, false
	}
	unlock, ok := d.sessionLocks.tryLock(id)
	if !ok {
		d.effectMu.RUnlock()
		return nil, false
	}
	return func() {
		unlock()
		d.effectMu.RUnlock()
	}, true
}

// effectRetryMax caps lockEffectExclusive's backoff between attempts.
const effectRetryMax = 2 * time.Millisecond

// lockEffectExclusive takes the gate exclusively without queueing on it. A
// writer blocked in sync.RWMutex.Lock stalls every new reader, so one
// exclusive holder waiting behind a slow shared holder (a session's restart,
// say) would stall every other session's reports until both finish. Retrying
// TryLock leaves readers flowing; the writer gets in at the first gap.
func (d *Daemon) lockEffectExclusive() {
	wait := 50 * time.Microsecond
	for !d.effectMu.TryLock() {
		time.Sleep(wait)
		if wait < effectRetryMax {
			wait *= 2
		}
	}
}

// creationAction reports a verb that creates a session. It runs under the
// shared gate and creationMu rather than the exclusive gate.
func creationAction(a core.Action) bool {
	switch a.Action {
	case core.ActionAddAgent, core.ActionNewWorkgroup, core.ActionCreateWorkspace:
		return true
	}
	return false
}

// workgroupStart reports a host `start` of a workgroup root, which starts
// every member under the members' own locks.
func workgroupStart(a core.Action) bool {
	if a.Action != core.ActionStart || a.ID == "" || a.ID == console.ID {
		return false
	}
	db, err := store.OpenReadOnly()
	if err != nil {
		return false
	}
	defer db.Close()
	session, found, err := db.GetSession(a.ID)
	return err == nil && found && session.IsRoot()
}

// archiveScopedAction names the sessions an archive, rename or delete
// touches when it runs under their locks instead of the exclusive gate: the
// target, plus every member when the target is a workgroup root. Restores and
// the one-key archive toggle (which may restore) stay exclusive: a restore
// regrants credentials and drains completions under dispatchMu.
func archiveScopedAction(a core.Action) (ids []string, creation bool, ok bool) {
	switch a.Action {
	case core.ActionSetArchived:
		if a.Fields["archived"] != "true" {
			return nil, false, false
		}
		creation = true
	case core.ActionDelete, core.ActionKill:
		creation = true
	case core.ActionRename:
	default:
		return nil, false, false
	}
	if a.ID == "" || a.ID == console.ID {
		return nil, false, false
	}
	ids, found := sessionAndMembers(a.ID)
	if !found {
		return nil, false, false
	}
	return ids, creation, true
}

// sessionAndMembers returns id and, for a workgroup root, its members.
func sessionAndMembers(id string) ([]string, bool) {
	db, err := store.OpenReadOnly()
	if err != nil {
		return nil, false
	}
	defer db.Close()
	session, found, err := db.GetSession(id)
	if err != nil || !found {
		return nil, false
	}
	ids := []string{session.ID}
	if session.IsRoot() {
		children, err := db.Children(id)
		if err != nil {
			return nil, false
		}
		for _, child := range children {
			ids = append(ids, child.ID)
		}
	}
	return ids, true
}

// sameMembers reports whether two resolutions of a session's members match.
func sameMembers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, id := range a {
		seen[id] = true
	}
	for _, id := range b {
		if !seen[id] {
			return false
		}
	}
	return true
}

// repoScopedAction reports a repo action that runs under the shared gate and
// creationMu instead of the exclusive gate, with the sessions it changes:
// tracking a new repo (none: its home session is a creation) or re-scoping
// one agent's repos (that agent).
func repoScopedAction(a core.Action) ([]string, bool) {
	switch a.Action {
	case core.ActionAddRepo:
		return nil, true
	case core.ActionAgentSetRepos:
		if a.ID == "" || a.ID == console.ID {
			return nil, false
		}
		db, err := store.OpenReadOnly()
		if err != nil {
			return nil, false
		}
		defer db.Close()
		session, found, err := db.GetSession(a.ID)
		if err != nil || !found || session.IsRoot() {
			return nil, false
		}
		return []string{session.ID}, true
	}
	return nil, false
}

// splitAction reports whether handle admits a by resource rather than with the
// exclusive gate.
func splitAction(a core.Action) bool {
	if _, ok := sessionScopedAction(a); ok {
		return true
	}
	if _, _, ok := archiveScopedAction(a); ok {
		return true
	}
	if _, ok := repoScopedAction(a); ok {
		return true
	}
	return creationAction(a) || workgroupStart(a)
}

// sessionScopedAction names the one session an action touches when it runs
// with only that session's admission: starting, restarting, recreating or
// steering one existing session. Anything else, a workgroup start included
// (it starts every member), keeps the exclusive gate.
func sessionScopedAction(a core.Action) (string, bool) {
	switch a.Action {
	case core.ActionStart, core.ActionRestart, actionSessionRecreate, core.ActionSteer:
	default:
		return "", false
	}
	if a.ID == "" || a.ID == console.ID {
		return "", false
	}
	db, err := store.OpenReadOnly()
	if err != nil {
		return "", false
	}
	defer db.Close()
	session, found, err := db.GetSession(a.ID)
	if err != nil || !found || (a.Action == core.ActionStart && session.IsRoot()) {
		return "", false
	}
	return session.ID, true
}
