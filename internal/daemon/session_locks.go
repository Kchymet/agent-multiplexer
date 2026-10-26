package daemon

import (
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
