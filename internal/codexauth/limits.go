package codexauth

import (
	"context"
	"encoding/json"
	"errors"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"sort"
)

// RateLimits reads native subscription windows, never inferring them from token
// usage. Missing/unsupported vendor fields remain explicitly unknown.
func (p *Provider) RateLimits(ctx context.Context) (hp.AccountLimits, error) {
	out := hp.AccountLimits{State: "unavailable", Source: "codex-native"}
	if err := p.acquire(ctx); err != nil {
		return out, err
	}
	defer p.release()
	srv, err := p.serverLocked(ctx)
	if err != nil {
		return out, err
	}
	raw, err := srv.call(ctx, "account/rateLimits/read", map[string]any{})
	if err != nil {
		return out, errors.New("native_limits_unavailable")
	}
	return decodeLimits(raw, p.cfg.Now().UnixMilli())
}
func decodeLimits(raw []byte, now int64) (hp.AccountLimits, error) {
	out := hp.AccountLimits{State: "unknown", Source: "codex-native", ObservedAt: now}
	type window struct {
		Used    *float64 `json:"usedPercent"`
		Minutes int64    `json:"windowDurationMins"`
		Reset   int64    `json:"resetsAt"`
	}
	type bucket struct {
		ID        string  `json:"limitId"`
		Primary   *window `json:"primary"`
		Secondary *window `json:"secondary"`
	}
	var res struct {
		Limits  *bucket           `json:"rateLimits"`
		Buckets map[string]bucket `json:"rateLimitsByLimitId"`
	}
	if json.Unmarshal(raw, &res) != nil {
		return out, errors.New("native_limits_invalid")
	}
	buckets := res.Buckets
	if len(buckets) == 0 && res.Limits != nil {
		buckets = map[string]bucket{res.Limits.ID: *res.Limits}
	}
	keys := []string{}
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		b := buckets[key]
		for _, entry := range []struct {
			name string
			w    *window
		}{{"primary", b.Primary}, {"secondary", b.Secondary}} {
			w := entry.w
			if w == nil || w.Used == nil {
				continue
			}
			if *w.Used < 0 || *w.Used > 100 || w.Minutes < 0 || w.Reset < 0 {
				return out, errors.New("native_limits_invalid")
			}
			name := entry.name
			if key != "" {
				name = key + ":" + name
			}
			out.Windows = append(out.Windows, hp.AccountLimitWindow{Name: name, UsedPercent: *w.Used, WindowMinutes: w.Minutes, ResetsAt: w.Reset})
		}
	}
	if len(out.Windows) > 0 {
		out.State = "known"
	}
	return out, nil
}
