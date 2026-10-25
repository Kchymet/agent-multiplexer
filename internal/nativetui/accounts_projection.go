package nativetui

import (
	"fmt"
	"math"
	"net/url"
	"strings"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

const accountObservationMaxAge = time.Duration(harnessproto.AccountLimitsTTLMillis) * time.Millisecond

func projectAccounts(in harnessproto.AccountSnapshot) accountSnapshot {
	out := accountSnapshot{canManage: true}
	for _, p := range in.Profiles {
		if p.Harness != "claude" && p.Harness != "codex" {
			continue
		}
		row := accountProfile{
			id: p.ID, harness: p.Harness, label: p.ID, mode: p.Source, state: p.State,
			revision: p.Revision, defaultProfile: in.Defaults[p.Harness] == p.ID,
			identitySource: p.Identity.Evidence, plan: p.Identity.Plan,
			canLogin:   p.Source == "central" && (p.LoginMethod == "browser" || p.LoginMethod == "setup-token") && p.State != "login-pending",
			canLogout:  p.Source == "central" && p.State != "logged-out" && p.State != "unconfigured",
			canDefault: true,
		}
		if p.Identity.State == "known" {
			row.account = p.Identity.Email
			if row.account == "" {
				row.account = p.Identity.AccountID
			}
		}
		switch p.LoginMethod {
		case "setup-token":
			row.loginHelp = "Enter: configure a Claude token using protected input."
		case "browser":
			row.loginHelp = "Enter: start browser login."
		default:
			row.loginHelp = "External source — manage login in the host tool. " + accountModeHelp(p.Source)
		}
		limits := p.Limits
		confidence := "provider report"
		if limits.Source == "claude-statusline" {
			confidence = "session advisory"
		}
		base := accountLimit{name: "Account limits", state: "unavailable", source: limits.Source, confidence: confidence,
			reason: "Unknown limits do not mean unlimited usage."}
		if limits.ObservedAt > 0 {
			base.observedAt = time.UnixMilli(limits.ObservedAt)
			base.freshUntil = base.observedAt.Add(accountObservationMaxAge)
		}
		attributed := limits.ProfileID == p.ID && limits.DeliveryEpoch == p.DeliveryEpoch && p.Identity.State == "known"
		if limits.State != "known" || len(limits.Windows) == 0 || !attributed {
			if !attributed && len(limits.Windows) > 0 {
				base.reason = "Observation is not attributable to this verified account and delivery epoch."
			}
			row.limits = []accountLimit{base}
		} else {
			for _, window := range limits.Windows {
				l := base
				l.name, l.state = window.Name, "available"
				l.reason = "Stale after 5 minutes; usage is an observation, not a billing guarantee."
				used := window.UsedPercent
				if !math.IsNaN(used) && !math.IsInf(used, 0) && used >= 0 && used <= 100 {
					l.usedPercent = &used
				} else {
					l.state, l.reason = "unavailable", "Invalid provider observation."
				}
				if window.WindowMinutes > 0 {
					l.name += fmt.Sprintf(" (%d minutes)", window.WindowMinutes)
				}
				if window.ResetsAt > 0 {
					l.resetsAt = time.Unix(window.ResetsAt, 0)
				}
				row.limits = append(row.limits, l)
			}
		}
		out.profiles = append(out.profiles, row)
	}
	return out
}

func projectAccountCeremony(in harnessproto.AccountCeremony) accountAttempt {
	a := accountAttempt{id: in.AttemptID, state: in.State, method: in.Method}
	if in.ExpiresAt > 0 {
		a.expiresAt = time.UnixMilli(in.ExpiresAt)
	}
	if a.active() && approvedAccountURL(in.AuthURL) {
		a.url = in.AuthURL
	}
	return a
}

// Defense in depth: native/browser output must not become arbitrary clickable
// URLs or terminal control sequences. The backend independently validates its
// ceremony URL before exposing it to any client.
func approvedAccountURL(raw string) bool {
	if len(raw) > 8192 || accountText(raw) != raw || strings.ContainsAny(raw, " \t\r\n") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" {
		return false
	}
	switch u.Hostname() {
	case "auth.openai.com", "login.openai.com", "claude.ai", "console.anthropic.com":
		return true
	}
	return false
}
