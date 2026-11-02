package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

const revokeTimeout = 15 * time.Second

// Revoke revokes only the registration identified by cfg.Token. It uses a
// standalone TLS exchange, never registers, advertises capabilities, serves
// requests, or writes the running provider's status. A lost acknowledgment is
// ambiguous: the caller may retry with the same token, including after revoke.
// Success means durable broker revocation and teardown on the receiving broker;
// other replicas close their connections through the broker's existing sweep.
func Revoke(ctx context.Context, cfg Config) error {
	if strings.TrimSpace(cfg.Token) == "" {
		return errors.New("provider revoke: empty token")
	}
	ctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	dial := cfg.Dial
	if dial == nil {
		dial = cfg.Relay().Dial
	}
	conn, err := dial(ctx)
	if err != nil {
		return fmt.Errorf("provider revoke: TLS connection failed: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	// One small acknowledgment is expected. Bound framing even if the peer
	// never sends a newline; the regular streaming codec has no size limit.
	hc := harnessproto.NewConn(revokeStream{Reader: io.LimitReader(conn, 16<<10), Writer: conn, Closer: conn})
	if err := hc.WriteHarness(harnessproto.HarnessMsg{
		Type: harnessproto.HProviderRevoke, Versions: []int{harnessproto.Version2}, Token: cfg.Token,
	}); err != nil {
		return revokeUnconfirmed(ctx)
	}
	m, err := hc.ReadMux()
	if err != nil {
		return revokeUnconfirmed(ctx)
	}
	if m.Type != harnessproto.MProviderRevoked {
		return errors.New("provider revoke: unsupported or unexpected broker response; revocation unconfirmed")
	}
	if !m.OK {
		// Never print arbitrary peer-controlled text: a server may echo the
		// credential in a rejection or malformed JSON.
		switch m.Error {
		case harnessproto.ErrBadToken, harnessproto.ErrBadVersion, "internal":
			return fmt.Errorf("provider revoke rejected: %s; revocation unconfirmed", m.Error)
		default:
			return errors.New("provider revoke: invalid broker rejection; revocation unconfirmed")
		}
	}
	if m.Version != harnessproto.Version2 || strings.TrimSpace(m.ProviderID) == "" || m.Error != "" {
		return errors.New("provider revoke: invalid acknowledgment; revocation unconfirmed")
	}
	return nil
}

func revokeUnconfirmed(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("provider revoke: revocation unconfirmed (retry with the same token): %w", err)
	}
	return errors.New("provider revoke: no valid acknowledgment (unsupported broker, disconnect, or malformed response); revocation unconfirmed; retry with the same token")
}

type revokeStream struct {
	io.Reader
	io.Writer
	io.Closer
}
