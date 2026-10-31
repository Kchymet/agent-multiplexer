package sessionrpc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"amux/internal/access"
	"golang.org/x/sys/unix"
)

type ServerOptions struct {
	Clock              func() time.Time
	Random             io.Reader
	PollInterval       time.Duration
	SettlementInterval time.Duration
	ResponseBudget     ResponseBudget
	// RequestNotify, if set, is called after a request may have been published
	// to this mailbox, so a caller that schedules ServeOnce itself can serve it
	// at once instead of on its next tick. It runs on the process-wide watcher
	// goroutine and must not block. It is never called for a mailbox whose
	// directory cannot be watched (see Watched).
	RequestNotify func()
	// Slow, if set, reports whether a call may run long enough that this
	// subject's other requests must not wait behind it. Slow calls are
	// dispatched in arrival order by one worker per mailbox, at most
	// MaxSlowRequests at a time; every other call is dispatched in arrival
	// order by ServeOnce itself and never waits for a slow one. It sees the
	// call before authentication and may only choose a lane.
	Slow func(Call) bool
	// SlowRequest, if set, receives the timing of every request that took at
	// least SlowRequestThreshold from the start of its processing to its
	// response, so a stall can be attributed to the phase that held it.
	SlowRequest func(RequestTiming)
}

// SlowRequestThreshold is the processing time that reports a request to
// ServerOptions.SlowRequest.
const SlowRequestThreshold = time.Second

// RequestTiming breaks down one request's processing. Verb, Kind and Age come
// from the request before authentication and are for diagnosis only.
type RequestTiming struct {
	SubjectID string
	RequestID string
	Kind      CallKind
	Verb      string
	Status    Status
	Code      string
	// Age is how long before processing began the request says it was issued.
	Age time.Duration
	// Reserve, Verify, Authorize (both credential rechecks and the policy
	// callback), Dispatch, Sign and Publish partition Total, apart from
	// decoding and bookkeeping.
	Reserve, Verify, Authorize, Dispatch, Sign, Publish, Total time.Duration
	// LockWait is the time spent waiting for the authority's lock anywhere in
	// the request, including inside the callbacks; it overlaps the phases.
	LockWait time.Duration
}

// requestTimer accumulates one request's RequestTiming.
type requestTimer struct {
	RequestTiming
	start    time.Time
	lockWait *access.LockWait
}

func (s *Server) startTimer(envelope access.SignedRequest, routed Call) *requestTimer {
	now := time.Now()
	t := &requestTimer{start: now}
	t.SubjectID, t.RequestID, t.Kind = s.subjectID, envelope.RequestID, routed.Kind
	if validCode(routed.Verb) {
		t.Verb = routed.Verb
	}
	if envelope.IssuedAt > 0 {
		t.Age = s.clock().Sub(time.UnixMilli(envelope.IssuedAt))
	}
	return t
}

func (s *Server) reportTimer(t *requestTimer) {
	t.Total = time.Since(t.start)
	if t.Total < SlowRequestThreshold {
		return
	}
	t.LockWait = t.lockWait.Total()
	s.slowRequest(t.RequestTiming)
}

// phase adds the time since *mark to d and moves the mark.
func (t *requestTimer) phase(d *time.Duration, mark *time.Time) {
	now := time.Now()
	*d += now.Sub(*mark)
	*mark = now
}

// MaxSlowRequests bounds one mailbox's claimed slow-lane requests. Further
// slow requests stay claimed until the lane has room.
const MaxSlowRequests = 8

type slowJob struct {
	ctx  context.Context
	name string
}

type pendingReceipt struct {
	principal access.Principal
	digest    string
	deadline  time.Time
	hooks     ReceiptHooks
	ready     bool
	expired   bool
}

type responseReservation struct {
	timer     *requestTimer
	lease     ResponseLease
	committed bool
	// discard marks a one-way receipt: the client never reads a result for
	// it, so none is published or accounted.
	discard bool
}

func (r *responseReservation) release() {
	if r != nil && r.lease != nil && !r.committed {
		r.lease.Release()
		r.lease = nil
	}
}

type Server struct {
	subjectID      string
	authority      Authority
	signer         IssuerSigner
	callbacks      Callbacks
	clock          func() time.Time
	random         io.Reader
	poll           time.Duration
	settlementPoll time.Duration
	responseWriter func(*os.File, string, []byte, io.Reader) error
	responseBudget ResponseBudget

	mailbox         *os.File
	requests        *os.File
	processing      *os.File
	responses       *os.File
	responseCleanup *os.File
	responseUsage   *os.File
	// watch reports request publication; nil when the requests directory
	// cannot be watched and callers must keep polling.
	watch  *dirWatch
	wake   wakeSignal
	notify func()
	// slowRequest is ServerOptions.SlowRequest.
	slowRequest func(RequestTiming)

	// slow-lane state; inflight is guarded by mu. publishMu serializes response
	// publication with itself (its capacity scan rewinds a shared directory
	// stream) and with cleanupResponses, which removes temporaries it does not
	// recognize, now that the slow lane publishes concurrently with ServeOnce.
	slow      func(Call) bool
	slowJobs  chan slowJob
	inflight  map[string]struct{}
	slowStop  chan struct{}
	slowDone  chan struct{}
	publishMu sync.Mutex

	serveMu sync.Mutex
	// rejectRequests drains an over-capacity client-writable queue without ever
	// dispatching a request from that batch. It clears only after observing empty.
	rejectRequests            bool
	mu                        sync.Mutex
	pending                   map[string]pendingReceipt
	closed                    bool
	settlementStop            chan struct{}
	settlementDone            chan struct{}
	settlementStopOnce        sync.Once
	responseScrubComplete     bool
	responseBudgetInitialized bool
	responseAdoptionSeen      map[string]struct{}
	budgetMu                  sync.Mutex
	responseLeases            map[string]ResponseLease
}

