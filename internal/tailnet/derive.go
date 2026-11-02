package tailnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// DefaultPort is the port `amux direct enable --tailnet` listens on unless
// --port says otherwise.
const DefaultPort = 8443

// Node is what amux derives from this node's tailnet identity.
type Node struct {
	DNSName    string     // MagicDNS name, without the trailing dot
	IP         netip.Addr // the Tailscale address to bind: IPv4 when there is one
	Tailnet    string     // tailnet name, display only
	ControlURL string     // control server; empty or hosted means Tailscale's
	CertNames  []string   // names the control server will issue certificates for
}

// Listen is the bind address for port.
func (n Node) Listen(port int) string {
	return net.JoinHostPort(n.IP.String(), strconv.Itoa(port))
}

// Endpoint is the https origin clients dial for port (the default 443 is
// omitted, as CanonicalOrigin would).
func (n Node) Endpoint(port int) string {
	if port == 443 {
		return "https://" + n.DNSName
	}
	return "https://" + n.DNSName + ":" + strconv.Itoa(port)
}

// Hosted reports whether the node is logged in to Tailscale's hosted control
// plane. Anything else (Headscale or another implementation) is reported as a
// custom control server, never as Tailscale.
func (n Node) Hosted() bool { return IsHostedControl(n.ControlURL) }

// CanIssue reports whether the control server offers a certificate for the
// node's own MagicDNS name.
func (n Node) CanIssue() bool { return slices.Contains(n.CertNames, n.DNSName) }

// Problem is a preflight failure with the one thing to do about it.
type Problem struct {
	Kind string // see the Problem* kinds
	Msg  string
	Fix  string
	Err  error
}

// Problem kinds. They separate where the fault lies: the local daemon or its
// socket (network), the node's login state (auth), the tailnet's
// configuration (identity), and certificate permission or availability.
const (
	ProblemUnreachable = "tailscaled-unreachable"
	// ProblemUnsupportedTransport: on macOS the default socket is absent,
	// which is what the Tailscale app (LocalAPI over a loopback port and
	// token) looks like; amux cannot use that transport.
	ProblemUnsupportedTransport = "unsupported-localapi-transport"
	ProblemSocketAccess         = "socket-access"
	ProblemLoggedOut            = "logged-out"
	ProblemNotRunning           = "not-running"
	ProblemMagicDNS             = "magicdns-disabled"
	ProblemNoAddress            = "no-tailscale-address"
	ProblemHTTPS                = "https-disabled"
	ProblemCertName             = "cert-name-unavailable"
	ProblemCertDenied           = "cert-denied"
	ProblemRateLimited          = "rate-limited"
	ProblemPortInUse            = "port-in-use"
	ProblemOther                = "other"
)

func (p *Problem) Error() string {
	if p.Fix == "" {
		return p.Msg
	}
	return p.Msg + "\n  fix: " + p.Fix
}

func (p *Problem) Unwrap() error { return p.Err }

// Discover reads this node's identity and checks it can carry a direct
// listener. It does not ask for a certificate (see CertProblem) and changes
// nothing.
func Discover(ctx context.Context, c *Client) (Node, error) {
	st, err := c.Status(ctx)
	if err != nil {
		return Node{}, classify(err, c.Socket)
	}
	var n Node
	// prefs is readable by anyone who can read status; when it cannot be read
	// the control server is reported as unknown rather than guessed.
	if p, err := c.Prefs(ctx); err == nil {
		n.ControlURL = p.ControlURL
	} else {
		n.ControlURL = "unknown"
	}
	switch st.BackendState {
	case "Running":
	case "NeedsLogin":
		return n, &Problem{Kind: ProblemLoggedOut, Msg: "this machine is logged out of its tailnet", Fix: "tailscale up (or tailscale login), then retry"}
	case "NeedsMachineAuth":
		return n, &Problem{Kind: ProblemLoggedOut, Msg: "this machine is waiting for a tailnet admin to approve it", Fix: "approve the machine in the tailnet's admin console, then retry"}
	case "Stopped":
		return n, &Problem{Kind: ProblemNotRunning, Msg: "Tailscale is stopped on this machine", Fix: "tailscale up, then retry"}
	default:
		return n, &Problem{Kind: ProblemNotRunning, Msg: fmt.Sprintf("tailscaled is not connected yet (state %q)", st.BackendState), Fix: "wait for Tailscale to connect (tailscale status), then retry"}
	}
	if st.CurrentTailnet != nil {
		n.Tailnet = st.CurrentTailnet.Name
	}
	n.CertNames = st.CertDomains
	if st.Self != nil {
		n.DNSName = strings.TrimSuffix(st.Self.DNSName, ".")
		n.IP = pickIP(st.Self.TailscaleIPs)
	}
	if n.DNSName == "" || (st.CurrentTailnet != nil && !st.CurrentTailnet.MagicDNSEnabled) {
		fix := "enable MagicDNS for the tailnet (" + AdminDNSURL + "), then retry"
		if !n.Hosted() {
			fix = "enable MagicDNS on your control server (" + n.ControlURL + "), or pass --listen, --endpoint, --tls-cert and --tls-key yourself"
		}
		return n, &Problem{Kind: ProblemMagicDNS, Msg: "this node has no MagicDNS name", Fix: fix}
	}
	if !n.IP.IsValid() {
		return n, &Problem{Kind: ProblemNoAddress, Msg: "this node has no Tailscale address yet", Fix: "check tailscale status, then retry"}
	}
	return n, nil
}

