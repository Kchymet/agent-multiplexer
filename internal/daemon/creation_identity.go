package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"amux/internal/core"
	"amux/internal/store"
)

// creation_identity.go is the host-only creation identity protocol: a host
// client that must later tell "the session it created" from an unrelated
// session reusing the id (the direct server's idempotent replays) asks the
// daemon for the created session's store incarnation (store.Identity).
//
// The protocol is additive and negotiated per connection:
//   - QueryHostCapabilities answers HostCapabilities. An older daemon answers it
//     with an unknown-query error, so a client learns that before it sends any
//     creation, never after a creation it cannot verify.
//   - A creation action sent inside a hostActionFrame with Identity set is run
//     through wsops.DispatchIdentity, and its result frame carries the
//     identity next to the unchanged core.Result fields.
//   - QuerySessionIdentities reads current identities by id (at most
//     store.MaxIdentityLookup).
//
// None of this is reachable outside the authenticated host stream: session
// RPC decodes its own closed Call vocabulary and denies every query outside
// its allowlist, and the identity request reaches the dispatch only through an
// unexported context value the host loop alone sets. core.Result, core.Action
// and core.Session are unchanged, so session RPC (which serializes the whole
// core.Result), prwatch, the mux and the provider never see an incarnation.

// Host-only query names. They are not in core: no restricted surface lists
// them, and session RPC's restricted query allowlist denies them.
const (
	QueryHostCapabilities  = "host-capabilities"
	QuerySessionIdentities = "session-identities"
)

// CreationIdentityVersion is the creation identity protocol HostCapabilities
// advertises.
const CreationIdentityVersion = 1

// HostCapabilities is QueryHostCapabilities' answer.
type HostCapabilities struct {
	CreationIdentity int `json:"creationIdentity"`
}

// IdentityRequest is the opt-in a host client attaches to a creation action.
// ExpectRoot, when set, pins an add-agent to that incarnation of its workgroup
// root; it can only make the daemon refuse, never redirect a creation.
type IdentityRequest struct {
	ExpectRoot string `json:"expectRoot,omitempty"`
}

// hostActionFrame is one line a host client sends: a core.Action, optionally
// with the identity opt-in. An older daemon decodes the same line as the bare
// action and ignores Identity, which is why a client negotiates first.
type hostActionFrame struct {
	core.Action
	Identity *IdentityRequest `json:"identity,omitempty"`
}

// identityResult is the result frame of an identity creation: the ordinary
// core.Result fields plus Identity (on success).
type identityResult struct {
	core.Result
	Identity *store.Identity `json:"identity,omitempty"`
}

// ErrDaemonUpgradeRequired means the daemon on the other end of a connection
// does not speak the creation identity protocol. Nothing was sent that could
// create anything.
var ErrDaemonUpgradeRequired = errors.New("the amux daemon does not support creation identities; upgrade and restart it")

type identityCallKey struct{}

// identityCall carries one host identity creation through handle: the request
// in, the created identity out.
type identityCall struct {
	req      IdentityRequest
	identity store.Identity
	set      bool
}

func withIdentityCall(ctx context.Context, call *identityCall) context.Context {
	return context.WithValue(ctx, identityCallKey{}, call)
}

func identityCallFrom(ctx context.Context) *identityCall {
	call, _ := ctx.Value(identityCallKey{}).(*identityCall)
	return call
}

// hostIdentityAction runs one opted-in host action and returns the frame to
// send. A verb that creates nothing is refused before it runs.
func (d *Daemon) hostIdentityAction(ctx context.Context, f hostActionFrame) any {
	if !core.DescriptorFor(f.Action.Action).CreatesSession {
		return identityResult{Result: fail("creation identity applies only to creation verbs")}
	}
	call := &identityCall{req: *f.Identity}
	res := d.handle(withIdentityCall(ctx, call), f.Action)
	out := identityResult{Result: res}
	if res.OK && call.set && call.identity.ID == res.NewID {
		id := call.identity
		out.Identity = &id
	}
	return out
}

