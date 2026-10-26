package nativetui

import (
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

const accountObservationMaxAge = time.Duration(harnessproto.AccountLimitsTTLMillis) * time.Millisecond

func projectAccounts(in harnessproto.AccountSnapshot) accountSnapshot {
	out := accountSnapshot{canManage: true, defaultRevisions: in.DefaultRevisions, legacyDefaults: make(map[string]bool)}
	for _, harness := range []string{"claude", "codex"} {
		out.legacyDefaults[harness] = in.Defaults[harness] != "" && in.Defaults[harness] != harness+":central"
	}
	seen := make(map[string]bool)
	for _, p := range in.Profiles {
		if (p.Harness != "claude" && p.Harness != "codex") || p.Source != "central" || p.ID != p.Harness+":central" || seen[p.Harness] {
			continue
		}
		seen[p.Harness] = true
		row := accountProfile{
			canAPI: slices.Contains(p.SupportedModes, "api"), authMode: p.Mode, id: p.ID, harness: p.Harness, label: accountHarnessName(p.Harness), mode: p.Source, state: p.State,
			revision: p.Revision, defaultProfile: in.Defaults[p.Harness] == p.ID,
			identitySource: p.Identity.Evidence, plan: p.Identity.Plan,
			statusDetail: accountProfileProblem(p.Error),
			canLogin:     p.Source == "central" && (p.LoginMethod == "browser" || p.LoginMethod == "setup-token" || p.LoginMethod == "api-key") && p.State != "login-pending",
			canLogout:    p.Source == "central" && p.State != "logged-out" && p.State != "unconfigured",
			canDefault:   in.DefaultRevisions[p.Harness] > 0,
		}
		switch row.authMode {
		case "":
			row.authMode = "subscription"
		case "api", "subscription":
		default:
			row.authMode = "unknown"
		}
		if p.Identity.State == "known" {
			row.account = p.Identity.Email
			if row.account == "" {
				row.account = p.Identity.AccountID
			}
		}
		switch p.LoginMethod {
		case "api-key":
			row.loginHelp = "a: replace API key using protected input; e: subscription login."
		case "setup-token":
			row.loginHelp = "Enter: configure a Claude token using protected input."
		case "browser":
			row.loginHelp = "Enter: start browser login."
		default:
			row.loginHelp = "Login unavailable. Check the installed harness and refresh."
		}
		if row.authMode == "api" {
			row.account, row.plan = "", "not applicable"
			row.limits = []accountLimit{{name: "Subscription usage", state: "not applicable", source: "API mode", reason: "API billing and limits are separate from subscription quotas."}}
			out.profiles = append(out.profiles, row)
			continue
		}
		// Under a preserved legacy default, new workloads spend the host or
		// shared login's budget, so the row shows that profile's limits.
		lp := p
		if out.legacyDefaults[p.Harness] {
			for _, q := range in.Profiles {
				if q.ID == in.Defaults[p.Harness] && q.Harness == p.Harness && (q.Source == "host" || q.Source == "shared") {
					lp = q
				}
			}
		}
		limits := lp.Limits
		confidence := "unknown"
		if limits.Source == "codex-native" || limits.Source == "claude-usage" || limits.Source == "claude-probe" {
			confidence = "provider report"
		} else if limits.Source == "claude-statusline" {
			confidence = "session advisory"
		}
		base := accountLimit{name: "Account limits", state: "unavailable", source: limits.Source, confidence: confidence,
			reason: "Unknown limits do not mean unlimited usage.", refreshError: limits.RefreshError}
		if limits.RetryAt > 0 {
			base.retryAt = time.UnixMilli(limits.RetryAt)
		}
		if limits.ObservedAt > 0 {
			base.observedAt = time.UnixMilli(limits.ObservedAt)
			base.freshUntil = base.observedAt.Add(accountObservationMaxAge)
			// A provider-stated lifetime (a periodic quota probe) replaces the
			// default, bounded so a bad value cannot hide a dead refresh.
			if until := time.UnixMilli(limits.FreshUntil); limits.FreshUntil > 0 && until.After(base.observedAt) {
				base.freshUntil = minTime(until, base.observedAt.Add(time.Hour))
			}
		}
		// An opaque Claude token cannot verify account identity. Its statusline
		// observations are still useful when the daemon attributes them to the
		// bound profile, delivery epoch, session, and runtime; label them advisory.
		advisory := p.Harness == "claude" && lp.Source == "central" && limits.Source == "claude-statusline" && limits.SessionID != "" && limits.RuntimeGeneration != ""
		// The central Claude usage report is read with the profile's own stored
		// credential; its epoch fence retires it on login, logout, or mode change.
		// A host Codex login is read through the host's own Codex home.
		report := p.Harness == "claude" && (limits.Source == "claude-usage" || limits.Source == "claude-probe") || lp.Source == "host" && limits.Source == "codex-native"
		attributed := limits.ProfileID == lp.ID && limits.DeliveryEpoch == lp.DeliveryEpoch && (lp.Identity.State == "known" || advisory || report)
		if attributed && limits.ResetsAvailable != nil && *limits.ResetsAvailable >= 0 {
			resets := *limits.ResetsAvailable
			row.resets = &resets
		}
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
				if limits.Source == "claude-probe" {
					l.reason = "Read from a one-token quota probe every 15 minutes; usage is an observation, not a billing guarantee."
				}
				if lp.ID != p.ID {
					l.reason += " Read from the legacy default " + lp.ID + " login."
				}
				if advisory {
					l.reason += " Reported by session " + limits.SessionID + ", runtime " + limits.RuntimeGeneration + "; account identity remains unverified."
				}
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

// Persisted status survives reconnect. Only fixed local copy crosses into the
// view model; an unknown error must never retain native text or credentials.
func accountProfileProblem(code string) string {
	switch code {
	case "":
		return ""
	case "account_logout_cleanup_failed":
		return "Local logout cleanup failed. New credential delivery remains disabled.\nStart login to recover."
	case "account_logout_cleanup_unavailable":
		return "Local logout cleanup unavailable. New credential delivery remains disabled.\nCheck the installed harness, then start login to recover."
	case "account_login_cleanup_failed":
		return "Login rollback failed. New credential delivery remains blocked.\nStart login to recover."
	case "credential_unavailable":
		return "Stored credential unavailable. Start login to recover."
	case "native_status_unavailable", "native_unavailable", "account_native_unavailable":
		return "Native account service unavailable. Check the installed harness and refresh."
	default:
		return "Account service reported a problem. Refresh status before trying again."
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// accountRefreshProblem explains a fixed refresh code; unknown codes pass
// through sanitized, since they are fixed daemon codes rather than native text.
func accountRefreshProblem(code string) string {
	switch code {
	case "claude_usage_rate_limited":
		return "Claude usage report rate limited"
	case "claude_usage_scope":
		return "this Claude login lacks the user:profile scope the usage report needs"
	case "claude_usage_unauthorized":
		return "Claude rejected the stored credential; log in again"
	case "credential_expired":
		return "Claude login expired; it renews on Claude's next use"
	case "credential_unavailable":
		return "stored credential unavailable"
	case "claude_probe_unavailable":
		return "Claude quota probe failed"
	case "native_limits_unavailable":
		return "Codex rate limits unavailable"
	}
	return accountText(code)
}

// accountRetry is " (retry in 37m)" while a provider-requested pause lasts.
func accountRetry(l accountLimit, now time.Time) string {
	if l.retryAt.IsZero() || !now.Before(l.retryAt) {
		return ""
	}
	return " (retry in " + accountDuration(l.retryAt.Sub(now)) + ")"
}

func accountDuration(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int((d+time.Minute-1)/time.Minute))
	}
	return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

func projectAccountCeremony(in harnessproto.AccountCeremony) accountAttempt {
	a := accountAttempt{mode: in.Mode, id: in.AttemptID, state: in.State, method: in.Method}
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
