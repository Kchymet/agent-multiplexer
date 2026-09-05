package codexapp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/websocket"
)

// wsconn.go dials the Codex App Server's WebSocket listener and adapts it to the
// message-framed msgConn the JSON-RPC layer expects. The App Server speaks
// JSON-RPC 2.0 over WebSocket (HTTP Upgrade) — over a Unix socket, loopback TCP,
// or, cross-machine, authenticated WSS (ROOT audit: raw JSONL over the unix socket
// is closed immediately; the correct handshake initializes). Each JSON-RPC object
// is one text frame.
//
// Endpoint forms (Config.Endpoint / --listen):
//
//	unix://<path>        WebSocket over a Unix socket — the per-session local
//	                     optimization, kept inside the session's private sandbox scope.
//	ws://host:port       WebSocket over loopback TCP (colocated clients). A non-loopback
//	                     host is refused: an unauthenticated non-loopback listener is
//	                     never opened/dialed.
//	wss://host:port      WebSocket over TLS (cross-machine). Verification is never
//	                     downgraded.
type wsConn struct {
	ws *websocket.Conn
}

func (c *wsConn) WriteMessage(b []byte) error {
	// Send a TEXT frame (JSON-RPC is text). websocket.Message sends []byte as a
	// binary frame, so pass a string to force the text opcode.
	return websocket.Message.Send(c.ws, string(b))
}

func (c *wsConn) ReadMessage() ([]byte, error) {
	var data []byte
	if err := websocket.Message.Receive(c.ws, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func (c *wsConn) Close() error { return c.ws.Close() }

// dialWS connects to endpoint and completes the WebSocket handshake, returning a
// msgConn. It dials the underlying net.Conn under ctx's deadline, then runs the
// client handshake over it (so the same code path serves unix, loopback TCP, and
// TLS). A non-loopback ws:// endpoint is refused up front.
func dialWS(ctx context.Context, endpoint string) (msgConn, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("codexapp: bad endpoint %q: %w", endpoint, err)
	}

	var network, addr, wsURL string
	switch u.Scheme {
	case "unix":
		network, addr = "unix", u.Path
		// The HTTP Host is irrelevant over a unix socket; a stable placeholder keeps
		// the handshake well-formed.
		wsURL = "ws://localhost/"
	case "ws":
		if !isLoopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("codexapp: refusing non-loopback ws:// endpoint %q (use wss:// for cross-machine)", endpoint)
		}
		network, addr = "tcp", u.Host
		wsURL = endpoint
	case "wss":
		network, addr = "tcp", u.Host
		wsURL = endpoint
	default:
		return nil, fmt.Errorf("codexapp: unsupported endpoint scheme %q", u.Scheme)
	}

	d := &net.Dialer{}
	raw, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, fmt.Errorf("codexapp: dial %s %s: %w", network, addr, err)
	}
	if u.Scheme == "wss" {
		// TLS with default (full) verification — never skipped. ServerName follows the
		// endpoint host.
		tlsConn := tls.Client(raw, &tls.Config{ServerName: u.Hostname()})
		if err := tlsHandshakeCtx(ctx, tlsConn); err != nil {
			_ = raw.Close()
			return nil, fmt.Errorf("codexapp: tls handshake %s: %w", addr, err)
		}
		raw = tlsConn
	}

	cfg, err := websocket.NewConfig(wsURL, "http://localhost")
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("codexapp: ws config: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(dl)
	}
	ws, err := websocket.NewClient(cfg, raw)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("codexapp: ws handshake %s: %w", endpoint, err)
	}
	// Clear the handshake deadline; reads block for the connection's life.
	_ = ws.SetDeadline(time.Time{})
	return &wsConn{ws: ws}, nil
}

// tlsHandshakeCtx runs the TLS handshake honoring ctx's deadline.
func tlsHandshakeCtx(ctx context.Context, c *tls.Conn) error {
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
		defer c.SetDeadline(time.Time{})
	}
	return c.HandshakeContext(ctx)
}

// isLoopbackHost reports whether h names the loopback interface, so a ws:// (no
// TLS, no auth) endpoint is only ever dialed for a colocated server.
func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(strings.Trim(h, "[]")); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
