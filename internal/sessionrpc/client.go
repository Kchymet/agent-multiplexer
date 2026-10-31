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
	"path/filepath"
	"sync"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"golang.org/x/sys/unix"
)

type Client struct {
	contextDir           *os.File
	mailbox              *os.File
	requests             *os.File
	responses            *os.File
	context              SessionContext
	clock                func() time.Time
	random               io.Reader
	poll                 time.Duration
	beforeRequestPublish func()
	afterServiceStat     func()
	beforeResponseRead   func()

	mu     sync.RWMutex
	closed bool
}

type clientOptions struct {
	clock                func() time.Time
	random               io.Reader
	poll                 time.Duration
	beforeRequestPublish func()
	afterServiceStat     func()
	beforeResponseRead   func()
}

// OpenClient discovers only the fixed read-only session context. There is no
// public path override: environment, cwd, and caller IDs cannot select authority.
func OpenClient() (*Client, error) {
	return openClientAt(core.SessionAccessDir(), clientOptions{})
}

func openClientAt(fixedContextDir string, options clientOptions) (*Client, error) {
	contextDir, err := openAbsoluteDirNoFollow(fixedContextDir)
	if err != nil {
		return nil, fmt.Errorf("open fixed session access: %w", err)
	}
	if err := validatePrivateDir(contextDir); err != nil {
		_ = contextDir.Close()
		return nil, err
	}
	contextBytes, err := readRegularAtMode(contextDir, ContextFileName, maxContextFileBytes, 0o400)
	if err != nil {
		_ = contextDir.Close()
		return nil, fmt.Errorf("read fixed session context: %w", err)
	}
	var sessionContext SessionContext
	if err := unmarshalBounded(contextBytes, maxContextFileBytes, &sessionContext); err != nil ||
		sessionContext.Protocol != ProtocolVersion || sessionContext.SubjectID == "" ||
		!filepath.IsAbs(sessionContext.MailboxDir) || filepath.Clean(sessionContext.MailboxDir) != sessionContext.MailboxDir {
		_ = contextDir.Close()
		return nil, ErrInvalidRecord
	}
	credential, err := loadCredentialAt(contextDir)
	if err != nil {
		_ = contextDir.Close()
		return nil, err
	}
	if credential.SubjectID != sessionContext.SubjectID || credential.Kind != access.SubjectSession {
		_ = contextDir.Close()
		return nil, ErrInvalidRecord
	}
	mailbox, err := openAbsoluteDirNoFollow(sessionContext.MailboxDir)
	if err != nil {
		_ = contextDir.Close()
		return nil, fmt.Errorf("open own mailbox: %w", err)
	}
	if err := validatePrivateDir(mailbox); err != nil {
		_ = mailbox.Close()
		_ = contextDir.Close()
		return nil, err
	}
	requests, err := openDirAt(mailbox, RequestsDirName)
	if err != nil {
		_ = mailbox.Close()
		_ = contextDir.Close()
		return nil, err
	}
	if err := validatePrivateDir(requests); err != nil {
		_ = requests.Close()
		_ = mailbox.Close()
		_ = contextDir.Close()
		return nil, err
	}
	responses, err := openDirAt(mailbox, ResponsesDirName)
	if err != nil {
		_ = requests.Close()
		_ = mailbox.Close()
		_ = contextDir.Close()
		return nil, err
	}
	if err := validatePrivateDir(responses); err != nil {
		_ = responses.Close()
		_ = requests.Close()
		_ = mailbox.Close()
		_ = contextDir.Close()
		return nil, err
	}
	clock := options.clock
	if clock == nil {
		clock = time.Now
	}
	random := options.random
	if random == nil {
		random = rand.Reader
	}
	poll := options.poll
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	return &Client{
		contextDir: contextDir, mailbox: mailbox, requests: requests, responses: responses,
		context: sessionContext, clock: clock, random: random, poll: poll,
		beforeRequestPublish: options.beforeRequestPublish,
		afterServiceStat:     options.afterServiceStat,
		beforeResponseRead:   options.beforeResponseRead,
	}, nil
}

