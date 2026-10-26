package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"amux/internal/access"
	"amux/internal/console"
	"amux/internal/engine"
	"amux/internal/store"
)

// reconcile brings every session's mailbox server and credential in line with
// the store, waiting for whatever locks that takes. Startup uses it so every
// current session is served before the daemon reports ready.
func (r *sessionRuntime) reconcile(ctx context.Context) error {
	return r.reconcilePass(ctx, true)
}

// reconcileTick is the serving loop's pass. It runs on the goroutine that
// starts mailbox passes, so it never queues for a lock: a session (or the host
// credential) whose lock is busy is left for the next tick.
func (r *sessionRuntime) reconcileTick(ctx context.Context) error {
	return r.reconcilePass(ctx, false)
}

// reconcilePass scans the store with no lock, skips the sessions with nothing
// to do using lock-free reads, and changes each remaining session under that
// session's own admission, rereading its row there. A host action that changes
// a session takes the gate exclusively, so the reread sees its result and the
// pass never acts on the stale scan.
func (r *sessionRuntime) reconcilePass(ctx context.Context, wait bool) error {
	admit := func(id string) (func(), bool) {
		if wait {
			return r.d.admitSession(id), true
		}
		return r.d.tryAdmitSession(id)
	}
	if err := r.ensureHostCredentialShared(ctx, wait); err != nil {
		return fmt.Errorf("renew host credential: %w", err)
	}
	db, err := store.Open()
	if err != nil {
		return err
	}
	sessions, err := db.AllSessions()
	closeErr := db.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	sessions = append(sessions, console.Session())
	current := make(map[string]store.Session, len(sessions))
	for _, session := range sessions {
		current[session.ID] = session
	}
	for i, session := range sessions {
		if r.progress != nil {
			r.progress(i, len(sessions))
		}
		// The durable parent flag also fences active members after a restart.
		session.Archived = session.Archived || current[session.RootID].Archived
		if !r.reconcileNeeded(ctx, session) {
			continue
		}
		if release, ok := admit(session.ID); ok {
			r.reconcileSession(ctx, session.ID, wait)
			release()
		}
	}
	if r.progress != nil {
		r.progress(len(sessions), len(sessions))
	}
	for id := range r.servers {
		if _, ok := current[id]; ok || r.completions.has(id) {
			continue
		}
		if release, ok := admit(id); ok {
			r.reconcileSession(ctx, id, wait)
			release()
		}
	}
	return nil
}

// reconcileNeeded is the lock-free pre-check. It may say yes needlessly (the
// locked pass rechecks), but says no only when the scan shows nothing to do.
func (r *sessionRuntime) reconcileNeeded(ctx context.Context, session store.Session) bool {
	if session.Archived {
		// A pending completion retains only its existing server and key.
		return !r.completions.has(session.ID) && !r.retired(session.ID)
	}
	if r.servers[session.ID] == nil {
		return !r.retiring(session.ID)
	}
	return !r.credentialFresh(ctx, session.ID)
}

// reconcileSession runs under id's admission. It rereads the session, so a
// restore, archive or deletion that committed after the scan is honored. It
// opens a mailbox only while no creation is in progress (creationMu), waiting
// for one when wait is set and otherwise leaving it to the next tick.
func (r *sessionRuntime) reconcileSession(ctx context.Context, id string, wait bool) {
	// Reread from the store the scan used, not the policy resolver: cleanup of
	// an archive whose commit outcome is uncertain must not depend on policy
	// reads succeeding.
	db, err := store.Open()
	if err != nil {
		log.Printf("session RPC %s lookup: %v", id, err)
		return
	}
	session, found, err := lookupPolicySession(db, id)
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		log.Printf("session RPC %s lookup: %v", id, err)
		return
	}
	switch {
	case r.completions.has(id) && (!found || session.Archived):
		// A pending completion retains only its existing server and key.
	case !found:
		if r.servers[id] != nil {
			r.closeAndRevoke(ctx, id)
		}
	case session.Archived:
		r.closeAndRevoke(ctx, id)
	case r.servers[id] != nil:
		r.renewCurrent(ctx, session)
	case r.retiring(id):
	default:
		if wait {
			r.d.creationMu.Lock()
		} else if !r.d.creationMu.TryLock() {
			return
		}
		defer r.d.creationMu.Unlock()
		if err := r.open(ctx, session); err != nil {
			log.Printf("session RPC %s unavailable: %v", id, err)
		}
	}
}

// retired reports that an archived session has nothing left to retire: no
// server, no current credential, and no runtime. Reconcile used to revoke and
// kill every archived session on every tick.
func (r *sessionRuntime) retired(id string) bool {
	if r.servers[id] != nil || r.d.authority.HasCurrent(access.SubjectSession, id) {
		return false
	}
	if _, live := r.d.permissions.generation(id); live {
		return false
	}
	if r.d.engine != nil {
		for tab := 0; tab < 3; tab++ {
			if _, ok := r.d.engine.Lookup(engine.Key{AgentID: id, Tab: tab}); ok {
				return false
			}
		}
	}
	if r.d.codex != nil {
		if _, ok := r.d.codex.Get(id); ok {
			return false
		}
	}
	return true
}

// credentialFresh reports, without a lock, that renewCurrent would do nothing:
// the served credential is valid and not yet due for rotation.
func (r *sessionRuntime) credentialFresh(ctx context.Context, id string) bool {
	credential, err := r.loadCredential(id)
	if errors.Is(err, os.ErrNotExist) {
		return true // launchSpec performs first provisioning
	}
	if err != nil {
		return false
	}
	return r.d.authority.Valid(ctx, credentialPrincipal(credential)) == nil &&
		r.now().Add(credentialRenewBefore).Before(time.UnixMilli(credential.NotAfter))
}

// ensureHostCredentialShared renews the host credential under the shared
// gate: host actions check the host principal under the exclusive gate, so a
// rotation cannot land between such a check and its effect.
func (r *sessionRuntime) ensureHostCredentialShared(ctx context.Context, wait bool) error {
	if wait {
		r.d.effectMu.RLock()
	} else if !r.d.effectMu.TryRLock() {
		return nil
	}
	defer r.d.effectMu.RUnlock()
	return r.d.ensureHostCredential(ctx, r.now())
}