// OpenServerMailbox accepts a path only from daemon-owned SessionAccess data.
// It walks that trusted path without following symlinks and retains descriptors
// for all subsequent operations. No request can select or alter this path.
func OpenServerMailbox(subjectID, trustedMailboxHostDir string, authority Authority, signer IssuerSigner, callbacks Callbacks, options ServerOptions) (*Server, error) {
	if subjectID == "" || authority == nil || signer == nil || callbacks.Authorize == nil || callbacks.Dispatch == nil ||
		!validBootID(authority.BootID()) || signer.IssuerKeyID() == "" {
		return nil, ErrInvalidRecord
	}
	mailbox, err := openAbsoluteDirNoFollow(trustedMailboxHostDir)
	if err != nil {
		return nil, fmt.Errorf("open server mailbox: %w", err)
	}
	if err := validatePrivateDir(mailbox); err != nil {
		_ = mailbox.Close()
		return nil, err
	}
	closeAll := func(files ...*os.File) {
		for _, file := range files {
			if file != nil {
				_ = file.Close()
			}
		}
	}
	requests, err := openDirAt(mailbox, RequestsDirName)
	if err != nil {
		closeAll(mailbox)
		return nil, fmt.Errorf("open request directory: %w", err)
	}
	if err := validatePrivateDir(requests); err != nil {
		closeAll(requests, mailbox)
		return nil, err
	}
	responses, err := openDirAt(mailbox, ResponsesDirName)
	if err != nil {
		closeAll(requests, mailbox)
		return nil, fmt.Errorf("open response directory: %w", err)
	}
	if err := validatePrivateDir(responses); err != nil {
		closeAll(responses, requests, mailbox)
		return nil, err
	}
	responseCleanup, err := openDirAt(mailbox, ResponsesDirName)
	if err != nil {
		closeAll(responses, requests, mailbox)
		return nil, fmt.Errorf("open response cleanup directory: %w", err)
	}
	responseUsage, err := openDirAt(mailbox, ResponsesDirName)
	if err != nil {
		closeAll(responseCleanup, responses, requests, mailbox)
		return nil, fmt.Errorf("open response usage directory: %w", err)
	}
	// processing is beneath the client-visible read-only mailbox mount. It is
	// server-writable through this host descriptor, but is not claimed hidden.
	processing, err := ensureDirAt(mailbox, processingDirName, 0o700)
	if err != nil {
		closeAll(responseUsage, responseCleanup, responses, requests, mailbox)
		return nil, fmt.Errorf("open processing directory: %w", err)
	}
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	random := options.Random
	if random == nil {
		random = rand.Reader
	}
	poll := options.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	settlementPoll := options.SettlementInterval
	if settlementPoll <= 0 || settlementPoll > DefaultPollInterval {
		settlementPoll = DefaultPollInterval
	}
	server := &Server{
		subjectID: subjectID, authority: authority, signer: signer, callbacks: callbacks,
		clock: clock, random: random, poll: poll, mailbox: mailbox, requests: requests,
		processing: processing, responses: responses, responseCleanup: responseCleanup, responseUsage: responseUsage,
		pending:        make(map[string]pendingReceipt),
		responseBudget: options.ResponseBudget, responseLeases: make(map[string]ResponseLease),
		settlementPoll: settlementPoll, settlementStop: make(chan struct{}), settlementDone: make(chan struct{}),
		responseWriter: atomicWriteAt, wake: newWakeSignal(),
	}
	notify := options.RequestNotify
	server.notify = notify
	server.slowRequest = options.SlowRequest
	if options.Slow != nil {
		server.slow = options.Slow
		server.slowJobs = make(chan slowJob, MaxSlowRequests)
		server.inflight = make(map[string]struct{})
		server.slowStop, server.slowDone = make(chan struct{}), make(chan struct{})
		go server.runSlow()
	}
	if watch, err := watchDir(requests, filepath.Join(trustedMailboxHostDir, RequestsDirName), func() {
		server.wake.notify()
		if notify != nil {
			notify()
		}
	}); err == nil {
		server.watch = watch
	}
	go server.runSettlement()
	return server, nil
}

// PublishService durably replaces service.json. Unlike unique request/response
// identities, daemon boot metadata must be republished after every restart.
func (s *Server) PublishService(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.isClosed() {
		return ErrClosed
	}
	service := Service{
		Protocol: ProtocolVersion, BootID: s.authority.BootID(), SubjectID: s.subjectID,
		IssuerKeyID: s.signer.IssuerKeyID(), PublishedAt: s.clock().UnixMilli(),
	}
	if err := signService(s.signer, &service); err != nil {
		return err
	}
	encoded, err := marshalBounded(service, maxServiceFileBytes)
	if err != nil {
		return err
	}
	return replaceFileAt(s.mailbox, ServiceFileName, encoded, s.random)
}

// Initialize performs the callback-free, bounded startup work required before
// this mailbox may dispatch requests. Callers that coordinate multiple subject
// servers must initialize every mailbox before serving any of them, so durable
// responses from a prior daemon boot are accounted ahead of fresh response
// admission. ErrResponseCapacity means bounded cleanup/adoption made progress
// but needs another call; no Authorize, Dispatch, or receipt hook runs here.
func (s *Server) Initialize(ctx context.Context) error {
	s.serveMu.Lock()
	defer s.serveMu.Unlock()
	return s.initializeLocked(ctx)
}

