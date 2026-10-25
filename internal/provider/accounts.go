package provider

import (
	"context"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// AccountChannel is scoped to one authenticated provider connection. Close must
// retire all pending attempts, including requests whose reply was lost.
type AccountChannel interface {
	Call(context.Context, hp.AccountRequest, bool) (hp.AccountResult, error)
	Close() error
}

func (p *Provider) handleAccount(s *session, m hp.MuxMsg) {
	ephemeral := m.Type == hp.MAccountCeremony
	typ := hp.HAccountResult
	if ephemeral {
		typ = hp.HAccountCeremony
	}
	result := hp.AccountResult{Error: "account_access_denied"}
	if m.Account != nil && m.ReqID != "" && len(m.ReqID) <= 128 && hp.ValidateAccountRequest(*m.Account, ephemeral) {
		r := *m.Account
		allowed := p.cfg.AccountManage
		if r.Operation == hp.AccountStatus {
			allowed = p.cfg.AccountStatus
		}
		if ephemeral && r.Initiator == "" {
			allowed = false
		}
		if allowed && p.cfg.OpenAccounts != nil {
			if s.accountChannel == nil {
				s.accountChannel, _ = p.cfg.OpenAccounts(s.rtCtx)
			}
			if s.accountChannel != nil {
				var err error
				result, err = s.accountChannel.Call(s.rtCtx, r, ephemeral)
				if err != nil {
					result = hp.AccountResult{Error: "account_service_unavailable"}
				}
				// Defense in depth: safe transport can never carry a ceremony.
				if !ephemeral {
					result.Ceremony = nil
				}
			}
		}
	} else {
		result.Error = "account_request_invalid"
	}
	// A direct connection write: never replayed, queued to durable storage or
	// published in inventory. The relay routes ReqID only to its initiating client.
	if s.rtCtx.Err() == nil {
		_ = s.hc.WriteHarness(hp.HarnessMsg{Type: typ, ReqID: m.ReqID, Account: &result})
	}
}
