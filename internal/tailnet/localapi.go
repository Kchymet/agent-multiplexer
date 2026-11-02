// Package tailnet is the small slice of tailscaled's LocalAPI that `amux direct
// enable --tailnet` needs: this node's MagicDNS name and Tailscale addresses
// (to derive the listen address and endpoint), the control server it is
// logged in to, and a TLS certificate for its MagicDNS name.
//
// It talks to tailscaled over its local socket with plain net/http instead of
// importing tailscale.com: the request shape is the documented one the
// official Go client uses (Host "local-tailscaled.sock", no Origin or Referer;
// see tailscale.com/client/local and ipn/localapi), and amux needs three GETs.
//
// Nothing here changes tailscaled: every call is a read, except that a cert
// request may make tailscaled obtain or renew the certificate it then caches.
// amux never changes the socket's permissions; when the current user may not
// fetch certificates the error says how the operator can grant it.
package tailnet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// LocalAPIHost is the Host header tailscaled's LocalAPI requires over its
// socket (apitype.LocalAPIHost upstream).
const LocalAPIHost = "local-tailscaled.sock"

// EnvSocket overrides the tailscaled socket path.
const EnvSocket = "AMUX_TAILSCALE_SOCKET"

// AdminDNSURL is where a tailnet admin enables MagicDNS and HTTPS certificates
// (tailscale.com/kb/1153/enabling-https).
const AdminDNSURL = "https://login.tailscale.com/admin/dns"

// DefaultControlURL is Tailscale's hosted control plane; the login URL is a
// synonym (ipn.DefaultControlURL / IsLoginServerSynonym upstream).
const DefaultControlURL = "https://controlplane.tailscale.com"

// DefaultSocket is tailscaled's socket on this platform when it runs as the
// open-source daemon. The macOS App Store app exposes LocalAPI differently
// (a loopback port and token) and is not supported here; pass --tls-cert and
// --tls-key there instead.
func DefaultSocket() string {
	if runtime.GOOS == "darwin" {
		return "/var/run/tailscaled.socket"
	}
	return "/var/run/tailscale/tailscaled.sock"
}

// Socket resolves the socket path: explicit, then $AMUX_TAILSCALE_SOCKET,
// then the platform default.
func Socket(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if s := os.Getenv(EnvSocket); s != "" {
		return s
	}
	return DefaultSocket()
}

// Client is a LocalAPI client bound to one socket.
type Client struct {
	Socket string
	// HTTP overrides the transport (tests). When nil, requests dial Socket.
	HTTP *http.Client
}

// Status is the subset of /localapi/v0/status amux reads. Field names match
// ipnstate.Status, which has no JSON tags.
type Status struct {
	BackendState   string
	Self           *PeerStatus
	CertDomains    []string
	CurrentTailnet *TailnetStatus
}

// PeerStatus is the subset of ipnstate.PeerStatus amux reads.
type PeerStatus struct {
	DNSName      string
	TailscaleIPs []netip.Addr
}

// TailnetStatus is ipnstate.TailnetStatus.
type TailnetStatus struct {
	Name            string
	MagicDNSSuffix  string
	MagicDNSEnabled bool
}

// Prefs is the subset of ipn.Prefs amux reads.
type Prefs struct {
	ControlURL string
}

// Errors, distinguished so preflight can say exactly what to do.
var (
	// ErrUnreachable: no tailscaled answered on the socket.
	ErrUnreachable = errors.New("tailscaled is not reachable")
	// ErrDenied: tailscaled refused the request for this user.
	ErrDenied = errors.New("tailscaled denied the request")
	// ErrHTTPSDisabled: the tailnet offers this node no certificate names.
	ErrHTTPSDisabled = errors.New("HTTPS certificates are not available for this node")
	// ErrRateLimited: the certificate authority is rate limiting this name.
	ErrRateLimited = errors.New("certificate issuance is rate limited")
)

// APIError is a LocalAPI answer other than 200.
type APIError struct {
	Path       string
	Code       int
	Body       string
	RetryAfter string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("tailscaled %s: HTTP %d: %s", e.Path, e.Code, e.Body)
}