// pickIP prefers the IPv4 Tailscale address (100.64.0.0/10), then IPv6.
func pickIP(ips []netip.Addr) netip.Addr {
	for _, ip := range ips {
		if ip.Is4() {
			return ip
		}
	}
	for _, ip := range ips {
		if ip.IsValid() {
			return ip
		}
	}
	return netip.Addr{}
}

// CertProblem says why n cannot get a certificate from the control server, or
// nil when it can. It does not contact anything.
func CertProblem(n Node) *Problem {
	override := "or pass --tls-cert and --tls-key with a certificate for " + n.DNSName
	if n.CanIssue() {
		return nil
	}
	if len(n.CertNames) == 0 {
		if n.Hosted() {
			return &Problem{Kind: ProblemHTTPS, Msg: "HTTPS certificates are not enabled for this tailnet",
				Fix: "in the admin console's DNS page (" + AdminDNSURL + ") enable HTTPS Certificates, " + override}
		}
		return &Problem{Kind: ProblemHTTPS, Msg: "the control server (" + n.ControlURL + ") offers this node no certificate names; custom control servers such as Headscale may not issue them",
			Fix: override[3:]}
	}
	return &Problem{Kind: ProblemCertName, Msg: fmt.Sprintf("the control server offers certificates for %s, not %s", strings.Join(n.CertNames, ", "), n.DNSName), Fix: override[3:]}
}

// classify turns a LocalAPI error into a Problem.
func classify(err error, socket string) error {
	var api *APIError
	switch {
	case errors.Is(err, ErrUnreachable) && goos == "darwin" && socket == DefaultSocket():
		// Most likely the Tailscale app, which is running fine but serves
		// LocalAPI on a loopback port with a token — not a socket amux can use.
		return &Problem{Kind: ProblemUnsupportedTransport, Err: err,
			Msg: "no tailscaled socket at " + socket + ". The macOS Tailscale app (App Store or standalone) serves its LocalAPI over a loopback port with a token, which amux does not support, so --tailnet cannot derive this machine's name and address",
			Fix: "configure the listener explicitly without --tailnet: amux direct enable --listen <tailscale-ip>:8443 --endpoint https://<magicdns-name>:8443 --tls-cert <pem> --tls-key <pem> (values from tailscale status and your certificate); or run the open-source tailscaled, or point --tailscale-socket at its socket"}
	case errors.Is(err, ErrUnreachable):
		return &Problem{Kind: ProblemUnreachable, Err: err, Msg: "tailscaled is not reachable at " + socket,
			Fix: "start Tailscale (tailscale status should answer), or point --tailscale-socket at its socket"}
	case errorsIsPermission(err):
		return &Problem{Kind: ProblemSocketAccess, Err: err, Msg: "this user may not open tailscaled's socket " + socket,
			Fix: "run as a user that can use the tailscale CLI without sudo (amux does not change the socket's permissions)"}
	case errors.As(err, &api) && errors.Is(err, ErrDenied):
		return &Problem{Kind: ProblemSocketAccess, Err: err, Msg: "tailscaled refused to show its status to this user", Fix: "sudo tailscale set --operator=$USER"}
	}
	return &Problem{Kind: ProblemOther, Err: err, Msg: err.Error()}
}

// CertError turns a certificate request's error into a Problem.
func CertError(err error, n Node) error {
	var api *APIError
	switch {
	case errors.Is(err, ErrDenied):
		return &Problem{Kind: ProblemCertDenied, Err: err, Msg: "tailscaled does not let this user fetch certificates",
			Fix: "sudo tailscale set --operator=$USER (lets this user operate tailscaled), or pass --tls-cert and --tls-key"}
	case errors.Is(err, ErrRateLimited):
		fix := "retry later"
		if errors.As(err, &api) && api.RetryAfter != "" {
			fix = "retry after " + api.RetryAfter
		}
		return &Problem{Kind: ProblemRateLimited, Err: err, Msg: "the certificate authority is rate limiting " + n.DNSName, Fix: fix}
	case errors.Is(err, ErrHTTPSDisabled):
		if p := CertProblem(Node{DNSName: n.DNSName, ControlURL: n.ControlURL}); p != nil {
			p.Err = err
			return p
		}
	case errors.Is(err, ErrUnreachable):
		return classify(err, "the tailscaled socket")
	}
	return &Problem{Kind: ProblemOther, Err: err, Msg: "obtain a certificate for " + n.DNSName + ": " + err.Error(), Fix: "retry, or pass --tls-cert and --tls-key"}
}

func errorsIsPermission(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && strings.Contains(strings.ToLower(op.Err.Error()), "permission denied")
}

// PortProblem probes whether addr is free to bind, by binding it and closing
// at once. inUseOK is called on EADDRINUSE to ask whether the holder is the
// managed listener itself (re-running enable while it runs is fine).
func PortProblem(addr string, inUseOK func() bool) error {
	ln, err := net.Listen("tcp", addr)
	if err == nil {
		return ln.Close()
	}
	if strings.Contains(err.Error(), "address already in use") {
		if inUseOK != nil && inUseOK() {
			return nil
		}
		return &Problem{Kind: ProblemPortInUse, Err: err, Msg: addr + " is already in use by another program", Fix: "free it, or choose another port with --port"}
	}
	return &Problem{Kind: ProblemOther, Err: err, Msg: "cannot bind " + addr + ": " + err.Error()}
}
