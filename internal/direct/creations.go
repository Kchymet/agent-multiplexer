package direct

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"amux/internal/core"
)

// creations.go is asynchronous creation: a creation verb sent with
// "async": true answers 202 with a CreationStatus as soon as its key is
// durably recorded, and runs as a background job the client follows with
// GET PathCreations/{creation_id}. It exists because preparing a repo and
// worktree routinely outlives actionExecLimit, and a synchronous creation that
// runs out of time is reported idem_ambiguous even though the daemon (which
// runs an accepted action to completion under its own lifetime) usually
// finishes it.
//
// The job uses the same daemon call (Backend.Apply), the same idempotency
// record and the same precheck as the synchronous path. It adds:
//   - its own worker lease (Authority.Hold): revocation and the grant's
//     absolute expiry end it and revoke drains it like any lease; access-token
//     expiry and client disconnect do not;
//   - a fresh fence and precheck after the queue wait, immediately before
//     dispatch, so a revoked, expired, idle or narrowed grant never executes
//     queued work, and a catalog or scope change is honored;
//   - a durable dispatch marker (Authority.MarkDispatched) committed before the
//     daemon call: after a restart, a record without it is known not to have
//     run and one with it is ambiguous. Neither is ever re-run.

// Async creation bounds.
const (
	// asyncCreateLimit bounds one dispatched daemon call (further bounded by
	// the grant's absolute expiry). It matches the Harness relay's create
	// abandon window. Past it the outcome is unknown, not failed.
	asyncCreateLimit = 10 * time.Minute
	// MaxCreationWait caps ?wait= on PathCreations.
	MaxCreationWait = 20 * time.Second
)

// Reservation bounds and the initial wait. Vars for tests.
var (
	// asyncInitialWait is how long the POST waits for a fast creation to
	// finish before answering with its current phase.
	asyncInitialWait = 2 * time.Second

	maxAsyncPerGrant = 4  // pending (queued or preparing) jobs per grant
	maxAsyncPending  = 32 // pending jobs per server
	maxAsyncRunning  = 4  // concurrent daemon calls
)

// creationJobs bounds and signals the async jobs of one Server.
type creationJobs struct {
	mu       sync.Mutex
	perGrant map[string]int
	total    int
	changed  chan struct{} // closed and replaced on every phase change
	slots    chan struct{}
}

func newCreationJobs() *creationJobs {
	return &creationJobs{perGrant: map[string]int{}, changed: make(chan struct{}), slots: make(chan struct{}, maxAsyncRunning)}
}

// reserve takes a pending slot for clientID. The returned release is safe to
// call more than once.
func (j *creationJobs) reserve(clientID string) (func(), bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.perGrant[clientID] >= maxAsyncPerGrant || j.total >= maxAsyncPending {
		return nil, false
	}
	j.perGrant[clientID]++
	j.total++
	return sync.OnceFunc(func() {
		j.mu.Lock()
		defer j.mu.Unlock()
		if j.perGrant[clientID]--; j.perGrant[clientID] <= 0 {
			delete(j.perGrant, clientID)
		}
		j.total--
	}), true
}

// watch returns the channel the next change closes. Take it before reading
// the state it guards, so no change between the read and the wait is missed.
func (j *creationJobs) watch() <-chan struct{} {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.changed
}

func (j *creationJobs) notify() {
	j.mu.Lock()
	defer j.mu.Unlock()
	close(j.changed)
	j.changed = make(chan struct{})
}

// pending reports the reserved job count (tests).
func (j *creationJobs) pending() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.total
}

