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
)

type Client struct {
	contextDir *os.File
	mailbox    *os.File
	requests   *os.File
	responses  *os.File
	context    SessionContext
	clock      func() time.Time
	random     io.Reader
	poll       time.Duration

	mu     sync.RWMutex
	closed bool
}

type clientOptions struct {
	clock  func() time.Time
	random io.Reader
	poll   time.Duration
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
	service, err := c.loadService(credential)
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
	if err := atomicWriteAt(c.requests, requestFileName(envelope.RequestID), encoded, c.random); err != nil {
		return Result{RequestID: envelope.RequestID}, errors.Join(ErrIndeterminate,
			fmt.Errorf("publish session RPC request: %w", err))
	}
	response, err := c.awaitResponse(ctx, credential, service, envelope)
	if err != nil {
		return Result{RequestID: envelope.RequestID}, errors.Join(ErrIndeterminate, err)
	}
	result := Result{
		Status: response.Status, Code: response.Code, Body: append([]byte(nil), response.Body...),
		RequestID: envelope.RequestID,
	}
	if response.ReceiptRequired {
		result.ReceiptSent = c.sendReceipt(credential, service, response) == nil
	}
	if response.Status != StatusOK {
		if response.Status == StatusReplay || response.Status == StatusIndeterminate {
			return result, fmt.Errorf("%w: %s", ErrIndeterminate, response.Code)
		}
		return result, &RemoteError{Status: response.Status, Code: response.Code}
	}
	return result, nil
}

func (c *Client) loadService(credential access.Credential) (Service, error) {
	data, err := readRegularAt(c.mailbox, ServiceFileName, maxServiceFileBytes)
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

func (c *Client) awaitResponse(ctx context.Context, credential access.Credential, service Service, request access.SignedRequest) (Response, error) {
	ticker := time.NewTicker(c.poll)
	defer ticker.Stop()
	for {
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
		select {
		case <-ctx.Done():
			return Response{}, fmt.Errorf("%w: %v", ErrIndeterminate, ctx.Err())
		case <-ticker.C:
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
	call := Call{Kind: CallReceipt, Receipt: &Receipt{RequestID: response.RequestID, ResponseDigest: responseDigest(response)}}
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
