package direct

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// CanonicalOrigin parses an origin and returns it as scheme://host[:port], with
// a lower-case host and the default port omitted. It refuses userinfo, a path
// (other than empty or "/"), a query or a fragment. HTTPS is required unless
// allowHTTP is set (used only for browser origins such as a local dev server).
func CanonicalOrigin(raw string, allowHTTP bool) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", fmt.Errorf("origin %q is empty or padded", raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("origin %q does not parse: %w", raw, err)
	}
	switch {
	case u.Scheme != "https" && !(allowHTTP && u.Scheme == "http"):
		return "", fmt.Errorf("origin %q must use https", raw)
	case u.User != nil:
		return "", fmt.Errorf("origin %q must not carry credentials", raw)
	case u.Path != "" && u.Path != "/", u.RawPath != "":
		return "", fmt.Errorf("origin %q must not have a path", raw)
	case u.RawQuery != "" || u.ForceQuery:
		return "", fmt.Errorf("origin %q must not have a query", raw)
	case u.Fragment != "" || strings.Contains(raw, "#"):
		return "", fmt.Errorf("origin %q must not have a fragment", raw)
	case u.Opaque != "" || u.Hostname() == "":
		return "", fmt.Errorf("origin %q has no host", raw)
	}
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if port == defaultPort(u.Scheme) {
		port = ""
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port != "" {
		return u.Scheme + "://" + host + ":" + port, nil
	}
	return u.Scheme + "://" + host, nil
}

func defaultPort(scheme string) string {
	if scheme == "http" {
		return "80"
	}
	return "443"
}

// hostKey is the Host-header form of a canonical https origin: host[:port],
// with :443 also accepted.
func hostKeys(origin string) []string {
	u, _ := url.Parse(origin)
	host := strings.ToLower(u.Host)
	if u.Port() == "" {
		return []string{host, host + ":" + defaultPort(u.Scheme)}
	}
	return []string{host}
}

// CheckBind refuses a listen address that would expose the listener beyond the
// private networks unless allowPublic is set. addr must be an IP literal and
// port; the unspecified addresses (0.0.0.0, ::) count as public because they
// bind every interface.
func CheckBind(addr string, allowPublic bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return fmt.Errorf("listen address %q must be ip:port", addr)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("listen address %q must use an IP literal, not a name", addr)
	}
	if allowPublic || PrivateAddr(ip) {
		return nil
	}
	return fmt.Errorf("listen address %s is not a private or loopback address; pass --allow-public-bind to expose it deliberately", addr)
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// PrivateAddr reports loopback, RFC 1918, IPv6 ULA, link-local, and the
// 100.64.0.0/10 shared space that tailnets use.
func PrivateAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if ip.IsUnspecified() {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}