func (c *Client) Query(ctx context.Context, query Query) (Result, error) {
	return c.Do(ctx, query.call())
}

func (c *Client) Action(ctx context.Context, action Action) (Result, error) {
	return c.Do(ctx, action.call())
}

func (c *Client) Do(ctx context.Context, call Call) (Result, error) {
	call = cloneCall(call)
	if err := validateCall(call); err != nil || call.Kind != CallOperation {
		return Result{}, ErrInvalidRecord
	}
	if err := c.checkOpen(); err != nil {
		return Result{}, err
	}
	body, err := marshalBounded(call, access.MaxBodyBytes)
	if err != nil {
		return Result{}, err
	}
	credential, err := loadCredentialAt(c.contextDir)
	if err != nil {
		return Result{}, err
	}
	if credential.SubjectID != c.context.SubjectID || credential.Kind != access.SubjectSession {
		return Result{}, ErrInvalidRecord
	}
	service, err := c.awaitService(ctx, credential)
	if errors.Is(err, os.ErrNotExist) {
		return Result{}, errors.Join(ErrUnpublished, err)
	}
	if err != nil {
		return Result{}, err
	}
	envelope, err := access.SignRequest(credential, service.BootID, body, c.clock(), c.random)
	if err != nil {
		return Result{}, err
	}
	encoded, err := marshalBounded(envelope, maxEnvelopeFileBytes)
	if err != nil {
		return Result{}, err
	}
	if err := atomicWriteAtObserved(c.requests, requestFileName(envelope.RequestID), encoded, c.random, c.beforeRequestPublish); err != nil {
		return Result{RequestID: envelope.RequestID}, errors.Join(ErrIndeterminate,
			fmt.Errorf("publish session RPC request: %w", err))
	}
	response, err := c.awaitResponse(ctx, credential, service, envelope)
	if errors.Is(err, ErrNotExecuted) {
		return Result{RequestID: envelope.RequestID}, err
	}
	if err != nil {
		return Result{RequestID: envelope.RequestID}, errors.Join(ErrIndeterminate, err)
	}
	result := Result{
		Status: response.Status, Code: response.Code, Body: append([]byte(nil), response.Body...),
		RequestID: envelope.RequestID,
	}
	if response.ReceiptRequired {
		result.ReceiptSent = c.sendReceipt(credential, service, response) == nil
	} else {
		// Best effort: an unsent release only leaves the response to age cleanup.
		_ = c.sendAck(credential, service, CallRelease, response)
	}
	if response.Status != StatusOK {
		if response.Status == StatusReplay || response.Status == StatusIndeterminate {
			return result, fmt.Errorf("%w: %s", ErrIndeterminate, response.Code)
		}
		return result, &RemoteError{Status: response.Status, Code: response.Code}
	}
	return result, nil
}

// awaitService loads service.json, waiting within ctx while it does not exist
// yet. A session's processes start as soon as it is created, but its mailbox
// is published by the daemon's next reconcile, so a credential helper or hook
// that runs first would otherwise fail. Nothing has been sent at this point,
// so waiting is always safe; when ctx ends the not-exist error is returned.
func (c *Client) awaitService(ctx context.Context, credential access.Credential) (Service, error) {
	service, err := c.loadService(credential)
	if !errors.Is(err, os.ErrNotExist) {
		return service, err
	}
	// service.json is renamed into the mailbox directory: wake on that, and
	// poll where the directory cannot be watched.
	wake := newWakeSignal()
	interval := c.poll
	if watch, watchErr := watchDir(c.mailbox, c.context.MailboxDir, wake.notify); watchErr == nil {
		defer watch.Close()
		interval = max(interval, WatchFallbackInterval)
	}
	// Publication may have landed before the watch existed.
	service, err = c.loadService(credential)
	for errors.Is(err, os.ErrNotExist) {
		waitWake(ctx.Done(), wake, interval)
		if ctx.Err() != nil {
			return Service{}, err
		}
		service, err = c.loadService(credential)
	}
	return service, err
}

