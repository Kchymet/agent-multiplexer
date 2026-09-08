package sessionrpc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
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
	lease     ResponseLease
	committed bool
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
		responseWriter: atomicWriteAt,
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

// ServeOnce performs bounded work for exactly one authoritative subject.
func (s *Server) ServeOnce(ctx context.Context) (int, error) {
	s.serveMu.Lock()
	defer s.serveMu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.isClosed() {
		return 0, ErrClosed
	}
	s.settleExpired()
	if err := s.initializeResponseBudget(ctx); err != nil {
		return 0, err
	}
	if err := s.cleanupResponses(); err != nil {
		return 0, err
	}
	processed := 0
	overflow := false

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
		if err := s.processClaimed(ctx, name); err != nil && !errors.Is(err, os.ErrNotExist) {
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
				if err := s.processClaimed(ctx, name); err != nil && !errors.Is(err, os.ErrNotExist) {
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

func (s *Server) Run(ctx context.Context) error {
	if err := s.PublishService(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		_, err := s.ServeOnce(ctx)
		if err != nil && !errors.Is(err, ErrQueueFull) && !errors.Is(err, ErrResponseCapacity) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
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

func (s *Server) processClaimed(ctx context.Context, name string) error {
	requestID, ok := requestIDFromFile(name)
	if !ok {
		return unlinkAt(s.processing, name)
	}
	// Reserve the worst-case per-subject response capacity before durable replay
	// acceptance or dispatch. No operation is admitted if the daemon cannot keep
	// its bounded result artifact.
	reservation, err := s.reserveResponse(ctx, requestID)
	if err != nil {
		return err
	}
	defer reservation.release()
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
	requestDigestBytes := sha256.Sum256(envelope.Body)
	requestDigest := hex.EncodeToString(requestDigestBytes[:])

	principal, verifyErr := s.authority.Verify(ctx, envelope)
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
	if err := s.authority.Valid(ctx, principal); err != nil {
		response := s.newResponse(envelope, requestDigest, StatusDenied, "credential_invalid", nil, false)
		if err := s.publishResponse(&response, reservation); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}
	if err := s.callbacks.Authorize(ctx, principal, cloneCall(call)); err != nil {
		response := s.newResponse(envelope, requestDigest, StatusDenied, "access_denied", nil, false)
		if err := s.publishResponse(&response, reservation); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}
	// Authorize may block while rotation, revocation, or archival commits. Check
	// credential generation again before receipt acceptance or dispatch. The
	// dispatcher still owns the atomic current-generation/policy execution guard.
	if err := s.authority.Valid(ctx, principal); err != nil {
		response := s.newResponse(envelope, requestDigest, StatusDenied, "credential_invalid", nil, false)
		if err := s.publishResponse(&response, reservation); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}

	if call.Kind == CallReceipt {
		status, code := s.acceptReceipt(principal, *call.Receipt)
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
	if err := signResponse(s.signer, response); err != nil {
		return err
	}
	encoded, err := marshalBounded(response, maxEnvelopeFileBytes)
	if err != nil {
		return err
	}
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