func (s *Server) initializeLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.isClosed() {
		return ErrClosed
	}
	if err := s.initializeResponseBudget(ctx); err != nil {
		return err
	}
	return s.cleanupResponses()
}

// ServeOnce performs bounded work for exactly one authoritative subject.
func (s *Server) ServeOnce(ctx context.Context) (int, error) {
	s.serveMu.Lock()
	defer s.serveMu.Unlock()
	if err := s.initializeLocked(ctx); err != nil {
		return 0, err
	}
	s.settleExpired()
	processed := 0
	overflow := false
	laneFull := false // see processOrDefer
	// blocked records exhausted response capacity. The pass continues so a
	// release queued behind a blocked request can still free that capacity.
	blocked := false

	processingNames, more, err := listNames(s.processing, MaxQueuedRequests)
	if err != nil {
		return 0, err
	}
	overflow = overflow || more
	sort.Strings(processingNames)
	for _, name := range processingNames {
		if processed >= MaxQueuedRequests {
			overflow = true
			break
		}
		if _, ok := requestIDFromFile(name); !ok {
			_ = unlinkAt(s.processing, name)
			processed++
			continue
		}
		if err := s.processOrDefer(ctx, name, !blocked, &laneFull); errors.Is(err, ErrResponseCapacity) {
			blocked = true
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return processed, err
		}
		processed++
	}

	if processed < MaxQueuedRequests {
		requestNames, more, err := listNames(s.requests, MaxQueuedRequests-processed)
		if err != nil {
			return processed, err
		}
		if s.rejectRequests || more {
			kept := false
			for _, name := range requestNames {
				removed, err := s.removeRequestEntry(name)
				if err != nil {
					return processed, err
				}
				kept = kept || !removed
				processed++
			}
			s.rejectRequests = more || kept
			if len(requestNames) != 0 || more {
				overflow = true
			}
			goto cleanup
		}
		sort.Strings(requestNames)
		for _, name := range requestNames {
			if processed >= MaxQueuedRequests {
				overflow = true
				break
			}
			if _, ok := requestIDFromFile(name); !ok {
				if _, err := s.removeRequestEntry(name); err != nil {
					return processed, err
				}
				processed++
				continue
			}
			claimed, err := s.claim(name)
			if err != nil {
				return processed, err
			}
			if claimed {
				if err := s.processOrDefer(ctx, name, !blocked, &laneFull); errors.Is(err, ErrResponseCapacity) {
					blocked = true
				} else if err != nil && !errors.Is(err, os.ErrNotExist) {
					return processed, err
				}
			}
			processed++
		}
	} else {
		remaining, _, err := listNames(s.requests, 1)
		if err != nil {
			return processed, err
		}
		if len(remaining) != 0 {
			s.rejectRequests = true
			overflow = true
		}
	}

cleanup:
	if err := s.cleanupResponses(); err != nil {
		return processed, err
	}
	if blocked {
		return processed, fmt.Errorf("%w: daemon response budget admission", ErrResponseCapacity)
	}
	if overflow {
		return processed, ErrQueueFull
	}
	return processed, nil
}

// removeRequestEntry removes hostile queue entries and abandoned publication
// temporaries, while preserving a recent safe temporary that a client may still
// be writing. Removing that inode would make the client's final rename fail
// with ENOENT after the request outcome has already become uncertain.
func (s *Server) removeRequestEntry(name string) (bool, error) {
	if !requestTemporaryFromFile(name) {
		return true, unlinkAt(s.requests, name)
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(s.requests.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return true, nil
		}
		return false, err
	}
	mode := uint32(stat.Mode)
	if mode&unix.S_IFMT == unix.S_IFREG && stat.Nlink == 1 && stat.Uid == uint32(os.Geteuid()) &&
		mode&0o7777 == regularFileMode && !statChangeTime(stat).Before(s.clock().Add(-requestTemporaryMaxAge)) {
		return false, nil
	}
	return true, unlinkAt(s.requests, name)
}

// Watched reports whether request publication wakes this server. When it
// does, a scheduler only needs a slow fallback pass (WatchFallbackInterval);
// otherwise it must keep polling.
func (s *Server) Watched() bool { return s.watch != nil }

