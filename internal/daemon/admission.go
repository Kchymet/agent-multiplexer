package daemon

import (
	"context"
	"sync"
	"time"

	"amux/internal/core"
)

// The admission gate is the pair (sessionRuntime.dispatchMu, Daemon.effectMu),
// always taken in that order. It guarantees that an authorization change —
// credential revocation or rotation, archive/restore, deletion, a move or a
// grant change — never commits between a caller's last authorization check
// and the effect that check admitted.
//
// Only those authorization changes need the gate exclusively. Everything else
// is an effect that must merely not overlap one, so it holds the gate shared:
//
//   - shared: session self reports and captures (plus a per-subject lock),
//     runtime-event and read-only query admission, credential broker reads
//     (plus credentialMu), deferred steer delivery, and host/session actions
//     that only create or start work (creation, start, refresh, steer).
//   - exclusive: every other action, reconcile when it opens, renews, closes or
//     revokes a mailbox, completion revocation, account changes, shutdown.
//
// Host and session actions are additionally serialized among themselves by
// Daemon.actionMu (taken before the gate), exactly as the single gate used to
// serialize them, so creations never race each other or a policy change. A
// slow creation therefore delays the next action but never another session's
// reports, and session A's reports never wait on session B's.
//
// Exclusive holders acquire politely (lockAdmissionExclusive): they retry
// TryLock instead of queueing as a pending writer, because a queued writer on
// a sync.RWMutex blocks every new reader — one waiting revocation would
// otherwise stall all reports behind a slow shared holder.
type admissionMode int

const (
	admitShared admissionMode = iota + 1
	admitExclusive
)

type effectAdmissionContextKey struct{}

func withAdmission(ctx context.Context, mode admissionMode) context.Context {
	return context.WithValue(ctx, effectAdmissionContextKey{}, mode)
}

// withEffectAdmission marks ctx as running under exclusive admission.
func withEffectAdmission(ctx context.Context) context.Context {
	return withAdmission(ctx, admitExclusive)
}

// effectAdmissionHeld reports whether ctx runs under the gate in either mode.
func effectAdmissionHeld(ctx context.Context) bool {
	mode, _ := ctx.Value(effectAdmissionContextKey{}).(admissionMode)
	return mode != 0
}

// admissionPoll is how often a polite exclusive acquirer retries.
const admissionPoll = time.Millisecond

// lockAdmission takes dispatch (when non-nil) and effect in mode and returns
// the release. ctx only bounds a polite exclusive wait; a cancelled wait
// returns ok=false with nothing held.
func lockAdmission(ctx context.Context, dispatch, effect *sync.RWMutex, mode admissionMode) (release func(), ok bool) {
	if mode == admitShared {
		if dispatch != nil {
			dispatch.RLock()
		}
		effect.RLock()
		return func() {
			effect.RUnlock()
			if dispatch != nil {
				dispatch.RUnlock()
			}
		}, true
	}
	for {
		if tryLockExclusive(dispatch, effect) {
			return func() {
				effect.Unlock()
				if dispatch != nil {
					dispatch.Unlock()
				}
			}, true
		}
		select {
		case <-ctx.Done():
			return func() {}, false
		case <-time.After(admissionPoll):
		}
	}
}

func tryLockExclusive(dispatch, effect *sync.RWMutex) bool {
	if dispatch != nil && !dispatch.TryLock() {
		return false
	}
	if !effect.TryLock() {
		if dispatch != nil {
			dispatch.Unlock()
		}
		return false
	}
	return true
}

// sharedAdmissionAction reports whether a host/session action only creates or
// starts work, so it may run under shared admission (still serialized with
// every other action by actionMu). Anything that could change who may do what
// — archive, delete, move, grants, accounts, restart/replacement — stays
// exclusive. Unknown actions default to exclusive.
func sharedAdmissionAction(a core.Action) bool {
	if hostRestore, _ := hostRestoreRequested(a); hostRestore {
		return false
	}
	switch a.Action {
	case "", core.ActionRefresh, core.ActionStart, core.ActionSteer:
		return true
	}
	return core.DescriptorFor(a.Action).CreatesSession
}

// subjectLocks serializes one subject's self-report effects without touching
// any other subject.
type subjectLocks struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func (s *subjectLocks) lock(subject string) func() {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = make(map[string]*sync.Mutex)
	}
	l := s.locks[subject]
	if l == nil {
		l = &sync.Mutex{}
		s.locks[subject] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func admissionModeName(mode admissionMode) string {
	if mode == admitShared {
		return "shared"
	}
	return "exclusive"
}
