package provider

import (
	"context"
	"net"

	"amux/internal/wiretls"
)

// HarnessRelay is the provider's outbound transport: one TLS connection that
// this host dials to the orchestrator broker (Harness, or any peer speaking
// harnessproto v2), over which it registers and then serves the relay
// protocol. It is a provider transport, not a client access route: clients
// reach this host's sessions through the orchestrator, and nothing here
// advertises an endpoint, a listener or a machine identity. The bearer token
// travels inside the TLS channel in the register frame, never here.
type HarnessRelay struct {
	Address    string // broker host:port; a tls: or tls:// prefix is accepted and stripped
	CAFile     string // private CA trusted on top of the system roots
	ServerName string // TLS server-name override for SNI and verification
}

// Relay returns the relay transport this configuration dials.
func (c Config) Relay() HarnessRelay {
	return HarnessRelay{Address: c.Orchestrator, CAFile: c.CAFile, ServerName: c.ServerName}
}

// Dial opens the verified TLS connection. Relay mode is TLS-only; there is no
// plaintext option. Cancelling ctx abandons the connect and handshake.
func (r HarnessRelay) Dial(ctx context.Context) (net.Conn, error) {
	return wiretls.DialCAContext(ctx, "tcp", stripScheme(r.Address), r.CAFile, r.ServerName)
}
