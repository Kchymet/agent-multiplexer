package tailnet

import (
	"fmt"
	"net"
	"strings"
)

// OrchestratorOrigin derives the browser origin of the configured
// orchestrator's web app from the provider's orchestrator address
// (provider.toml `orchestrator`, optionally with `server-name`). It keeps the
// host and port exactly; only the scheme is mapped:
//
//	tls:host:port, tls://host:port, host:port  -> https://host:port
//	wss://host[:port]                          -> https://host[:port]
//	ws://host[:port]                           -> http://host[:port]
//	https://… / http://…                       -> unchanged
//
// The provider always dials the orchestrator over TLS, so a bare host:port is
// https. The result still needs canonicalizing (the default port dropped)
// by the caller. It refuses rather than guesses when the address carries a
// path, or when a TLS server-name override names a different host — then the
// web app's host is not knowable from the config and --allow-origin must say
// it.
func OrchestratorOrigin(orchestrator, serverName string) (string, error) {
	a := strings.TrimSpace(orchestrator)
	if a == "" {
		return "", fmt.Errorf("no orchestrator is configured (provider.toml)")
	}
	scheme := "https"
	switch {
	case strings.HasPrefix(a, "wss://"):
		a = strings.TrimPrefix(a, "wss://")
	case strings.HasPrefix(a, "ws://"):
		scheme, a = "http", strings.TrimPrefix(a, "ws://")
	case strings.HasPrefix(a, "https://"):
		a = strings.TrimPrefix(a, "https://")
	case strings.HasPrefix(a, "http://"):
		scheme, a = "http", strings.TrimPrefix(a, "http://")
	case strings.HasPrefix(a, "tls:"):
		a = strings.TrimPrefix(strings.TrimPrefix(a, "tls:"), "//")
	}
	a = strings.TrimSuffix(a, "/")
	if a == "" || strings.ContainsAny(a, "/?#@") {
		return "", fmt.Errorf("orchestrator %q is not a host[:port] address", orchestrator)
	}
	host := a
	if h, _, err := net.SplitHostPort(a); err == nil {
		host = h
	}
	if serverName != "" && !strings.EqualFold(serverName, host) {
		return "", fmt.Errorf("orchestrator %q is dialed with server-name %q, so its web origin is ambiguous", orchestrator, serverName)
	}
	return scheme + "://" + a, nil
}