// Unwrap maps the documented statuses onto the sentinel errors.
func (e *APIError) Unwrap() error {
	switch {
	case e.Code == http.StatusForbidden:
		return ErrDenied
	case e.Code == http.StatusTooManyRequests:
		return ErrRateLimited
	case strings.Contains(e.Body, "does not support getting TLS certs"):
		return ErrHTTPSDisabled
	}
	return nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	sock := c.Socket
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableKeepAlives: true,
	}}
}

// get performs one LocalAPI GET with a bounded body.
func (c *Client) get(ctx context.Context, path string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+LocalAPIHost+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		if unreachable(err) {
			return nil, fmt.Errorf("%w at %s: %v", ErrUnreachable, c.Socket, err)
		}
		return nil, fmt.Errorf("tailscaled %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("tailscaled %s: %w", path, err)
	}
	if int64(len(body)) > max {
		return nil, fmt.Errorf("tailscaled %s: response larger than %d bytes", path, max)
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, &APIError{Path: strings.SplitN(path, "?", 2)[0], Code: resp.StatusCode, Body: msg, RetryAfter: resp.Header.Get("Retry-After")}
	}
	return body, nil
}

func unreachable(err error) bool {
	var op *net.OpError
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOENT) || (errors.As(err, &op) && op.Op == "dial" && !errors.Is(err, os.ErrPermission))
}

// Status reads /localapi/v0/status without peers.
func (c *Client) Status(ctx context.Context) (Status, error) {
	b, err := c.get(ctx, "/localapi/v0/status?peers=false", 4<<20)
	if err != nil {
		return Status{}, err
	}
	var st Status
	if err := json.Unmarshal(b, &st); err != nil {
		return Status{}, fmt.Errorf("tailscaled status: %w", err)
	}
	return st, nil
}

// Prefs reads /localapi/v0/prefs (readable without operator rights).
func (c *Client) Prefs(ctx context.Context) (Prefs, error) {
	b, err := c.get(ctx, "/localapi/v0/prefs", 1<<20)
	if err != nil {
		return Prefs{}, err
	}
	var p Prefs
	if err := json.Unmarshal(b, &p); err != nil {
		return Prefs{}, fmt.Errorf("tailscaled prefs: %w", err)
	}
	return p, nil
}

// CertPair asks tailscaled for a certificate and key for domain, renewed
// synchronously when fewer than minValidity remain. The key is returned only
// to the caller; it is never logged.
func (c *Client) CertPair(ctx context.Context, domain string, minValidity time.Duration) (certPEM, keyPEM []byte, err error) {
	q := url.Values{"type": {"pair"}}
	if minValidity > 0 {
		q.Set("min_validity", minValidity.String())
	}
	b, err := c.get(ctx, "/localapi/v0/cert/"+url.PathEscape(domain)+"?"+q.Encode(), 1<<20)
	if err != nil {
		return nil, nil, err
	}
	return splitPair(b)
}

// splitPair splits tailscaled's type=pair answer — the key PEM, then the
// certificate chain PEM — the way the official client does, and refuses an
// answer whose certificate half carries a private key.
func splitPair(b []byte) (certPEM, keyPEM []byte, err error) {
	i := bytes.Index(b, []byte("--\n--"))
	if i < 0 {
		return nil, nil, errors.New("tailscaled cert: unexpected response shape")
	}
	keyPEM, certPEM = b[:i+3], b[i+3:]
	if bytes.Contains(certPEM, []byte(" PRIVATE KEY-----")) || !bytes.Contains(keyPEM, []byte(" PRIVATE KEY-----")) {
		return nil, nil, errors.New("tailscaled cert: unexpected response shape")
	}
	return certPEM, keyPEM, nil
}

// IsHostedControl reports whether a ControlURL is Tailscale's hosted control
// plane (empty means the default for a fresh install).
func IsHostedControl(controlURL string) bool {
	u := strings.TrimRight(controlURL, "/")
	return u == "" || u == DefaultControlURL || u == "https://login.tailscale.com"
}