func (c *Client) loadService(credential access.Credential) (Service, error) {
	var (
		data []byte
		err  error
	)
	// service.json is the sole replaceable protocol record. A reader can hold
	// the prior inode while atomic replacement drops its link count/changes its
	// ctime; retry that transient strict-file rejection against the held mailbox
	// directory. Unique request/response records are never retried this way.
	for attempt := 0; attempt < 3; attempt++ {
		data, err = readRegularAtObserved(c.mailbox, ServiceFileName, maxServiceFileBytes, c.afterServiceStat)
		if err == nil || !errors.Is(err, ErrInvalidRecord) {
			break
		}
	}
	if err != nil {
		return Service{}, err
	}
	var service Service
	if err := unmarshalBounded(data, maxServiceFileBytes, &service); err != nil {
		return Service{}, err
	}
	if err := verifyService(credential, service); err != nil {
		return Service{}, err
	}
	if service.SubjectID != c.context.SubjectID {
		return Service{}, ErrInvalidRecord
	}
	return service, nil
}

// awaitResponse waits for request's response. While the request is still
// unclaimed the client may withdraw it: when it expires past the point any
// daemon could accept it, or when ctx ends first. The daemon claims a request
// by renaming it out of requests/, so exactly one of that rename and the
// client's unlink succeeds; a successful unlink proves the request never ran
// (ErrNotExecuted). Once the unlink finds it gone, it may have run, and only a
// response can say what happened.
func (c *Client) awaitResponse(ctx context.Context, credential access.Credential, service Service, request access.SignedRequest) (Response, error) {
	cancelAt := time.UnixMilli(request.ExpiresAt).Add(requestCancelAfter)
	claimed := false
	withdraw := func() bool {
		if claimed {
			return false
		}
		if err := unix.Unlinkat(int(c.requests.Fd()), requestFileName(request.RequestID), 0); err != nil {
			claimed = true // gone (claimed) or unremovable: no longer ours to cancel
			return false
		}
		_ = unix.Fsync(int(c.requests.Fd()))
		return true
	}
	// Wake when the response is published, or service.json is replaced by a
	// restart, rather than on a tick. The watches are registered before the
	// first read below, so a response that landed before them is still found.
	// Without both (a sandbox that refuses them), poll as before.
	wake := newWakeSignal()
	interval := c.poll
	responses, responsesErr := watchDir(c.responses, filepath.Join(c.context.MailboxDir, ResponsesDirName), wake.notify)
	defer responses.Close()
	mailbox, mailboxErr := watchDir(c.mailbox, c.context.MailboxDir, wake.notify)
	defer mailbox.Close()
	if responsesErr == nil && mailboxErr == nil {
		interval = max(interval, WatchFallbackInterval)
	}
	for {
		if c.beforeResponseRead != nil {
			c.beforeResponseRead()
		}
		response, found, err := c.readResponse(credential, service, request)
		if err != nil {
			return Response{}, err
		}
		if found {
			return response, nil
		}
		currentService, err := c.loadService(credential)
		if err != nil {
			return Response{}, err
		}
		if currentService.BootID != service.BootID {
			return Response{}, ErrRestarted
		}
		wait := interval
		if !claimed {
			untilCancel := cancelAt.Sub(c.clock())
			if untilCancel <= 0 {
				if withdraw() {
					return Response{}, fmt.Errorf("%w: request expired before the daemon claimed it", ErrNotExecuted)
				}
				continue // claimed meanwhile: its response may already be there
			}
			wait = min(wait, untilCancel)
		}
		waitWake(ctx.Done(), wake, wait)
		if err := ctx.Err(); err != nil {
			if withdraw() {
				return Response{}, fmt.Errorf("%w: %w", ErrNotExecuted, err)
			}
			if response, found, readErr := c.readResponse(credential, service, request); readErr == nil && found {
				return response, nil
			}
			return Response{}, fmt.Errorf("%w: %w", ErrIndeterminate, err)
		}
	}
}

