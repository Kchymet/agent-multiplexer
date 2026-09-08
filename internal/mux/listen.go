package mux

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"amux/internal/core"
	"amux/internal/wiretls"
)

// Listen opens a TLS-authenticated listener for "unix:/path" or
// "tls:host:port". Even the Unix transport is wrapped in TLS: the pathname is
// only routing and must not be able to solicit the mux bearer from a client.
// Plain TCP/bare-address listeners are refused.
func Listen(spec string) (net.Listener, error) {
	network, addr := "unix", spec
	switch {
	case strings.HasPrefix(spec, "unix:"):
		network, addr = "unix", strings.TrimPrefix(spec, "unix:")
	case strings.HasPrefix(spec, "tls:"):
		// TLS wraps a TCP listener; message framing above the seam is unchanged.
		network, addr = "tls", trimScheme(spec, "tls:")
	case strings.HasPrefix(spec, "tcp:"):
		return nil, fmt.Errorf("plaintext mux transport %q is disabled; use tls:", spec)
	case strings.Contains(spec, ":") && !strings.Contains(spec, "/"):
		return nil, fmt.Errorf("plaintext mux transport %q is disabled; use tls:", spec)
	}
	cfg, err := wiretls.ServerConfigFromEnv()
	if err != nil {
		return nil, err
	}
	if network == "unix" {
		_ = os.Remove(addr)
	} else {
		network = "tcp"
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return nil, err
	}
	return tls.NewListener(ln, cfg), nil
}

// trimScheme strips a "scheme:" prefix and any leading "//" so both "tls:addr"
// and "tls://addr" resolve to the same address.
func trimScheme(spec, scheme string) string {
	return strings.TrimPrefix(strings.TrimPrefix(spec, scheme), "//")
}

// Run starts a server listening with TLS on the local Unix socket plus any extra
// TLS listen specs, until interrupted.
func Run(extra ...string) error {
	return RunWithPrimary(daemonPrimary{}, extra...)
}

// RunWithPrimary is the authority-injection seam. Production passes the
// authenticated primary-daemon relay; tests may pass an inert implementation.
func RunWithPrimary(primary Primary, extra ...string) error {
	if strings.TrimSpace(os.Getenv("AMUX_MUX_TOKEN")) == "" {
		return fmt.Errorf("legacy mux requires a nonempty AMUX_MUX_TOKEN")
	}
	if primary == nil {
		return fmt.Errorf("legacy mux requires an authenticated primary daemon relay")
	}
	preflightCtx, cancelPreflight := context.WithTimeout(context.Background(), 5*time.Second)
	initial, err := primary.Snapshot(preflightCtx)
	cancelPreflight()
	if err != nil {
		return fmt.Errorf("authenticate primary daemon relay: %w", err)
	}
	specs := append([]string{"unix:" + core.MuxSocketPath()}, extra...)
	var lns []net.Listener
	for _, spec := range specs {
		ln, err := Listen(spec)
		if err != nil {
			return fmt.Errorf("listen %s: %w", spec, err)
		}
		defer ln.Close()
		fmt.Fprintf(os.Stderr, "amux multiplexer listening on %s\n", spec)
		lns = append(lns, ln)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := New(primary)
	server.remember(initial)
	return server.Serve(ctx, lns...)
}
