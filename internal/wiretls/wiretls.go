// Package wiretls is the TLS seam for amux's wire protocols. It wraps the raw
// net.Conn under a wire.Conn — message framing is untouched — so any transport
// (mux server <-> remote UI, or a provider dialing a remote orchestrator) can run
// over an authenticated, encrypted channel. Config follows amux's env-var
// conventions (see ServerConfig/ClientConfig); the helpers here are shared by the
// mux listener (internal/mux), the UI client (internal/muxclient), and provider
// mode.
package wiretls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"os"
)

// Env vars for TLS material, mirroring amux's other AMUX_* config knobs.
const (
	EnvCert     = "AMUX_TLS_CERT"       // server: PEM certificate (chain) file
	EnvKey      = "AMUX_TLS_KEY"        // server: PEM private key file
	EnvClientCA = "AMUX_TLS_CLIENT_CA"  // server: CA to verify client certs (enables mTLS)
	EnvCA       = "AMUX_TLS_CA"         // client: extra CA to trust on top of system roots
	EnvServer   = "AMUX_TLS_SERVERNAME" // client: SNI / verification hostname override
)

// ServerConfig builds a TLS config that presents certFile/keyFile. When
// clientCAFile is non-empty, client certificates are required and verified
// against it (mutual TLS); otherwise clients are unauthenticated at the TLS
// layer and prove themselves with a bearer token inside the channel.
func ServerConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	if certFile == "" || keyFile == "" {
		return nil, fmt.Errorf("tls: server needs both a certificate (%s) and key (%s)", EnvCert, EnvKey)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("tls: load keypair: %w", err)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
	if clientCAFile != "" {
		pool, err := poolFromFile(nil, clientCAFile)
		if err != nil {
			return nil, err
		}
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// ServerConfigFromEnv builds a ServerConfig from the AMUX_TLS_* env vars.
func ServerConfigFromEnv() (*tls.Config, error) {
	return ServerConfig(os.Getenv(EnvCert), os.Getenv(EnvKey), os.Getenv(EnvClientCA))
}

// ClientConfig builds a TLS config that verifies the server against the system
// roots plus, when caFile is non-empty, an additional private CA. A non-empty
// serverName overrides the hostname used for SNI and certificate verification
// (useful when dialing by IP or through a tunnel).
func ClientConfig(caFile, serverName string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if caFile != "" {
		sys, _ := x509.SystemCertPool() // nil is fine: poolFromFile starts a fresh pool
		pool, err := poolFromFile(sys, caFile)
		if err != nil {
			return nil, err
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// Dial opens a TLS connection to addr (host:port) verifying per ClientConfig,
// reading the optional private CA and server-name override from the environment.
func Dial(network, addr string) (net.Conn, error) {
	return DialCA(network, addr, os.Getenv(EnvCA), os.Getenv(EnvServer))
}

// DialCA is Dial with explicit CA file and server-name override, for callers
// (e.g. provider mode) that carry their own config rather than the environment.
func DialCA(network, addr, caFile, serverName string) (net.Conn, error) {
	cfg, err := ClientConfig(caFile, serverName)
	if err != nil {
		return nil, err
	}
	if cfg.ServerName == "" {
		if host, _, err := net.SplitHostPort(addr); err == nil {
			cfg.ServerName = host
		}
	}
	return tls.Dial(network, addr, cfg)
}

// poolFromFile appends the PEM certs in file to base (a fresh pool when base is
// nil), returning the extended pool.
func poolFromFile(base *x509.CertPool, file string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("tls: read CA %s: %w", file, err)
	}
	pool := base
	if pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tls: no certificates found in %s", file)
	}
	return pool, nil
}
