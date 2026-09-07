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
	Clock        func() time.Time
	Random       io.Reader
	PollInterval time.Duration
}

type pendingReceipt struct {
	principal access.Principal
	digest    string
	deadline  time.Time
	hooks     ReceiptHooks
}

type Server struct {
	subjectID string
	authority Authority
	signer    IssuerSigner
	callbacks Callbacks
	clock     func() time.Time
	random    io.Reader
	poll      time.Duration

	mailbox    *os.File
	requests   *os.File
	processing *os.File
	responses  *os.File

	serveMu sync.Mutex
	// rejectRequests drains an over-capacity client-writable queue without ever
	// dispatching a request from that batch. It clears only after observing empty.
	rejectRequests bool
	mu             sync.Mutex
	pending        map[string]pendingReceipt
	closed         bool
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
	// processing is beneath the client-visible read-only mailbox mount. It is
	// server-writable through this host descriptor, but is not claimed hidden.
	processing, err := ensureDirAt(mailbox, processingDirName, 0o700)
	if err != nil {
		closeAll(responses, requests, mailbox)
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
	return &Server{
		subjectID: subjectID, authority: authority, signer: signer, callbacks: callbacks,
		clock: clock, random: random, poll: poll, mailbox: mailbox, requests: requests,
		processing: processing, responses: responses, pending: make(map[string]pendingReceipt),
	}, nil
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
			s.rejectRequests = more
			for _, name := range requestNames {
				_ = unlinkAt(s.requests, name)
				processed++
			}
			if len(requestNames) != 0 || more {
				overflow = true
			}
			if more {
				s.rejectRequests = true
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
				_ = unlinkAt(s.requests, name)
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

func (s *Server) Run(ctx context.Context) error {
	if err := s.PublishService(ctx); err != nil {
		return err
	}
	ticker := time.NewTicker(s.poll)
	defer ticker.Stop()
	for {
		_, err := s.ServeOnce(ctx)
		if err != nil && !errors.Is(err, ErrQueueFull) {
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
		if err := s.publishResponse(&response); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}
	if principal.Kind != access.SubjectSession || principal.SubjectID != s.subjectID {
		response := s.newResponse(envelope, requestDigest, StatusDenied, "subject_mismatch", nil, false)
		if err := s.publishResponse(&response); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}

	var call Call
	if err := unmarshalBounded(envelope.Body, access.MaxBodyBytes, &call); err != nil || validateCall(call) != nil {
		response := s.newResponse(envelope, requestDigest, StatusInvalid, "invalid_call", nil, false)
		if err := s.publishResponse(&response); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}
	if err := s.authority.Valid(ctx, principal); err != nil {
		response := s.newResponse(envelope, requestDigest, StatusDenied, "credential_invalid", nil, false)
		if err := s.publishResponse(&response); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}
	if err := s.callbacks.Authorize(ctx, principal, cloneCall(call)); err != nil {
		response := s.newResponse(envelope, requestDigest, StatusDenied, "access_denied", nil, false)
		if err := s.publishResponse(&response); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}

	if call.Kind == CallReceipt {
		status, code := s.acceptReceipt(principal, *call.Receipt)
		response := s.newResponse(envelope, requestDigest, status, code, nil, false)
		if err := s.publishResponse(&response); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}

	result, dispatchErr := s.callbacks.Dispatch(ctx, DispatchRequest{Principal: principal, RequestID: requestID, Call: cloneCall(call)})
	if dispatchErr != nil {
		result = DispatchResult{Status: StatusFailed, Code: "dispatch_failed"}
	}
	if result.Status == "" {
		result.Status = StatusOK
	}
	if !validStatus(result.Status) || !validCode(result.Code) || len(result.Body) > MaxResponseBody {
		result = DispatchResult{Status: StatusFailed, Code: "invalid_dispatch_result"}
	}
	receiptRequired := result.Receipt != nil && result.Status == StatusOK
	response := s.newResponse(envelope, requestDigest, result.Status, result.Code, result.Body, receiptRequired)
	if err := s.publishResponse(&response); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return err
		}
		return unlinkAt(s.processing, name)
	}
	if receiptRequired {
		s.registerReceipt(principal, response, *result.Receipt)
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

func (s *Server) publishResponse(response *Response) error {
	if response == nil {
		return ErrInvalidRecord
	}
	if err := signResponse(s.signer, response); err != nil {
		return err
	}
	encoded, err := marshalBounded(response, maxEnvelopeFileBytes)
	if err != nil {
		return err
	}
	return atomicWriteAt(s.responses, responseFileName(response.RequestID), encoded, s.random)
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
	delete(s.pending, receipt.RequestID)
	s.mu.Unlock()
	if pending.hooks.Settled != nil {
		pending.hooks.Settled(ReceiptSettlement{RequestID: receipt.RequestID, Received: true, At: now})
	}
	return StatusOK, ""
}

func (s *Server) settleExpired() {
	now := s.clock()
	var expired []pendingReceipt
	var ids []string
	s.mu.Lock()
	for id, pending := range s.pending {
		if !now.Before(pending.deadline) {
			ids = append(ids, id)
			expired = append(expired, pending)
			delete(s.pending, id)
		}
	}
	s.mu.Unlock()
	for index, pending := range expired {
		if pending.hooks.Settled != nil {
			pending.hooks.Settled(ReceiptSettlement{RequestID: ids[index], Received: false, At: now})
		}
	}
}

func (s *Server) cleanupResponses() error {
	names, _, err := listNames(s.responses, MaxQueuedRequests)
	if err != nil {
		return err
	}
	cutoff := s.clock().Add(-time.Duration(access.MaxResponseAge) * time.Second)
	for _, name := range names {
		if _, ok := requestIDFromResponseFile(name); !ok {
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(int(s.responses.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return err
		}
		if uint32(stat.Mode)&unix.S_IFMT == unix.S_IFREG && stat.Nlink == 1 && statModTime(stat).Before(cutoff) {
			if err := unlinkAt(s.responses, name); err != nil {
				return err
			}
		}
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
	pending := s.pending
	s.pending = make(map[string]pendingReceipt)
	s.mu.Unlock()
	now := s.clock()
	for requestID, value := range pending {
		if value.hooks.Settled != nil {
			value.hooks.Settled(ReceiptSettlement{RequestID: requestID, Received: false, At: now})
		}
	}
	var joined error
	for _, file := range []*os.File{s.processing, s.responses, s.requests, s.mailbox} {
		if file != nil {
			joined = errors.Join(joined, file.Close())
		}
	}
	return joined
}
