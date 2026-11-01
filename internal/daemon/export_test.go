package daemon

import (
	"context"
	"net"
	"time"

	"amux/internal/access"
)

// ServeHostForTest serves the daemon's real host listener (TLS pin, issuer
// challenge, SubjectHost-only proof) on ln with authority, for external tests
// of callers such as RelayHostProof.
func ServeHostForTest(ctx context.Context, authority *access.FileAuthority, ln net.Listener) {
	d := New("", nil, time.Hour)
	d.authority = authority
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go d.serve(ctx, conn)
	}
}
