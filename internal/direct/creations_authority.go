package direct

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"slices"
	"time"
)

// creations_authority.go is the Authority's half of creation tracking: a
// stable creation id per (grant, idem_key), the durable dispatch marker that
// separates "never sent" from "maybe applied", the job's own lease, and status
// lookup. The idempotency record stays the single source of truth.

// errGrantEnded is the cause of a held lease reaching the grant's absolute
// expiry.
var errGrantEnded = errors.New(CodeAuthExpired)

// CreationIDPrefix starts every creation id.
const CreationIDPrefix = "crt_"

func newCreationID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return CreationIDPrefix + base64.RawURLEncoding.EncodeToString(b[:])
}

// validCreationID reports whether id has the shape newCreationID makes.
func validCreationID(id string) bool {
	if len(id) != len(CreationIDPrefix)+22 || id[:len(CreationIDPrefix)] != CreationIDPrefix {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(id[len(CreationIDPrefix):])
	return err == nil
}

// stampCreation marks the just-admitted record for key as a creation. It runs
// in BeginAction's critical section, before the admission commit, so the id
// is durable with the record.
func stampCreation(verb string, async bool) func(*idemRecord) {
	return func(r *idemRecord) { r.CreationID, r.Verb, r.Async = newCreationID(), verb, async }
}

// EnsureCreationID returns the creation id of the record for key, assigning
// one to a record that predates creation ids (a creation recorded by an older
// server). The record's body hash already binds it to this verb and fields.
func (a *Authority) EnsureCreationID(l *Lease, key, verb string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.state.Grants[l.ClientID]
	if g == nil || g.RevokedAt != 0 {
		return "", errAuthRevoked
	}
	r := g.Idem[key]
	if r == nil {
		return "", errNotFound
	}
	if r.CreationID != "" {
		return r.CreationID, nil
	}
	if a.degraded != nil {
		return "", errDegraded
	}
	r.CreationID, r.Verb = newCreationID(), verb
	if err := a.commitLocked(); err != nil {
		if rolledBack(err) {
			r.CreationID, r.Verb = "", ""
		}
		return "", err
	}
	return r.CreationID, nil
}

// MarkDispatched durably records that the creation for key is about to be
// handed to the daemon. Only after it returns nil may the call be made: a
// record without the marker is known never to have been sent. On an error the
// caller must not dispatch.
func (a *Authority) MarkDispatched(l *Lease, key string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l.rctx.Err() != nil {
		return errAuthRevoked
	}
	g := a.state.Grants[l.ClientID]
	if g == nil || g.RevokedAt != 0 {
		return errAuthRevoked
	}
	r := g.Idem[key]
	if r == nil || r.State != idemAdmitted {
		return errNotFound
	}
	if a.degraded != nil {
		return errDegraded
	}
	r.DispatchedAt = a.now().Unix()
	if err := a.commitLocked(); err != nil {
		if rolledBack(err) {
			r.DispatchedAt = 0
		}
		// Not rolled back: the marker may be on disk. Not dispatching keeps
		// it truthful in the conservative direction (a restart reads the
		// record as "maybe applied", never as "did not run" after it ran).
		return err
	}
	return nil
}

// GrantLive reports whether l's grant still authorizes perm right now: not
// revoked, inside its absolute lifetime, of the current identity, and holding
// perm. It is the fence a queued creation passes before dispatch.
func (a *Authority) GrantLive(l *Lease, perm string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || l.rctx.Err() != nil {
		if errors.Is(context.Cause(l.rctx), errGrantEnded) {
			return errAuthExpired
		}
		return errAuthRevoked
	}
	g := a.state.Grants[l.ClientID]
	switch {
	case g == nil || g.RevokedAt != 0 || g.IdentityGeneration != a.identity.Generation:
		return errAuthRevoked
	case a.now().Unix() >= g.AbsoluteExpiry || a.now().Unix() >= g.LastRefreshAt+g.IdleTTL:
		// Read only: neither a queued job nor a status poll ever moves
		// LastRefreshAt, so pending work cannot keep an idle grant alive.
		return errAuthExpired
	case !slices.Contains(g.Perms, perm):
		return errDenied
	}
	return nil
}

// Hold registers a worker lease for l's grant that outlives the request: it
// ends on revocation (cause auth.revoked, drained like any lease), at the
// grant's absolute expiry, or on Done — never on access-token expiry or
// client disconnect. Its Context and ExecContext share that lifetime.
func (a *Authority) Hold(l *Lease, perm string) (*Lease, error) {
	if err := a.GrantLive(l, perm); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.state.Grants[l.ClientID]
	if a.closed || g == nil || g.RevokedAt != 0 {
		return nil, errAuthRevoked
	}
	base, rcancel := context.WithCancelCause(context.Background())
	rctx, dcancel := context.WithDeadlineCause(base, time.Now().Add(time.Unix(g.AbsoluteExpiry, 0).Sub(a.now())), errGrantEnded)
	h := &Lease{
		Grant:     Grant{ClientID: g.ClientID, Perms: slices.Clone(g.Perms), Resource: g.Resource},
		ExpiresAt: g.AbsoluteExpiry, ctx: rctx, rctx: rctx, rcancel: rcancel, done: make(chan struct{}), a: a,
		release: dcancel,
	}
	if a.leases[g.ClientID] == nil {
		a.leases[g.ClientID] = map[*Lease]struct{}{}
	}
	a.leases[g.ClientID][h] = struct{}{}
	return h, nil
}

// creationView is one creation record as status sees it.
type creationView struct {
	rec     idemRecord
	running bool
}

// Creation finds the creation id among l's grant's records. Another grant's
// id, or an unknown one, is not found.
func (a *Authority) Creation(l *Lease, id string) (creationView, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.state.Grants[l.ClientID]
	if g == nil || g.RevokedAt != 0 || id == "" {
		return creationView{}, false
	}
	for k, r := range g.Idem {
		if r.CreationID == id {
			return creationView{rec: *r, running: a.running[runKey(l.ClientID, k)]}, true
		}
	}
	return creationView{}, false
}

// creationKeyView finds the creation record by its idem key.
func (a *Authority) creationByKey(l *Lease, key string) (creationView, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.state.Grants[l.ClientID]
	if g == nil || g.Idem[key] == nil {
		return creationView{}, false
	}
	return creationView{rec: *g.Idem[key], running: a.running[runKey(l.ClientID, key)]}, true
}

// statusOf derives the public status from a record. A record that is admitted
// but not running in this process belongs to a previous process (or a call
// whose outcome was never recorded): dispatched means it may have applied,
// undispatched means it never left this server.
func statusOf(v creationView, now time.Time) CreationStatus {
	r := v.rec
	st := CreationStatus{CreationID: r.CreationID, Verb: r.Verb}
	end := now.Unix()
	switch {
	case r.State == idemAdmitted && v.running && r.Async && r.DispatchedAt == 0:
		st.Phase, st.Version = CreatePhaseQueued, 1
	case r.State == idemAdmitted && v.running:
		// An async job past its dispatch marker, or a synchronous creation in
		// flight (which records no marker and is always treated as sent).
		st.Phase, st.Version = CreatePhasePreparing, 2
	case r.State == idemAdmitted && r.Async && r.DispatchedAt == 0:
		// Only an async record carries the dispatch marker, so only its
		// absence there proves the daemon was never called.
		st.Phase, st.Version, st.Code = CreatePhaseFailed, 3, CodeUnavailable
		st.Error = "the server stopped before the creation was sent; it did not run"
	case r.State == idemDone && r.OK && validSessionID(r.NewID) && validSessionID(r.RootID):
		st.Phase, st.Version, st.NewID, st.RootID = CreatePhaseReady, 3, r.NewID, r.RootID
	case r.State == idemDone && !r.OK && r.Code != "":
		st.Phase, st.Version, st.Code, st.Error = CreatePhaseFailed, 3, r.Code, r.Error
	default:
		// ambiguous, admitted-and-dispatched without a live call, or a
		// success that lacks an id: the outcome is unknown.
		st.Phase, st.Version, st.Code, st.Uncertain = CreatePhaseFailed, 3, CodeIdemAmbiguous, true
		st.Error = "the creation's outcome is unknown; the daemon may have created it. It will not run again with this idem_key; check the session list before retrying with a new one."
	}
	if st.Version == 3 && r.EndedAt != 0 {
		end = r.EndedAt
	}
	if end > r.At {
		st.ElapsedMS = (end - r.At) * 1000
	}
	return st
}
