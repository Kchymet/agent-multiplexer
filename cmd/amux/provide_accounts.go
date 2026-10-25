package main

import (
	"amux/internal/daemon"
	"amux/internal/provider"
	"amux/internal/providercfg"
	"context"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

type providerAccounts struct{ c *daemon.Client }

func openProviderAccounts(ctx context.Context) (provider.AccountChannel, error) {
	c, err := daemon.DialContext(ctx)
	if err != nil {
		return nil, err
	}
	go func() { <-ctx.Done(); _ = c.Close() }()
	return &providerAccounts{c}, nil
}
func (p *providerAccounts) Call(ctx context.Context, r hp.AccountRequest, ceremony bool) (hp.AccountResult, error) {
	if err := ctx.Err(); err != nil {
		return hp.AccountResult{}, err
	}
	return p.c.Accounts(r, ceremony)
}
func (p *providerAccounts) Close() error { return p.c.Close() }

func providerAccountUseDefault(c providercfg.Config, publish, readOnly bool) bool {
	if c.AccountUseSet {
		return c.AccountUse
	}
	return c.AccountUse || publish && !readOnly
}