func (c *Client) readResponse(credential access.Credential, service Service, request access.SignedRequest) (Response, bool, error) {
	data, err := readRegularAt(c.responses, responseFileName(request.RequestID), maxEnvelopeFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return Response{}, false, nil
	}
	if err != nil {
		return Response{}, false, err
	}
	var response Response
	if err := unmarshalBounded(data, maxEnvelopeFileBytes, &response); err != nil {
		return Response{}, false, err
	}
	if err := verifyResponse(credential, response); err != nil {
		return Response{}, false, err
	}
	requestDigest := sha256.Sum256(request.Body)
	if response.BootID != service.BootID || response.SubjectID != c.context.SubjectID ||
		response.KeyID != request.KeyID || response.Generation != request.Generation ||
		response.RequestID != request.RequestID || response.RequestBodyDigest != hex.EncodeToString(requestDigest[:]) {
		return Response{}, false, ErrInvalidSignature
	}
	return response, true, nil
}

func (c *Client) sendReceipt(originalCredential access.Credential, originalService Service, response Response) error {
	credential, err := loadCredentialAt(c.contextDir)
	if err != nil {
		return err
	}
	if credential.SubjectID != c.context.SubjectID || credential.Kind != access.SubjectSession {
		return ErrInvalidRecord
	}
	service, err := c.loadService(credential)
	if err != nil {
		return err
	}
	// A restart after receiving a durable response does not invalidate that
	// success, but its receipt belongs to the old server and grace will settle it.
	if service.BootID != originalService.BootID || credential.KeyID != originalCredential.KeyID || credential.Generation != originalCredential.Generation {
		return ErrRestarted
	}
	return c.sendAck(credential, service, CallReceipt, response)
}

// sendAck publishes a receipt or release naming response. Neither awaits a
// result: a receipt settles through the server's grace window and a release
// produces no response at all.
func (c *Client) sendAck(credential access.Credential, service Service, kind CallKind, response Response) error {
	call := Call{Kind: kind, Receipt: &Receipt{RequestID: response.RequestID, ResponseDigest: responseDigest(response)}}
	body, err := marshalBounded(call, access.MaxBodyBytes)
	if err != nil {
		return err
	}
	envelope, err := access.SignRequest(credential, service.BootID, body, c.clock(), c.random)
	if err != nil {
		return err
	}
	encoded, err := marshalBounded(envelope, maxEnvelopeFileBytes)
	if err != nil {
		return err
	}
	return atomicWriteAt(c.requests, requestFileName(envelope.RequestID), encoded, c.random)
}

func loadCredentialAt(dir *os.File) (access.Credential, error) {
	data, err := readRegularAtMode(dir, CredentialFile, maxCredentialFileBytes, 0o400)
	if err != nil {
		return access.Credential{}, fmt.Errorf("read session credential: %w", err)
	}
	var credential access.Credential
	if err := unmarshalBounded(data, maxCredentialFileBytes, &credential); err != nil {
		return access.Credential{}, err
	}
	if credential.Protocol != access.ProtocolVersion || !validRequestID(credential.KeyID) || credential.SubjectID == "" ||
		credential.Generation == 0 || credential.PrivateKey == "" || !validRequestID(credential.IssuerKeyID) || credential.IssuerPublicKey == "" {
		return access.Credential{}, ErrInvalidRecord
	}
	return credential, nil
}

func (c *Client) checkOpen() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.closed {
		return ErrClosed
	}
	return nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	var joined error
	for _, file := range []*os.File{c.responses, c.requests, c.mailbox, c.contextDir} {
		if file != nil {
			joined = errors.Join(joined, file.Close())
		}
	}
	return joined
}