func (s *Server) Run(ctx context.Context) error {
	if err := s.PublishService(ctx); err != nil {
		return err
	}
	interval := s.poll
	if s.Watched() {
		interval = max(interval, WatchFallbackInterval)
	}
	for {
		_, err := s.ServeOnce(ctx)
		if err != nil && !errors.Is(err, ErrQueueFull) && !errors.Is(err, ErrResponseCapacity) {
			return err
		}
		waitWake(ctx.Done(), s.wake, interval)
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

// processOrDefer dispatches a claimed request now, or hands it to the slow
// lane. A request the lane already owns is skipped; one the lane has no room
// for stays claimed for a later pass.
// admit is processClaimed's: a pass blocked on response capacity hands no new
// work to the lane either. laneFull is per pass: once the lane had no room, the
// pass hands it nothing more, so a slot freed mid-pass cannot go to a later
// request ahead of the one just left waiting.
func (s *Server) processOrDefer(ctx context.Context, name string, admit bool, laneFull *bool) error {
	if s.slow == nil {
		return s.processClaimed(ctx, name, admit)
	}
	s.mu.Lock()
	_, owned := s.inflight[name]
	s.mu.Unlock()
	if owned {
		return nil
	}
	if !s.claimedSlow(name) {
		return s.processClaimed(ctx, name, admit)
	}
	if !admit {
		return ErrResponseCapacity
	}
	if *laneFull {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.inflight) >= MaxSlowRequests {
		*laneFull = true
		return nil
	}
	s.inflight[name] = struct{}{}
	s.slowJobs <- slowJob{ctx: ctx, name: name} // capacity MaxSlowRequests: never blocks
	return nil
}

// claimedSlow peeks at a claimed request's unauthenticated call to pick its
// lane. Anything unreadable is processed inline, which rejects it.
func (s *Server) claimedSlow(name string) bool {
	raw, err := readRegularAt(s.processing, name, maxEnvelopeFileBytes)
	if err != nil {
		return false
	}
	var envelope access.SignedRequest
	if unmarshalBounded(raw, maxEnvelopeFileBytes, &envelope) != nil {
		return false
	}
	var call Call
	if unmarshalBounded(envelope.Body, access.MaxBodyBytes, &call) != nil || call.Kind != CallOperation {
		return false
	}
	return s.slow(cloneCall(call))
}

// runSlow dispatches the slow lane in arrival order. A request whose
// processing fails stays claimed, and a later pass retries it as before.
func (s *Server) runSlow() {
	defer close(s.slowDone)
	for {
		select {
		case <-s.slowStop:
			return // queued jobs stay claimed for the next server
		case job := <-s.slowJobs:
			err := s.processClaimed(job.ctx, job.name, true)
			s.mu.Lock()
			delete(s.inflight, job.name)
			s.mu.Unlock()
			if errors.Is(err, ErrResponseCapacity) {
				continue // retried by the pass that next finds capacity
			}
			// Requests left claimed while the lane was full can now be taken.
			s.wake.notify()
			if s.notify != nil {
				s.notify()
			}
		}
	}
}

// Busy reports whether the slow lane holds a request. Close waits for it, so a
// caller holding a lock that dispatch may need must hand the close off.
func (s *Server) Busy() bool {
	if s.slow == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inflight) != 0
}

func (s *Server) claim(name string) (bool, error) {
	err := renameNoReplace(int(s.requests.Fd()), name, int(s.processing.Fd()), name)
	if err == nil {
		if err := unix.Fsync(int(s.requests.Fd())); err != nil {
			return false, err
		}
		if err := unix.Fsync(int(s.processing.Fd())); err != nil {
			return false, err
		}
		return true, nil
	}
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	if errors.Is(err, unix.EEXIST) {
		// A prior claim owns this identity. The duplicate is not dispatched.
		return false, unlinkAt(s.requests, name)
	}
	return false, err
}

// processClaimed handles one claimed request. admit=false means response
// capacity is already exhausted in this pass: only a release, which needs no
// response, is processed and everything else stays claimed for a later pass.
func (s *Server) processClaimed(ctx context.Context, name string, admit bool) error {
	requestID, ok := requestIDFromFile(name)
	if !ok {
		return unlinkAt(s.processing, name)
	}
	raw, err := readRegularAt(s.processing, name, maxEnvelopeFileBytes)
	if err != nil {
		_ = unlinkAt(s.processing, name)
		return nil // hostile/nonregular input is rejected without dispatch
	}
	// raw is one bounded immutable copy. JSON decoding creates the exact Body
	// copy used by both Authority.Verify and Call decoding; the claimed inode is
	// never consulted again, even if a client retained a writable descriptor.
	var envelope access.SignedRequest
	if err := unmarshalBounded(raw, maxEnvelopeFileBytes, &envelope); err != nil || envelope.RequestID != requestID || !validEnvelopeShape(envelope) {
		_ = unlinkAt(s.processing, name)
		return nil
	}
	// The unverified kind only routes: a release must not wait for the response
	// capacity it exists to free. processRelease verifies everything itself.
	var routed Call
	if unmarshalBounded(envelope.Body, access.MaxBodyBytes, &routed) == nil && routed.Kind == CallRelease {
		return s.processRelease(ctx, name, envelope)
	}
	timer := s.startTimer(envelope, routed)
	if s.slowRequest != nil {
		ctx, timer.lockWait = access.WithLockWait(ctx)
		defer s.reportTimer(timer)
	}
	mark := timer.start
	// Reserve the worst-case per-subject response capacity before durable replay
	// acceptance or dispatch. No operation is admitted if the daemon cannot keep
	// its bounded result artifact.
	var reservation *responseReservation
	switch {
	case routed.Kind == CallReceipt:
		// A receipt is one-way: the client never reads a result for it, and it
		// settles a mandatory completion that frees capacity, so it must neither
		// wait behind the pool it drains nor retain an unread response.
		reservation = &responseReservation{discard: true}
	case !admit:
		return ErrResponseCapacity
	default:
		reservation, err = s.reserveResponse(ctx, requestID)
		if err != nil {
			return err
		}
	}
	reservation.timer = timer
	timer.phase(&timer.Reserve, &mark)
	defer reservation.release()
	requestDigestBytes := sha256.Sum256(envelope.Body)
	requestDigest := hex.EncodeToString(requestDigestBytes[:])

	principal, verifyErr := s.authority.Verify(ctx, envelope)
	timer.phase(&timer.Verify, &mark)
	if verifyErr != nil {
		status, code := verifyStatus(verifyErr)
		response := s.newResponse(envelope, requestDigest, status, code, nil, false)
		if err := s.publishResponse(&response, reservation); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}
	if principal.Kind != access.SubjectSession || principal.SubjectID != s.subjectID {
		response := s.newResponse(envelope, requestDigest, StatusDenied, "subject_mismatch", nil, false)
		if err := s.publishResponse(&response, reservation); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}

	var call Call
	if err := unmarshalBounded(envelope.Body, access.MaxBodyBytes, &call); err != nil || validateCall(call) != nil {
		response := s.newResponse(envelope, requestDigest, StatusInvalid, "invalid_call", nil, false)
		if err := s.publishResponse(&response, reservation); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}
	err = s.authority.Valid(ctx, principal)
	timer.phase(&timer.Authorize, &mark)
	if err != nil {
		response := s.newResponse(envelope, requestDigest, StatusDenied, "credential_invalid", nil, false)
		if err := s.publishResponse(&response, reservation); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}
	err = s.callbacks.Authorize(ctx, principal, cloneCall(call))
	timer.phase(&timer.Authorize, &mark)
	if err != nil {
		response := s.newResponse(envelope, requestDigest, StatusDenied, "access_denied", nil, false)
		if err := s.publishResponse(&response, reservation); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}
	// Authorize may block while rotation, revocation, or archival commits. Check
	// credential generation again before receipt acceptance or dispatch. The
	// dispatcher still owns the atomic current-generation/policy execution guard.
	err = s.authority.Valid(ctx, principal)
	timer.phase(&timer.Authorize, &mark)
	if err != nil {
		response := s.newResponse(envelope, requestDigest, StatusDenied, "credential_invalid", nil, false)
		if err := s.publishResponse(&response, reservation); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}

	if call.Kind == CallReceipt {
		status, code := s.acceptReceipt(principal, *call.Receipt)
		if status == StatusOK {
			// The receipt proved the client holds this exact response.
			s.retireReceipted(call.Receipt.RequestID)
		}
		response := s.newResponse(envelope, requestDigest, status, code, nil, false)
		if err := s.publishResponse(&response, reservation); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}
	// Reserve pending-receipt admission before entering a dispatcher that may
	// commit a terminal operation. This intentionally blocks even a nonterminal
	// operation while the subject has exhausted its receipt budget: the
	// transport cannot know whether Dispatch will require a receipt in advance.
	if !s.receiptCapacity() {
		response := s.newResponse(envelope, requestDigest, StatusIndeterminate, "receipt_capacity", nil, false)
		if err := s.publishResponse(&response, reservation); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}

	result, dispatchErr := s.callbacks.Dispatch(ctx, DispatchRequest{Principal: principal, RequestID: requestID, Call: cloneCall(call)})
	timer.phase(&timer.Dispatch, &mark)
	hooks := result.Receipt
	if dispatchErr != nil {
		if hooks != nil {
			s.settleHooks(requestID, *hooks, SettlementResponseFailed)
			hooks = nil
		}
		result = DispatchResult{Status: StatusFailed, Code: "dispatch_failed"}
	}
	if result.Status == "" {
		result.Status = StatusOK
	}
	if !validStatus(result.Status) || !validCode(result.Code) || len(result.Body) > MaxResponseBody ||
		(hooks != nil && (result.Status != StatusOK || hooks.Settled == nil)) {
		if hooks != nil {
			s.settleHooks(requestID, *hooks, SettlementResponseFailed)
			hooks = nil
		}
		result = DispatchResult{Status: StatusFailed, Code: "invalid_dispatch_result"}
	}
	receiptRequired := hooks != nil
	response := s.newResponse(envelope, requestDigest, result.Status, result.Code, result.Body, receiptRequired)
	if err := s.publishResponse(&response, reservation); err != nil {
		if hooks != nil {
			s.settleHooks(requestID, *hooks, SettlementResponseFailed)
		}
		if !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}
	if receiptRequired {
		s.registerReceipt(principal, response, *hooks)
	}
	return unlinkAt(s.processing, name)
}

// processRelease retires one consumed response. It reserves nothing and
// publishes nothing; any failure simply leaves the response to age cleanup.
// The request is still verified (and durably consumed in the replay ledger)
// like every other call before anything is deleted.
func (s *Server) processRelease(ctx context.Context, name string, envelope access.SignedRequest) error {
	defer func() { _ = unlinkAt(s.processing, name) }()
	principal, err := s.authority.Verify(ctx, envelope)
	if err != nil || principal.Kind != access.SubjectSession || principal.SubjectID != s.subjectID {
		return nil
	}
	var call Call
	if err := unmarshalBounded(envelope.Body, access.MaxBodyBytes, &call); err != nil || call.Kind != CallRelease || validateCall(call) != nil {
		return nil
	}
	if s.authority.Valid(ctx, principal) != nil {
		return nil
	}
	s.retireResponse(principal, *call.Receipt)
	return nil
}

// retireResponse deletes a committed response only when the caller proves it
// holds that exact signed response: same subject, credential generation and
// boot, and the digest of the bytes the daemon signed. A response that still
// awaits a mandatory receipt is never retired here.
func (s *Server) retireResponse(principal access.Principal, release Receipt) {
	s.mu.Lock()
	_, pending := s.pending[release.RequestID]
	s.mu.Unlock()
	if pending {
		return
	}
	name := responseFileName(release.RequestID)
	data, err := readRegularAt(s.responses, name, maxEnvelopeFileBytes)
	if err != nil {
		return
	}
	var response Response
	if unmarshalBounded(data, maxEnvelopeFileBytes, &response) != nil {
		return
	}
	if response.RequestID != release.RequestID || response.SubjectID != s.subjectID ||
		response.KeyID != principal.KeyID || response.Generation != principal.Generation ||
		response.BootID != s.authority.BootID() || response.ReceiptRequired ||
		subtle.ConstantTimeCompare([]byte(responseDigest(response)), []byte(release.ResponseDigest)) != 1 {
		return
	}
	if unlinkAt(s.responses, name) == nil {
		s.releaseResponseLease(release.RequestID)
	}
}

// retireReceipted deletes a response whose mandatory receipt was accepted.
func (s *Server) retireReceipted(requestID string) {
	if unlinkAt(s.responses, responseFileName(requestID)) == nil {
		s.releaseResponseLease(requestID)
	}
}

func (s *Server) newResponse(request access.SignedRequest, requestDigest string, status Status, code string, body []byte, receiptRequired bool) Response {
	return Response{
		Protocol: ProtocolVersion, BootID: s.authority.BootID(), IssuerKeyID: s.signer.IssuerKeyID(),
		SubjectID: s.subjectID, KeyID: request.KeyID, Generation: request.Generation,
		RequestID: request.RequestID, RequestBodyDigest: requestDigest, Status: status, Code: code,
		Body: append([]byte(nil), body...), CompletedAt: s.clock().UnixMilli(), ReceiptRequired: receiptRequired,
	}
}

func (s *Server) publishResponse(response *Response, reservation *responseReservation) error {
	if response == nil || reservation == nil || reservation.committed {
		return ErrInvalidRecord
	}
	if reservation.discard {
		reservation.committed = true
		return nil
	}
	timer := reservation.timer
	if timer == nil {
		timer = &requestTimer{}
	}
	timer.Status, timer.Code = response.Status, response.Code
	mark := time.Now()
	defer func() { timer.phase(&timer.Publish, &mark) }()
	if err := signResponse(s.signer, response); err != nil {
		return err
	}
	encoded, err := marshalBounded(response, maxEnvelopeFileBytes)
	if err != nil {
		return err
	}
	timer.phase(&timer.Sign, &mark)
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if err := s.ensureResponseCapacity(len(encoded)); err != nil {
		return err
	}
	if err := s.responseWriter(s.responses, responseFileName(response.RequestID), encoded, s.random); err != nil {
		// Directory fsync can fail after the no-replace rename made the response
		// visible. It is not a persisted success, but any visible daemon-created
		// bytes must retain their aggregate-budget lease until cleanup.
		if !errors.Is(err, unix.EEXIST) {
			s.retainResponseLeaseIfPresent(response.RequestID, int64(len(encoded)), reservation)
		}
		return err
	}
	if reservation.lease != nil {
		reservation.lease.Commit(int64(len(encoded)), response.ReceiptRequired)
		s.budgetMu.Lock()
		s.responseLeases[response.RequestID] = reservation.lease
		s.budgetMu.Unlock()
		reservation.lease = nil
	}
	reservation.committed = true
	return nil
}

func (s *Server) retainResponseLeaseIfPresent(requestID string, size int64, reservation *responseReservation) {
	if reservation == nil || reservation.lease == nil || reservation.committed {
		return
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(s.responses.Fd()), responseFileName(requestID), &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return
	}
	mode := uint32(stat.Mode)
	if mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) ||
		mode&0o7777 != regularFileMode || stat.Size != size {
		return
	}
	reservation.lease.Commit(size, false)
	s.budgetMu.Lock()
	s.responseLeases[requestID] = reservation.lease
	s.budgetMu.Unlock()
	reservation.lease = nil
	reservation.committed = true
}