// sessionIdentities answers QuerySessionIdentities: Fields["ids"] is a
// comma-separated id list.
func sessionIdentities(a core.Action) (any, error) {
	var ids []string
	for _, id := range strings.Split(a.Fields["ids"], ",") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || len(ids) > store.MaxIdentityLookup {
		return nil, fmt.Errorf("session identities: 1-%d ids", store.MaxIdentityLookup)
	}
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	found, err := db.SessionIdentities(ids)
	if err != nil {
		return nil, err
	}
	rows := make([]store.SessionIdentity, 0, len(found))
	for _, id := range ids {
		if row, ok := found[id]; ok {
			rows = append(rows, row)
			delete(found, id)
		}
	}
	return rows, nil
}

// ── client ────────────────────────────────────────────────────────────────

// NegotiateCreationIdentity asks this connection's daemon whether it speaks
// the creation identity protocol. It must succeed on a connection before
// DispatchIdentity is used on it; a new connection negotiates again. An older
// daemon (an unknown-query answer) is ErrDaemonUpgradeRequired.
func (c *Client) NegotiateCreationIdentity() error {
	raw, err := c.queryActionData(core.Action{Action: core.ActionQuery, Query: QueryHostCapabilities})
	if err != nil {
		return err
	}
	if raw == nil {
		c.identityOK.Store(false)
		return ErrDaemonUpgradeRequired
	}
	var caps HostCapabilities
	if err := json.Unmarshal(raw, &caps); err != nil {
		return err
	}
	ok := caps.CreationIdentity >= CreationIdentityVersion
	c.identityOK.Store(ok)
	if !ok {
		return ErrDaemonUpgradeRequired
	}
	return nil
}

// DispatchIdentity runs one creation action and returns the created session's
// identity. It refuses, without sending anything, on a connection that has not
// negotiated the protocol. A daemon refusal is a *RejectedError as for
// Dispatch; a success without a complete identity is an error (the creation
// may have happened).
func (c *Client) DispatchIdentity(a core.Action, req IdentityRequest) (store.Identity, error) {
	if !c.identityOK.Load() {
		return store.Identity{}, ErrDaemonUpgradeRequired
	}
	b, err := json.Marshal(hostActionFrame{Action: a, Identity: &req})
	if err != nil {
		return store.Identity{}, err
	}
	if err := c.sendRaw(b); err != nil {
		return store.Identity{}, err
	}
	for {
		frame, err := c.Next()
		if err != nil {
			return store.Identity{}, err
		}
		if frame.Result == nil {
			continue
		}
		if !frame.Result.OK {
			return store.Identity{}, &RejectedError{Msg: frame.Result.Error}
		}
		if frame.Identity == nil || !frame.Identity.Complete() || frame.Identity.ID != frame.Result.NewID {
			return store.Identity{}, errIdentityMissing
		}
		return *frame.Identity, nil
	}
}

// errIdentityMissing is a successful creation answer without a usable
// identity: the session may exist, so the caller must treat the outcome as
// unknown.
var errIdentityMissing = errors.New("the daemon reported a creation without its identity")

// SessionIdentities reads the current identity of each id (at most
// store.MaxIdentityLookup); a missing session is absent from the map. An
// older daemon is ErrDaemonUpgradeRequired.
func (c *Client) SessionIdentities(ids []string) (map[string]store.SessionIdentity, error) {
	if len(ids) == 0 {
		return map[string]store.SessionIdentity{}, nil
	}
	if len(ids) > store.MaxIdentityLookup {
		return nil, fmt.Errorf("at most %d session identities per lookup", store.MaxIdentityLookup)
	}
	if !c.identityOK.Load() {
		if err := c.NegotiateCreationIdentity(); err != nil {
			return nil, err
		}
	}
	raw, err := c.queryActionData(core.Action{Action: core.ActionQuery, Query: QuerySessionIdentities,
		Fields: map[string]string{"ids": strings.Join(ids, ",")}})
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("session identities: the daemon refused the lookup")
	}
	var rows []store.SessionIdentity
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]store.SessionIdentity, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out, nil
}

// queryActionData is queryAction that reports a daemon's not-OK data answer
// as (nil, nil) rather than an error, so a caller can tell "the daemon
// answered no" from a transport failure.
func (c *Client) queryActionData(a core.Action) (json.RawMessage, error) {
	if err := c.Send(a); err != nil {
		return nil, err
	}
	for {
		f, err := c.Next()
		if err != nil {
			return nil, err
		}
		if f.Data == nil {
			continue
		}
		if !f.Data.OK {
			return nil, nil
		}
		return f.Data.Rows, nil
	}
}
