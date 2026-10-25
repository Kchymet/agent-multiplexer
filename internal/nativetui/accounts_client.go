package nativetui

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"amux/internal/core"
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
	SendContext(context.Context, core.Action) error
	NextContext(context.Context) (daemon.Frame, error)
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

func accountWireRequest(req accountRequest) (core.Action, error) {
	op := req.op
	if op == "progress" {
		op = "poll"
	}
	query := "account-manage"
	switch op {
	case "status":
		return core.Action{Action: core.ActionQuery, Query: "accounts"}, nil
	case "login", "poll", "cancel", "complete", "disconnect":
		query = "account-ceremony"
	case "default", "logout":
	default:
		return core.Action{}, errAccountsUnsupported
	}
	wire := harnessproto.AccountRequest{Operation: op, ProfileID: req.profile, Revision: req.revision, AttemptID: req.attempt}
	if query == "account-ceremony" {
		// The daemon namespaces this label by the authenticated connection.
		wire.Initiator = "native-tui"
	}
	if op == "complete" {
		wire.Secret = req.secret
	}
	if op == "poll" || op == "cancel" || op == "complete" {
		wire.Revision = 0
	}
	if !harnessproto.ValidateAccountRequest(wire, query == "account-ceremony") {
		return core.Action{}, errors.New("invalid account request")
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		return core.Action{}, err
	}
	return core.Action{Action: core.ActionQuery, Query: query, Fields: map[string]string{"request": string(raw)}}, nil
}

func (t *machineAccountTransport) call(ctx context.Context, req accountRequest) (accountResponse, error) {
	a, err := accountWireRequest(req)
	if err != nil {
		return accountResponse{}, err
	}
	c, err := t.connection(ctx)
	if err != nil {
		return accountResponse{}, err
	}
	// A timeout closes the owner connection, fencing late completion. Do not
	// reconnect and replay mutations: the next explicit refresh reconciles.
	if err := c.SendContext(ctx, a); err != nil {
		t.close()
		return accountResponse{}, err
	}
	for {
		frame, err := c.NextContext(ctx)
		if err != nil {
			t.close()
			return accountResponse{}, err
		}
		if frame.Data == nil {
			continue
		}
		d := frame.Data
		if d.Query != a.Query || len(d.Rows) > 128<<10 {
			t.close()
			return accountResponse{}, errAccountsUnsupported
		}
		if !d.OK {
			return accountResponse{}, classifyAccountError(d.Error)
		}
		var result harnessproto.AccountResult
		if err := json.Unmarshal(d.Rows, &result); err != nil {
			return accountResponse{}, errAccountsUnsupported
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
		if result.Ceremony != nil && a.Query == "account-ceremony" {
			ceremony := projectAccountCeremony(*result.Ceremony)
			out.attempt = &ceremony
		}
		if req.op == "status" && out.snapshot == nil {
			return accountResponse{}, errAccountsUnsupported
		}
		return out, nil
	}
}

func classifyAccountError(code string) error {
	switch code {
	case "access_denied", "denied", "forbidden":
		return errAccountsDenied
	case "revision_conflict", "conflict", "stale_revision":
		return errAccountsConflict
	case "unsupported", "unknown_query", "unsupported_operation":
		return errAccountsUnsupported
	}
	// Never return arbitrary native/provider error text to the view.
	return errors.New("account request failed")
}