func (s *Server) receiptCapacity() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.closed && len(s.pending) < MaxPendingReceipts
}

func (s *Server) registerReceipt(principal access.Principal, response Response, hooks ReceiptHooks) {
	grace := hooks.Grace
	if grace <= 0 {
		grace = DefaultReceiptGrace
	}
	if grace > MaxReceiptGrace {
		grace = MaxReceiptGrace
	}
	persisted := PersistedResponse{
		RequestID: response.RequestID, ResponseDigest: responseDigest(response),
		CompletedAt: time.UnixMilli(response.CompletedAt),
	}
	s.mu.Lock()
	s.pending[response.RequestID] = pendingReceipt{
		principal: principal, digest: persisted.ResponseDigest, deadline: s.clock().Add(grace), hooks: hooks,
	}
	s.mu.Unlock()
	if hooks.ResponsePersisted != nil {
		hooks.ResponsePersisted(persisted)
	}
	now := s.clock()
	var expired *pendingReceipt
	s.mu.Lock()
	if pending, ok := s.pending[response.RequestID]; ok {
		pending.ready = true
		if pending.expired || !now.Before(pending.deadline) {
			delete(s.pending, response.RequestID)
			expired = &pending
		} else {
			s.pending[response.RequestID] = pending
		}
	}
	s.mu.Unlock()
	if expired != nil {
		s.settle(response.RequestID, *expired, false, SettlementGraceExpired, now)
	}
}

