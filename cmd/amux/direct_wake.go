package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"amux/internal/direct"
	"amux/internal/hostbackend"
	"amux/internal/provider"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// cmdDirectWake is `amux direct wake enable|disable|status`: the host
// operator's opt-in for content-free cloud wake-ups
// (docs/direct-wake-contract.md). Pins default from this host's live provider
// registration, exactly like `amux direct cloud enable`.
func cmdDirectWake(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: amux direct wake enable [--owner <harness-user-id> --provider <provider-id>] | disable | status [--json]")
	}
	switch args[0] {
	case "enable":
		fs := flag.NewFlagSet("amux direct wake enable", flag.ContinueOnError)
		owner := fs.String("owner", "", "")
		prov := fs.String("provider", "", "")
		fs.SetOutput(io.Discard)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		st, stErr := provider.ReadStatus(provider.StatusPath())
		pins, err := resolveCloudPins(*owner, *prov, st, stErr, processAlive)
		if err != nil {
			return err
		}
		fmt.Println(pins.describe())
		if pins.Note != "" {
			fmt.Println(pins.Note)
		}
		resp, err := adminRun(direct.AdminRequest{Op: direct.OpWakeEnable, Wake: &direct.WakeAdminRequest{OwnerPin: pins.Owner, ProviderPin: pins.Provider}})
		if err != nil {
			return err
		}
		printWakeStatus(resp)
		fmt.Println("restart `amux provide` with --direct-wake (or direct-wake = true) so it advertises wake-ups")
		return nil
	case "disable":
		if len(args) != 1 {
			return errors.New("usage: amux direct wake disable")
		}
		resp, err := adminRun(direct.AdminRequest{Op: direct.OpWakeDisable})
		if err != nil {
			return err
		}
		printWakeStatus(resp)
		return nil
	case "status":
		resp, err := adminRun(direct.AdminRequest{Op: direct.OpWakeStatus})
		if err != nil {
			return err
		}
		if len(args) > 1 && args[1] == "--json" {
			st := direct.WakeStatus{}
			if resp.Wake != nil && resp.Wake.Status != nil {
				st = *resp.Wake.Status
			}
			return json.NewEncoder(os.Stdout).Encode(st)
		}
		printWakeStatus(resp)
		return nil
	}
	return fmt.Errorf("unknown wake command %q", args[0])
}

func printWakeStatus(resp direct.AdminResponse) {
	if resp.Wake == nil || resp.Wake.Status == nil || !resp.Wake.Status.Enabled {
		fmt.Println("direct wake-ups: disabled")
		return
	}
	st := resp.Wake.Status
	fmt.Printf("direct wake-ups: enabled\n  owner %s  provider %s\n  subscriptions %d (active %d)\n", st.OwnerPin, st.ProviderPin, st.Subscriptions, st.Active)
	if st.Degraded {
		fmt.Printf("  degraded: %s\n", st.Reason)
	}
}

// wakeAdapter adapts direct.WakeRelay to provider.DirectWaker.
type wakeAdapter struct{ r *direct.WakeRelay }

func (w wakeAdapter) Status(ctx context.Context) (*provider.WakeOffer, error) {
	o, err := w.r.Status(ctx)
	if err != nil || o == nil {
		return nil, err
	}
	return &provider.WakeOffer{ServerID: o.ServerID, OwnerPin: o.OwnerPin, ProviderPin: o.ProviderPin}, nil
}

func (w wakeAdapter) Lease(ctx context.Context, max int, wait time.Duration) ([]provider.WakeItem, error) {
	items, err := w.r.Lease(ctx, max, wait)
	if err != nil {
		return nil, err
	}
	out := make([]provider.WakeItem, 0, len(items))
	for _, it := range items {
		out = append(out, provider.WakeItem{ID: it.ID, Kind: it.Kind, RouteID: it.RouteID, WakeID: it.WakeID, GrantRef: it.GrantRef, Requester: it.Requester, Secret: it.Secret})
	}
	return out, nil
}

func (w wakeAdapter) Settle(ctx context.Context, results []provider.WakeSettle) error {
	out := make([]direct.WakeSettleResult, 0, len(results))
	for _, r := range results {
		out = append(out, direct.WakeSettleResult{ID: r.ID, Status: r.Status})
	}
	return w.r.Settle(ctx, out)
}

func (w wakeAdapter) Routes(ctx context.Context) ([]hp.DirectWakeRouteRef, error) {
	return w.r.Routes(ctx)
}

// wireDirectWake offers direct-wake-v1 through the host-only admin channel
// (status, lease, settle and routes only).
func wireDirectWake(cfg *provider.Config) {
	cfg.DirectWake = wakeAdapter{direct.NewWakeRelay(func(ctx context.Context, req direct.AdminRequest) (direct.AdminResponse, error) {
		cred, err := hostCredential()
		if err != nil {
			return direct.AdminResponse{}, err
		}
		root, err := directRoot()
		if err != nil {
			return direct.AdminResponse{}, err
		}
		return direct.AdminCall(ctx, root, cred, req)
	})}
}

// wireSessionIncarnation stamps runtime-events with the store incarnation.
func wireSessionIncarnation(cfg *provider.Config, h hostbackend.HostDaemon) {
	cfg.SessionIncarnation = func(ctx context.Context, sid string) (string, bool) {
		got, err := h.SessionIdentities(ctx, []string{sid})
		if err != nil {
			return "", false // transient: unknown, never "changed"
		}
		return got[sid].Incarnation, true
	}
}