// runCreationAsync is runAction for an async creation.
func (s *Server) runCreationAsync(w http.ResponseWriter, l *Lease, req ActionRequest, bodyHash string, act core.Action, precheck func(*Lease) ([]string, error)) {
	if !core.DescriptorFor(act.Action).CreatesSession {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "async applies only to creation verbs")
		return
	}
	// An existing record — sync- or async-first, in flight, done or
	// ambiguous — answers with its status under its one creation id. It is
	// never run again, and a different body under the key is a conflict.
	out, _, err := s.cfg.Authority.BeginAction(l, req.IdemKey, bodyHash, true)
	if err != nil {
		writeAuthErr(w, err)
		return
	}
	switch out {
	case idemConflict:
		writeErr(w, http.StatusUnprocessableEntity, CodeIdemConflict, "idem_key was used with a different request")
		return
	case idemReplay, idemInProgress, idemAmbiguous:
		s.answerExisting(w, l, req)
		return
	}
	if _, err := precheck(l); err != nil {
		s.writePrecheckErr(w, l, req.Verb, err)
		return
	}
	release, ok := s.jobs.reserve(l.ClientID)
	if !ok {
		writeErr(w, http.StatusTooManyRequests, CodeRateLimited, "too many creations are in progress; nothing was recorded, retry later")
		return
	}
	launched := false
	defer func() {
		if !launched {
			release()
		}
	}()
	if code := l.AuthEnded(); code != "" {
		writeErr(w, http.StatusUnauthorized, code, "access ended before the creation was recorded")
		return
	}
	out, _, err = s.cfg.Authority.BeginAction(l, req.IdemKey, bodyHash, false, stampCreation(req.Verb, true))
	if s.writeBeginErr(w, err) {
		return
	}
	if out != idemProceed { // raced with a concurrent request on the same key
		if out == idemConflict {
			writeErr(w, http.StatusUnprocessableEntity, CodeIdemConflict, "idem_key was used with a different request")
			return
		}
		s.answerExisting(w, l, req)
		return
	}
	h, err := s.cfg.Authority.Hold(l, VerbPermissions[req.Verb])
	if err != nil {
		// Never dispatched: a known "did not run".
		s.cfg.Authority.FinishAction(l, req.IdemKey, false, authCode(err), "access ended before the creation ran; it did not run")
		writeAuthErr(w, err)
		return
	}
	launched = true
	go s.creationJob(h, release, req.IdemKey, req.Verb, act, precheck)
	s.awaitCreation(w, l, req.IdemKey, http.StatusAccepted)
}

// answerExisting reports the creation already recorded under req's key.
func (s *Server) answerExisting(w http.ResponseWriter, l *Lease, req ActionRequest) {
	if _, err := s.cfg.Authority.EnsureCreationID(l, req.IdemKey, req.Verb); err != nil {
		if errors.Is(err, errAuthRevoked) {
			writeAuthErr(w, err)
			return
		}
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, "could not read the creation's record")
		return
	}
	v, ok := s.cfg.Authority.creationByKey(l, req.IdemKey)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, "could not read the creation's record")
		return
	}
	s.writeStatus(w, l, http.StatusAccepted, statusOf(v, s.cfg.Now()))
}

// awaitCreation answers with the creation's status once it is terminal or
// asyncInitialWait has passed.
func (s *Server) awaitCreation(w http.ResponseWriter, l *Lease, key string, status int) {
	deadline := time.NewTimer(asyncInitialWait)
	defer deadline.Stop()
	for {
		changed := s.jobs.watch()
		v, ok := s.cfg.Authority.creationByKey(l, key)
		if !ok {
			writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, "could not read the creation's record")
			return
		}
		st := statusOf(v, s.cfg.Now())
		if st.Version >= 3 {
			s.writeStatus(w, l, status, st)
			return
		}
		select {
		case <-changed:
		case <-deadline.C:
			s.writeStatus(w, l, status, st)
			return
		case <-l.Context().Done():
			// The creation is recorded and continues; this reader is gone (or
			// its token expired). The client polls with a fresh token.
			if code := l.AuthEnded(); code != "" {
				writeErr(w, http.StatusUnauthorized, code, "access ended; the creation continues — poll its status with a fresh token")
			}
			return
		}
	}
}

func authCode(err error) string {
	switch {
	case errors.Is(err, errAuthExpired):
		return CodeAuthExpired
	case errors.Is(err, errDenied):
		return CodeDenied
	}
	return CodeAuthRevoked
}