func (s *Server) acceptReceipt(principal access.Principal, receipt Receipt) (Status, string) {
	now := s.clock()
	s.mu.Lock()
	pending, ok := s.pending[receipt.RequestID]
	if !ok || pending.digest != receipt.ResponseDigest || pending.principal.SubjectID != principal.SubjectID ||
		pending.principal.KeyID != principal.KeyID || pending.principal.Generation != principal.Generation {
		s.mu.Unlock()
		return StatusInvalid, "invalid_receipt"
	}
	if !now.Before(pending.deadline) {
		delete(s.pending, receipt.RequestID)
		s.mu.Unlock()
		s.settle(receipt.RequestID, pending, false, SettlementGraceExpired, now)
		return StatusInvalid, "receipt_expired"
	}
	delete(s.pending, receipt.RequestID)
	s.mu.Unlock()
	s.settle(receipt.RequestID, pending, true, SettlementReceipt, now)
	return StatusOK, ""
}

func (s *Server) settleExpired() {
	now := s.clock()
	var expired []pendingReceipt
	var ids []string
	s.mu.Lock()
	for id, pending := range s.pending {
		if !now.Before(pending.deadline) {
			if !pending.ready {
				pending.expired = true
				s.pending[id] = pending
				continue
			}
			ids = append(ids, id)
			expired = append(expired, pending)
			delete(s.pending, id)
		}
	}
	s.mu.Unlock()
	for index, pending := range expired {
		s.settle(ids[index], pending, false, SettlementGraceExpired, now)
	}
}

