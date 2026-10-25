package accounts

import (
	"encoding/json"
	"errors"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"time"
)

// ClaudeWindows accepts only the documented statusline windows. These are
// session-reported observations, not an issuer-authenticated identity assertion.
func ClaudeWindows(raw []byte) ([]hp.AccountLimitWindow, error) {
	if len(raw) > 4096 {
		return nil, errors.New("limits_invalid")
	}
	type window struct {
		Used  *float64 `json:"used_percentage"`
		Reset int64    `json:"resets_at"`
	}
	var data struct {
		Five *window `json:"five_hour"`
		Week *window `json:"seven_day"`
	}
	if json.Unmarshal(raw, &data) != nil {
		return nil, errors.New("limits_invalid")
	}
	out := []hp.AccountLimitWindow{}
	for _, v := range []struct {
		name    string
		minutes int64
		w       *window
	}{{"five_hour", 300, data.Five}, {"seven_day", 10080, data.Week}} {
		if v.w == nil || v.w.Used == nil {
			continue
		}
		if *v.w.Used < 0 || *v.w.Used > 100 || v.w.Reset < 0 {
			return nil, errors.New("limits_invalid")
		}
		out = append(out, hp.AccountLimitWindow{Name: v.name, UsedPercent: *v.w.Used, ResetsAt: v.w.Reset, WindowMinutes: v.minutes})
	}
	return out, nil
}
func ObserveClaude(session, generation string, raw []byte) error {
	windows, e := ClaudeWindows(raw)
	if e != nil {
		return e
	}
	return Update(func(s *State) error {
		b, ok := s.Bindings[session]
		if !ok || b.Harness != "claude" || b.Source != "central" {
			return ErrBlocked
		}
		p := s.Profiles[b.ProfileID]
		if p.Mode == "api" || p.State != "ready" || b.DeliveredEpoch != p.DeliveryEpoch || b.CredentialVersion != s.CredentialVersion[p.ID] {
			return ErrStale
		}
		state := "unknown"
		if len(windows) > 0 {
			state = "known"
		}
		p.Limits = hp.AccountLimits{State: state, Source: "claude-statusline", ProfileID: p.ID, DeliveryEpoch: p.DeliveryEpoch, SessionID: session, RuntimeGeneration: generation, ObservedAt: time.Now().UnixMilli(), Windows: windows}
		s.Profiles[p.ID] = p
		return nil
	})
}
