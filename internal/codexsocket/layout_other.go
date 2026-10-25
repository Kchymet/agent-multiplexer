//go:build !linux

package codexsocket

import (
	"context"
	"net"
)

// PrepareNamespaceMount is Linux-only. Other platforms keep their existing
// transport policy (including macOS's exact Seatbelt grants).
func PrepareNamespaceMount(string) (source, target string, err error) { return "", "", nil }

// DialContext leaves non-Linux endpoint handling unchanged.
func DialContext(ctx context.Context, rendezvous string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", rendezvous)
}