// creationJob runs one admitted async creation on its worker lease h. Every
// exit records an outcome and releases its reservation and lease.
func (s *Server) creationJob(h *Lease, release func(), key, verb string, act core.Action, precheck func(*Lease) ([]string, error)) {
	defer s.jobs.notify()
	defer h.Done()
	defer release()
	notRun := func(code, msg string) { s.cfg.Authority.FinishAction(h, key, false, code, msg) }
	select {
	case s.jobs.slots <- struct{}{}:
		defer func() { <-s.jobs.slots }()
	case <-h.Context().Done():
		code := CodeAuthRevoked
		if errors.Is(context.Cause(h.Context()), errGrantEnded) {
			code = CodeAuthExpired
		}
		notRun(code, "access ended while the creation was queued; it did not run")
		return
	}
	// The fence and the destination checks run again now, after the queue
	// wait: the grant, scope, archive state, console exclusion, the row's
	// capability and the catalog/ceiling may all have changed.
	if err := s.cfg.Authority.GrantLive(h, VerbPermissions[verb]); err != nil {
		notRun(authCode(err), "access ended while the creation was queued; it did not run")
		return
	}
	if _, err := precheck(h); err != nil {
		_, code, msg := precheckCode(h, verb, err)
		notRun(code, msg+"; it did not run")
		return
	}
	if err := s.cfg.Authority.MarkDispatched(h, key); err != nil {
		code, msg := CodeUnavailable, "could not record the creation's dispatch; it did not run"
		if errors.Is(err, errAuthRevoked) {
			code, msg = CodeAuthRevoked, "access ended before the creation ran; it did not run"
		}
		notRun(code, msg)
		return
	}
	s.jobs.notify() // queued → preparing
	exec, cancel := h.ExecContext(asyncCreateLimit)
	newID, err := s.cfg.Backend.Apply(exec, act)
	cancel()
	s.settle(h, key, verb, act, nil, newID, err)
}

// creationStatus is GET PathCreations/{creation_id}[?wait=N&after=V]. With
// wait it long-polls until the status version exceeds after (default: the
// version current when the request arrived), the creation is terminal, or
// wait passes; the reader's lease (token expiry, revocation) bounds it too.
func (s *Server) creationStatus(w http.ResponseWriter, r *http.Request) {
	l := s.admit(w, r, PermCreate)
	if l == nil {
		return
	}
	defer l.Done()
	id := r.PathValue("cid")
	if !validCreationID(id) {
		writeErr(w, http.StatusNotFound, CodeNotFound, "no such creation")
		return
	}
	q := r.URL.Query()
	var wait time.Duration
	if v := q.Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, CodeBadRequest, "wait must be a whole number of seconds")
			return
		}
		wait = min(time.Duration(n)*time.Second, MaxCreationWait)
	}
	after := -1
	if v := q.Get("after"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, CodeBadRequest, "after must be a status version")
			return
		}
		after = n
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		changed := s.jobs.watch()
		v, ok := s.cfg.Authority.Creation(l, id)
		if !ok {
			writeErr(w, http.StatusNotFound, CodeNotFound, "no such creation")
			return
		}
		st := statusOf(v, s.cfg.Now())
		if after < 0 {
			after = st.Version
		}
		if wait == 0 || st.Version > after || st.Version >= 3 {
			s.writeStatus(w, l, http.StatusOK, st)
			return
		}
		select {
		case <-changed:
		case <-deadline.C:
			s.writeStatus(w, l, http.StatusOK, st)
			return
		case <-l.Context().Done():
			if code := l.AuthEnded(); code != "" {
				writeErr(w, http.StatusUnauthorized, code, "access ended")
			}
			return
		}
	}
}

// writeStatus discloses a status only while the reader's lease still holds:
// every answer that carries a CreationStatus (the POST and the GET) goes
// through it.
func (s *Server) writeStatus(w http.ResponseWriter, l *Lease, status int, st CreationStatus) {
	if code := l.AuthEnded(); code != "" {
		writeErr(w, http.StatusUnauthorized, code, "access ended; the creation is unaffected — poll its status with a fresh token")
		return
	}
	writeJSON(w, status, st)
}
