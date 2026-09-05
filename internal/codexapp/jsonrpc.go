// Package codexapp is amux's supervisor for a Codex App Server: it owns a
// background `codex app-server` process per structured session (AGE-181),
// listening on a per-session Unix socket inside the session's private scope, and
// speaks the App Server JSON-RPC protocol to it as a client. The App Server's
// lifetime is owned here — never by a UI pane — so closing the native Codex TUI
// or a remote client never stops the server or an in-flight turn.
//
// This package is the amux (provider-side) counterpart to the harness pilot
// adapter validated in AGE-179/PR118: the wire protocol is identical (initialize
// → thread/start|resume → turn/start|steer|interrupt, server-initiated approval
// requests), so both amux's own client and a native `codex --remote` client can
// drive the same server/thread. amux normalizes the App Server's event stream
// into the shared harnessproto runtime-event vocabulary (docs/
// remote-provider-sessions.md §4), the same events the on-disk rollout tailer
// produces — so a consumer sees one story regardless of control mode.
//
// jsonrpc.go is the minimal JSON-RPC 2.0 framing the App Server speaks over its
// newline-delimited transport:
//
//   - client→server requests (initialize, thread/*, turn/*) get an integer id and
//     block in call() until the matching response arrives;
//   - client→server notifications (initialized) carry no id (notify());
//   - server→client notifications (thread/*, turn/*, item/*) dispatch to onNotify;
//   - server→client requests (item/*/requestApproval, tool/requestUserInput) carry
//     their own id and dispatch to onRequest; the supervisor answers with respond().
//
// Wire note (App Server docs): stdio/socket frames omit the "jsonrpc":"2.0"
// member, so we neither emit nor require it — a message is classified purely by
// which of {method, id, result/error} it carries.
package codexapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// errClosed is returned by the rpc layer once the transport is torn down.
var errClosed = errors.New("codexapp: connection closed")

// msgConn is a message-framed transport: one JSON-RPC object per message. The App
// Server speaks JSON-RPC over WebSocket (HTTP Upgrade over a unix socket or
// loopback TCP), so a message boundary is a WebSocket frame — NOT a newline. The
// production implementation is wsConn (wsconn.go); tests use an in-memory pair.
// Raw newline-delimited JSONL is never written to the App Server's listener (the
// real binary closes such a connection immediately — ROOT audit f4483d7e).
type msgConn interface {
	ReadMessage() ([]byte, error)
	WriteMessage([]byte) error
	Close() error
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// rpcResponse is the decoded result/error of one call().
type rpcResponse struct {
	Result json.RawMessage
	Err    *rpcError
}

// rpcConn multiplexes requests, responses, notifications and server-initiated
// requests over one message-framed transport (a WebSocket connection). The read
// loop (run) owns all reads; call() registers a pending waiter the read loop wakes
// on the matching response id.
type rpcConn struct {
	transport msgConn

	writeMu sync.Mutex // serialize writes to the transport

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcResponse
	closed  bool

	// Handlers set by the supervisor before run() starts. Both may be nil.
	onNotify  func(method string, params json.RawMessage)
	onRequest func(id json.RawMessage, method string, params json.RawMessage)
}

func newRPCConn(t msgConn) *rpcConn {
	return &rpcConn{transport: t, pending: map[int64]chan rpcResponse{}}
}

// incoming is the union shape of every message on the wire; classification keys
// off which members are populated.
type incoming struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// run reads and dispatches messages until the transport closes or ctx is
// cancelled, then fails every pending call so no caller hangs on a dead
// connection. Each WebSocket message is one JSON-RPC object.
func (c *rpcConn) run(ctx context.Context) error {
	for {
		msg, err := c.transport.ReadMessage()
		if err != nil {
			c.failPending()
			return err
		}
		if ctx.Err() != nil {
			c.failPending()
			return ctx.Err()
		}
		line := bytes.TrimSpace(msg)
		if len(line) == 0 {
			continue
		}
		c.dispatch(line)
	}
}

// unparsableMethod is the synthetic notification method a non-JSON-RPC line is
// routed under so it still reaches raw passthrough (never dropped, §4).
const unparsableMethod = "$unparsable"

// dispatch classifies one frame and routes it.
func (c *rpcConn) dispatch(line []byte) {
	var m incoming
	if err := json.Unmarshal(line, &m); err != nil {
		if c.onNotify != nil {
			c.onNotify(unparsableMethod, line)
		}
		return
	}
	hasID := len(m.ID) > 0 && string(m.ID) != "null"
	switch {
	case m.Method != "" && hasID:
		if c.onRequest != nil {
			c.onRequest(m.ID, m.Method, m.Params)
		}
	case m.Method != "":
		if c.onNotify != nil {
			c.onNotify(m.Method, m.Params)
		}
	case hasID:
		c.deliver(m.ID, rpcResponse{Result: m.Result, Err: m.Error})
	default:
		// Neither method nor id — nothing actionable.
	}
}

func (c *rpcConn) deliver(rawID json.RawMessage, resp rpcResponse) {
	var id int64
	if err := json.Unmarshal(rawID, &id); err != nil {
		return // a non-int id the server uses for its own requests, not our call
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- resp
	}
}

// call issues a request and blocks until its response arrives, ctx is done, or
// the connection closes.
func (c *rpcConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errClosed
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.write(map[string]any{"method": method, "id": id, "params": params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Err != nil {
			return nil, fmt.Errorf("codexapp: %s: %w", method, resp.Err)
		}
		return resp.Result, nil
	}
}

// notify sends a fire-and-forget notification (no id, no response).
func (c *rpcConn) notify(method string, params any) error {
	return c.write(map[string]any{"method": method, "params": params})
}

// respond answers a server-initiated request, echoing its id verbatim so the
// server correlates the reply to its request.
func (c *rpcConn) respond(id json.RawMessage, result any) error {
	return c.write(map[string]any{"id": id, "result": result})
}

// respondErr answers a server-initiated request with a JSON-RPC error.
func (c *rpcConn) respondErr(id json.RawMessage, code int, message string) error {
	return c.write(map[string]any{"id": id, "error": rpcError{Code: code, Message: message}})
}

func (c *rpcConn) write(obj any) error {
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return errClosed
	}
	// One JSON-RPC object per WebSocket message — never a newline-framed byte
	// stream to the App Server listener.
	return c.transport.WriteMessage(b)
}

// failPending drains every waiting call with a closed-connection error so no
// caller blocks forever once the transport is gone.
func (c *rpcConn) failPending() {
	c.mu.Lock()
	pend := c.pending
	c.pending = map[int64]chan rpcResponse{}
	c.mu.Unlock()
	for _, ch := range pend {
		select {
		case ch <- rpcResponse{Err: &rpcError{Code: -1, Message: "connection closed"}}:
		default:
		}
	}
}

func (c *rpcConn) close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	err := c.transport.Close()
	c.failPending()
	return err
}
