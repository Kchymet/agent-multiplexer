package nativetui

import (
	"context"
	"errors"
	"sync"

	"amux/internal/daemon"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// A ceremony belongs to this dedicated authenticated connection, never to the
// pane/event stream. Closing it cancels even a login whose reply was lost.
type machineAccountTransport struct {
	mu     sync.Mutex
	client accountConnection
	closed bool
}

type accountConnection interface {
	Accounts(harnessproto.AccountRequest, bool) (harnessproto.AccountResult, error)
	Close() error
}

func (t *machineAccountTransport) close() {
	t.mu.Lock()
	t.closed = true
	c := t.client
	t.client = nil
	t.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

func (t *machineAccountTransport) connection(ctx context.Context) (accountConnection, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, context.Canceled
	}
	if t.client != nil {
		c := t.client
		t.mu.Unlock()
		return c, nil
	}
	t.mu.Unlock()
	c, err := daemon.DialContext(ctx)
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		_ = c.Close()
		return nil, context.Canceled
	}
	t.client = c
	return c, nil
}

func accountWireRequest(req accountRequest) (harnessproto.AccountRequest, bool, error) {
	op := req.op
	if op == "progress" {
		op = harnessproto.AccountPoll
	}
	ceremony := false
	switch op {
	case harnessproto.AccountStatus:
		return harnessproto.AccountRequest{Operation: op}, false, nil
	case harnessproto.AccountLogin, harnessproto.AccountPoll, harnessproto.AccountCancel, harnessproto.AccountComplete, harnessproto.AccountDisconnect:
		ceremony = true
	case harnessproto.AccountDefault, harnessproto.AccountLogout:
	default:
		return harnessproto.AccountRequest{}, false, errAccountsUnsupported
	}
	wire := harnessproto.AccountRequest{Operation: op, ProfileID: req.profile, Revision: req.revision, AttemptID: req.attempt}
	if op == harnessproto.AccountDefault {
		wire.DefaultRevision = req.defaultRevision
	}
	if ceremony {
		// The daemon namespaces this label by the authenticated connection.
		wire.Initiator = "native-tui"
	}
	if op == harnessproto.AccountComplete {
		wire.Secret = req.secret
	}
	if op == harnessproto.AccountPoll || op == harnessproto.AccountCancel || op == harnessproto.AccountComplete {
		wire.Revision = 0
	}
	if !harnessproto.ValidateAccountRequest(wire, ceremony) {
		return harnessproto.AccountRequest{}, false, errors.New("invalid account request")
	}
	return wire, ceremony, nil
}

func (t *machineAccountTransport) call(ctx context.Context, req accountRequest) (accountResponse, error) {
	wire, ceremony, err := accountWireRequest(req)
	if err != nil {
		return accountResponse{}, err
	}
	c, err := t.connection(ctx)
	if err != nil {
		return accountResponse{}, err
	}
	// Client.Accounts owns framing. Its bounded lifetime here closes the owner
	// connection, fencing late completion. Never reconnect/replay a mutation.
	stop := context.AfterFunc(ctx, t.close)
	defer stop()
	result, err := c.Accounts(wire, ceremony)
	if err != nil || ctx.Err() != nil {
		t.close()
		if err != nil && err.Error() == `unknown query "accounts"` {
			return accountResponse{}, errAccountsUnsupported
		}
		return accountResponse{}, errors.New("account transport unavailable")
	}
	if !result.OK {
		return accountResponse{}, classifyAccountError(result.Error)
	}
	var out accountResponse
	if result.Snapshot != nil {
		if result.Snapshot.Version != 1 {
			return accountResponse{}, errAccountsUnsupported
		}
		s := projectAccounts(*result.Snapshot)
		out.snapshot = &s
	}
	if result.Ceremony != nil && ceremony {
		projected := projectAccountCeremony(*result.Ceremony)
		out.attempt = &projected
	}
	if req.op == "status" && out.snapshot == nil {
		return accountResponse{}, errAccountsUnsupported
	}
	return out, nil
}

func classifyAccountError(code string) error {
	switch code {
	case "account_access_denied", "access_denied", "denied", "forbidden":
		return errAccountsDenied
	case "account_revision_changed", "revision_conflict", "conflict", "stale_revision":
		return errAccountsConflict
	case "account_operation_unsupported", "unsupported", "unknown_query", "unsupported_operation":
		return errAccountsUnsupported
	case "account_native_unavailable", "account_login_unavailable":
		return errAccountsNative
	case "account_secret_invalid":
		return errAccountsSecret
	case "account_attempt_unknown", "account_attempt_finished":
		return errAccountsAttempt
	}
	// Never return arbitrary native/provider error text to the view.
	return errors.New("account request failed")
}
