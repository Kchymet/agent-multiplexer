package provider

import (
	"context"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// DirectEnroller is the provider's narrow bridge to this host's direct server
// (production: direct.RelayIssuer). It can report whether cloud enrollment is
// offered and issue one PKCE-bound code; nothing else.
type DirectEnroller interface {
	Capabilities(context.Context) (*hp.DirectCapabilities, error)
	Enroll(ctx context.Context, providerID string, req hp.DirectEnrollRequest) hp.DirectEnrollResult
}

// Direct enrollment bounds. directEnrollTimeout bounds every reply; the
// orchestrator's own pending entry should wait somewhat longer.
var (
	directDiscoverTimeout = 5 * time.Second
	directEnrollTimeout   = 15 * time.Second
)

const (
	maxDirectInFlight   = 4
	maxDirectField      = 128
	maxDirectLabel      = 64
	maxDirectPerms      = 4
	maxDirectWorkgroups = 64
)

// discoverDirect refreshes the direct-enroll-v1 offer before a registration.
// Any failure (no direct server, issuance off, admin channel refused) means no
// offer: the feature is never advertised on a guess.
func (p *Provider) discoverDirect(ctx context.Context) {
	var offer *hp.DirectCapabilities
	if p.cfg.DirectEnroll != nil {
		dctx, cancel := context.WithTimeout(ctx, directDiscoverTimeout)
		caps, err := p.cfg.DirectEnroll.Capabilities(dctx)
		cancel()
		if err == nil && caps != nil && caps.Issuance && caps.ServerID != "" && len(caps.Endpoints) > 0 {
			offer = caps
		}
	}
	p.mu.Lock()
	p.direct = offer
	p.mu.Unlock()
}

func (p *Provider) directOffer() *hp.DirectCapabilities {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.direct == nil {
		return nil
	}
	c := *p.direct
	c.Endpoints = append([]string(nil), c.Endpoints...)
	return &c
}

func validDirectRequest(r *hp.DirectEnrollRequest) bool {
	if r == nil {
		return false
	}
	for _, f := range []string{r.IssueKey, r.Requester} {
		if f == "" || len(f) > maxDirectField {
			return false
		}
	}
	switch r.Handoff {
	case "":
		for _, f := range []string{r.CodeChallenge, r.CodeChallengeMethod} {
			if f == "" || len(f) > maxDirectField {
				return false
			}
		}
	case hp.DirectHandoffDeviceTicket:
		// A device ticket carries no challenge; the receiving device binds its
		// own at the host.
		if r.CodeChallenge != "" || r.CodeChallengeMethod != "" {
			return false
		}
	default:
		return false
	}
	if len(r.Label) > maxDirectLabel || len(r.Permissions) == 0 || len(r.Permissions) > maxDirectPerms ||
		len(r.Resource.Workgroups) > maxDirectWorkgroups {
		return false
	}
	for _, w := range r.Resource.Workgroups {
		if w == "" || len(w) > maxDirectField {
			return false
		}
	}
	return true
}

// handleDirectEnroll answers one direct-enroll frame with exactly one
// direct-enroll-result carrying its ReqID, within directEnrollTimeout. The
// provider id sent to the host is this connection's registered id, never a
// frame field: it is the id the orchestrator assigned when this connection
// registered. The reply is a direct connection write: never queued, replayed
// or logged.
func (p *Provider) handleDirectEnroll(s *session, m hp.MuxMsg) {
	if m.ReqID == "" || len(m.ReqID) > 128 {
		return // nothing to correlate a reply with
	}
	reply := func(r hp.DirectEnrollResult) {
		if s.rtCtx.Err() == nil {
			_ = s.hc.WriteHarness(hp.HarnessMsg{Type: hp.HDirectEnrollResult, ReqID: m.ReqID, DirectEnroll: &r})
		}
	}
	if p.cfg.DirectEnroll == nil || p.directOffer() == nil {
		reply(hp.DirectEnrollResult{Error: hp.DirectEnrollErrDisabled})
		return
	}
	if !validDirectRequest(m.DirectEnroll) {
		reply(hp.DirectEnrollResult{Error: hp.DirectEnrollErrInvalidRequest})
		return
	}
	providerID := s.providerID
	if providerID == "" {
		reply(hp.DirectEnrollResult{Error: hp.DirectEnrollErrUnavailable})
		return
	}
	if s.directInFlight.Add(1) > maxDirectInFlight {
		s.directInFlight.Add(-1)
		reply(hp.DirectEnrollResult{Error: hp.DirectEnrollErrBusy})
		return
	}
	req := *m.DirectEnroll
	go func() {
		defer s.directInFlight.Add(-1)
		ctx, cancel := context.WithTimeout(s.rtCtx, directEnrollTimeout)
		defer cancel()
		done := make(chan hp.DirectEnrollResult, 1)
		go func() { done <- p.cfg.DirectEnroll.Enroll(ctx, providerID, req) }()
		select {
		case r := <-done:
			reply(r)
		case <-ctx.Done():
			// The host may still have issued: the client retries with the same
			// issue key and receives the same bundle, never a second code.
			reply(hp.DirectEnrollResult{Error: hp.DirectEnrollErrUnavailable})
		}
	}()
}