func (s *Server) settle(requestID string, pending pendingReceipt, received bool, reason SettlementReason, at time.Time) {
	s.releaseReceiptLease(requestID)
	if pending.hooks.Settled != nil {
		pending.hooks.Settled(ReceiptSettlement{RequestID: requestID, Received: received, Reason: reason, At: at})
	}
}

func (s *Server) settleHooks(requestID string, hooks ReceiptHooks, reason SettlementReason) {
	s.releaseReceiptLease(requestID)
	if hooks.Settled != nil {
		hooks.Settled(ReceiptSettlement{RequestID: requestID, Reason: reason, At: s.clock()})
	}
}

func (s *Server) runSettlement() {
	ticker := time.NewTicker(s.settlementPoll)
	defer func() {
		ticker.Stop()
		close(s.settlementDone)
	}()
	for {
		select {
		case <-ticker.C:
			s.settleExpired()
		case <-s.settlementStop:
			return
		}
	}
}

func (s *Server) cleanupResponses() error {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	names, _, err := nextNames(s.responseCleanup, MaxQueuedRequests)
	if err != nil {
		return err
	}
	cutoff := s.clock().Add(-time.Duration(access.MaxResponseAge) * time.Second)
	for _, name := range names {
		var stat unix.Stat_t
		if err := unix.Fstatat(int(s.responses.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return err
		}
		mode := uint32(stat.Mode)
		_, validName := requestIDFromResponseFile(name)
		safe := validName && mode&unix.S_IFMT == unix.S_IFREG && stat.Nlink == 1 &&
			stat.Uid == uint32(os.Geteuid()) && mode&0o7777 == regularFileMode &&
			stat.Size >= 0 && stat.Size <= int64(maxEnvelopeFileBytes)
		if !safe || statModTime(stat).Before(cutoff) {
			if err := unlinkAt(s.responses, name); err != nil {
				return err
			}
			if requestID, ok := requestIDFromResponseFile(name); ok {
				s.releaseResponseLease(requestID)
			}
		}
	}
	return nil
}

func (s *Server) initializeResponseBudget(ctx context.Context) error {
	if s.responseBudgetInitialized {
		return nil
	}
	if s.responseBudget == nil {
		s.responseBudgetInitialized = true
		return nil
	}
	// A response publication crash can leave a temporary behind. Scrub only
	// entries that cannot be committed response identities before adoption; a
	// valid durable response is never removed until its budget lease is held.
	if !s.responseScrubComplete {
		complete, err := s.scrubResponseArtifacts()
		if err != nil {
			return err
		}
		if !complete {
			return ErrResponseCapacity
		}
		s.responseScrubComplete = true
	}

	// Adoption advances in bounded chunks and gates all request dispatch until a
	// complete directory pass has accounted every surviving response. Leases are
	// retained across retryable budget failures, while a restarted pass skips
	// identities already adopted by this server.
	names, complete, err := nextNames(s.responseUsage, MaxResponseFiles)
	if err != nil {
		return err
	}
	if s.responseAdoptionSeen == nil {
		s.responseAdoptionSeen = make(map[string]struct{})
	}
	for _, name := range names {
		requestID, size, safe, err := s.responseArtifact(name)
		if err != nil {
			return err
		}
		if !safe {
			// No package writer can publish a committed response with this shape.
			// Remove a late unsafe artifact and restart the pass so it cannot be
			// mistaken for a completed adoption cycle.
			if err := unlinkAt(s.responses, name); err != nil {
				return err
			}
			s.releaseResponseLease(requestID)
			if err := rewindDirectory(s.responseUsage); err != nil {
				return err
			}
			s.responseAdoptionSeen = make(map[string]struct{})
			return ErrResponseCapacity
		}
		s.responseAdoptionSeen[requestID] = struct{}{}
		s.budgetMu.Lock()
		existing := s.responseLeases[requestID]
		s.budgetMu.Unlock()
		if existing != nil {
			continue
		}
		lease, err := s.responseBudget.ReserveResponse(ctx, ResponseReservation{
			SubjectID: s.subjectID, RequestID: requestID, MaxBytes: size, Existing: true,
		})
		if err != nil || lease == nil {
			if rewindErr := rewindDirectory(s.responseUsage); rewindErr != nil {
				return rewindErr
			}
			s.responseAdoptionSeen = make(map[string]struct{})
			return fmt.Errorf("%w: daemon response budget admission", ErrResponseCapacity)
		}
		lease.Commit(size, false)
		s.budgetMu.Lock()
		s.responseLeases[requestID] = lease
		s.budgetMu.Unlock()
	}
	if !complete {
		return ErrResponseCapacity
	}
	// Reconcile identities that vanished during a restarted adoption pass. The
	// responses mount is client-read-only, but this keeps accounting correct if
	// daemon-owned cleanup or replacement raced a recoverable budget failure.
	s.budgetMu.Lock()
	var vanished []ResponseLease
	for requestID, lease := range s.responseLeases {
		if _, ok := s.responseAdoptionSeen[requestID]; !ok {
			delete(s.responseLeases, requestID)
			vanished = append(vanished, lease)
		}
	}
	s.budgetMu.Unlock()
	for _, lease := range vanished {
		lease.Release()
	}
	s.responseAdoptionSeen = nil
	s.responseBudgetInitialized = true
	return nil
}

func (s *Server) scrubResponseArtifacts() (bool, error) {
	names, complete, err := nextNames(s.responseUsage, MaxQueuedRequests)
	if err != nil {
		return false, err
	}
	for _, name := range names {
		_, _, safe, err := s.responseArtifact(name)
		if err != nil {
			return false, err
		}
		if safe {
			continue
		}
		if err := unlinkAt(s.responses, name); err != nil {
			return false, err
		}
	}
	return complete, nil
}

// responseArtifact identifies the only shape atomicWriteAt can commit. A
// missing entry is unsafe but harmless: unlinkAt treats the race as success.
func (s *Server) responseArtifact(name string) (requestID string, size int64, safe bool, err error) {
	requestID, validName := requestIDFromResponseFile(name)
	var stat unix.Stat_t
	if err := unix.Fstatat(int(s.responses.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return requestID, 0, false, nil
		}
		return requestID, 0, false, err
	}
	mode := uint32(stat.Mode)
	safe = validName && mode&unix.S_IFMT == unix.S_IFREG && stat.Nlink == 1 &&
		stat.Uid == uint32(os.Geteuid()) && mode&0o7777 == regularFileMode &&
		stat.Size > 0 && stat.Size <= int64(maxEnvelopeFileBytes)
	return requestID, stat.Size, safe, nil
}

func rewindDirectory(dir *os.File) error {
	_, err := dir.Seek(0, io.SeekStart)
	return err
}

func (s *Server) reserveResponse(ctx context.Context, requestID string) (*responseReservation, error) {
	if err := s.ensureResponseCapacity(maxEnvelopeFileBytes); err != nil {
		return nil, err
	}
	reservation := &responseReservation{}
	if s.responseBudget == nil {
		return reservation, nil
	}
	lease, err := s.responseBudget.ReserveResponse(ctx, ResponseReservation{
		SubjectID: s.subjectID, RequestID: requestID, MaxBytes: int64(maxEnvelopeFileBytes), ReceiptSlot: true,
	})
	if err != nil || lease == nil {
		return nil, fmt.Errorf("%w: daemon response budget admission", ErrResponseCapacity)
	}
	reservation.lease = lease
	return reservation, nil
}

func (s *Server) releaseResponseLease(requestID string) {
	s.budgetMu.Lock()
	lease := s.responseLeases[requestID]
	if lease != nil {
		delete(s.responseLeases, requestID)
	}
	s.budgetMu.Unlock()
	if lease != nil {
		lease.Release()
	}
}

func (s *Server) releaseReceiptLease(requestID string) {
	s.budgetMu.Lock()
	lease := s.responseLeases[requestID]
	s.budgetMu.Unlock()
	if lease != nil {
		lease.ReleaseReceipt()
	}
}

func (s *Server) ensureResponseCapacity(additional int) error {
	if additional <= 0 || additional > maxEnvelopeFileBytes {
		return ErrInvalidRecord
	}
	names, more, err := listNames(s.responseUsage, MaxResponseFiles)
	if err != nil {
		return err
	}
	if more {
		return ErrResponseCapacity
	}
	count := 0
	var bytes int64
	for _, name := range names {
		var stat unix.Stat_t
		if err := unix.Fstatat(int(s.responses.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return err
		}
		mode := uint32(stat.Mode)
		if mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) ||
			mode&0o7777 != regularFileMode || stat.Size < 0 || stat.Size > int64(maxEnvelopeFileBytes) {
			return ErrResponseCapacity
		}
		count++
		bytes += stat.Size
	}
	if count >= MaxResponseFiles || bytes+int64(additional) > MaxResponseBytes {
		return ErrResponseCapacity
	}
	return nil
}

func requestIDFromResponseFile(name string) (string, bool) {
	if len(name) != 32+4 || name[len(name)-4:] != ".res" {
		return "", false
	}
	id := name[:32]
	return id, validRequestID(id)
}

func verifyStatus(err error) (Status, string) {
	switch {
	case errors.Is(err, access.ErrReplay):
		return StatusReplay, "replay_rejected"
	case errors.Is(err, access.ErrExpired):
		return StatusInvalid, "expired"
	case errors.Is(err, access.ErrRevoked):
		return StatusDenied, "credential_revoked"
	case errors.Is(err, access.ErrCapacity):
		return StatusFailed, "authority_capacity"
	default:
		return StatusInvalid, "authentication_failed"
	}
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *Server) Close() error {
	s.serveMu.Lock()
	defer s.serveMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	s.watch.Close()
	// Join the slow lane before settling receipts: a slow dispatch may still
	// register one.
	if s.slowStop != nil {
		close(s.slowStop)
		<-s.slowDone
	}
	s.mu.Lock()
	s.settlementStopOnce.Do(func() { close(s.settlementStop) })
	pending := s.pending
	s.pending = make(map[string]pendingReceipt)
	s.mu.Unlock()
	<-s.settlementDone
	now := s.clock()
	for requestID, value := range pending {
		s.settle(requestID, value, false, SettlementServerClosed, now)
	}
	var joined error
	// Durable response leases remain owned by the shared budget across close.
	// A replacement server idempotently adopts the same subject/request IDs;
	// only anchored file removal releases their count and byte reservations.
	s.budgetMu.Lock()
	s.responseLeases = make(map[string]ResponseLease)
	s.budgetMu.Unlock()
	for _, file := range []*os.File{s.responseUsage, s.responseCleanup, s.processing, s.responses, s.requests, s.mailbox} {
		if file != nil {
			joined = errors.Join(joined, file.Close())
		}
	}
	return joined
}
