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

	"amux/internal/core"
	"amux/internal/wiretls"
)

// Listen opens a listener for a spec: "unix:/path", "tcp:host:port",
// "tls:host:port" (TLS over TCP, cert/key from AMUX_TLS_* — see wiretls), or a
// bare "host:port" (assumed TCP). A unix socket is removed first if stale.
func Listen(spec string) (net.Listener, error) {
	network, addr := "unix", spec
	switch {
	case strings.HasPrefix(spec, "unix:"):
		network, addr = "unix", strings.TrimPrefix(spec, "unix:")
	case strings.HasPrefix(spec, "tls:"):
		// TLS wraps a TCP listener; message framing above the seam is unchanged.
		network, addr = "tls", trimScheme(spec, "tls:")
	case strings.HasPrefix(spec, "tcp:"):
		network, addr = "tcp", strings.TrimPrefix(spec, "tcp:")
	case strings.Contains(spec, ":") && !strings.Contains(spec, "/"):
		network, addr = "tcp", spec
	}
	if network == "tls" {
		cfg, err := wiretls.ServerConfigFromEnv()
		if err != nil {
			return nil, err
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		return tls.NewListener(ln, cfg), nil
	}
	if network == "unix" {
		_ = os.Remove(addr)
	}
	return net.Listen(network, addr)
}

// trimScheme strips a "scheme:" prefix and any leading "//" so both "tls:addr"
// and "tls://addr" resolve to the same address.
func trimScheme(spec, scheme string) string {
	return strings.TrimPrefix(strings.TrimPrefix(spec, scheme), "//")
}

// Run starts a server listening on the local unix socket plus any extra listen
// specs (e.g. "tcp:0.0.0.0:7077" for remote access), until interrupted.
func Run(extra ...string) error {
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
	return New().Serve(ctx, lns...)
}
