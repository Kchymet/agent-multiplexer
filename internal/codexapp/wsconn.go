package codexapp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// wsconn.go dials the Codex App Server's WebSocket listener and adapts it to the
// message-framed msgConn the JSON-RPC layer expects. The App Server speaks
// JSON-RPC 2.0 over WebSocket (HTTP Upgrade) — over a Unix socket, loopback TCP,
// or, cross-machine, authenticated WSS.
//
// It uses gorilla/websocket rather than golang.org/x/net/websocket for one
// concrete reason (verified against Codex 0.153.4): the App Server's loopback TCP
// and WSS listeners reject any request carrying an `Origin` header with 403
// (DNS-rebinding protection), and x/net/websocket ALWAYS sends Origin (and panics
// if it is nil). gorilla sends no Origin unless we set one, so amux — a same-host
// or authenticated cross-host client, not a browser — connects to every listener
// (unix, loopback, wss). An Origin can still be set via Config.Origin when a server
// deployment allowlists a specific value.
//
// Endpoint forms (Config.Endpoint / --listen):
//
//	unix://<path>        WebSocket over a Unix socket — the per-session default,
//	                     kept inside the session's private sandbox scope.
//	ws://host:port       WebSocket over loopback TCP (colocated clients). A non-loopback
//	                     host is refused: an unauthenticated non-loopback listener is
//	                     never dialed.
//	wss://host:port      WebSocket over TLS (cross-machine). Verification is never
//	                     downgraded.
type wsConn struct {
	c *websocket.Conn
}

// WriteMessage sends one JSON-RPC object as a WebSocket TEXT frame.
func (w *wsConn) WriteMessage(b []byte) error {
	return w.c.WriteMessage(websocket.TextMessage, b)
}

// ReadMessage returns the next WebSocket message payload (text or binary).
func (w *wsConn) ReadMessage() ([]byte, error) {
	_, b, err := w.c.ReadMessage()
	return b, err
}

func (w *wsConn) Close() error { return w.c.Close() }

// dialWS connects to endpoint, completes the WebSocket handshake, and returns a
// msgConn. It omits the Origin header by default (see the package note); a
// non-loopback ws:// endpoint is refused up front. origin, when non-empty, is sent
// as the Origin header (for a server that allowlists one).
func dialWS(ctx context.Context, endpoint, origin string) (msgConn, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("codexapp: bad endpoint %q: %w", endpoint, err)
	}

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	var dialURL string
	switch u.Scheme {
	case "unix":
		sockPath := u.Path
		dialer.NetDialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
		}
		// The Host is irrelevant over a unix socket; a stable placeholder keeps the
		// handshake well-formed, and the path is "/".
		dialURL = "ws://localhost/"
	case "ws":
		if !isLoopbackHost(u.Hostname()) {
			return nil, fmt.Errorf("codexapp: refusing non-loopback ws:// endpoint %q (use wss:// for cross-machine)", endpoint)
		}
		dialURL = wsURLWithPath(u)
	case "wss":
		// TLS with default (full) verification — never skipped; ServerName follows
		// the endpoint host.
		dialer.TLSClientConfig = &tls.Config{ServerName: u.Hostname()}
		dialURL = wsURLWithPath(u)
	default:
		return nil, fmt.Errorf("codexapp: unsupported endpoint scheme %q", u.Scheme)
	}

	// gorilla omits Origin unless we add it — exactly what codex's Origin-rejecting
	// listeners need. Set it only when a server allowlists a specific Origin.
	var reqHeader http.Header
	if origin != "" {
		reqHeader = http.Header{"Origin": {origin}}
	}

	c, resp, err := dialer.DialContext(ctx, dialURL, reqHeader)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("codexapp: ws dial %s: %w (HTTP %s)", endpoint, err, resp.Status)
		}
		return nil, fmt.Errorf("codexapp: ws dial %s: %w", endpoint, err)
	}
	return &wsConn{c: c}, nil
}

// LoopbackEndpoint allocates a free loopback TCP port and returns a
// `ws://127.0.0.1:<port>` endpoint, for a colocated App Server reached over the
// shared network namespace. A tiny TOCTOU window remains (the server rebinds the
// port); a lost race surfaces as a Start error the caller retries.
func LoopbackEndpoint() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("codexapp: allocate loopback port: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return fmt.Sprintf("ws://127.0.0.1:%d", port), nil
}

// wsURLWithPath returns u as a ws/wss URL with a non-empty path. The App Server's
// WebSocket handler is at "/", and an empty path yields a malformed request line
// some servers reject, so default an empty path to "/".
func wsURLWithPath(u *url.URL) string {
	v := *u
	if v.Path == "" {
		v.Path = "/"
	}
	return v.String()
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
