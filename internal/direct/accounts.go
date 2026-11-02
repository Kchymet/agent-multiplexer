package direct

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"amux/internal/hostbackend"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Machine account status (read-only). This mirrors the only account operation
// the cloud relay serves remotely (provider handleAccount: a status request,
// ManagedOnly profiles, ceremony stripped). Login, logout, default, rebind and
// every ceremony stay local CLI/TUI only; nothing here can reach them.

// mayReadAccounts is the single rule for every account-bearing projection: the
// accounts route, the capabilities flag, and each inventory row's
// account_binding (list, watch snapshots and watch updates). Account metadata
// is machine-wide owner information, so it needs the explicit accounts
// permission AND an all_sessions grant; a workgroup-scoped grant never sees it.
func mayReadAccounts(g Grant) bool {
	return g.Can(PermAccounts) && g.Resource.Kind == ResourceAllSessions
}

// AccountsResponse is GET /direct/v1/accounts.
type AccountsResponse struct {
	ServerID string            `json:"server_id"`
	Defaults map[string]string `json:"defaults"`
	Accounts []Account         `json:"accounts"`
}

// Account is one allowlisted machine account profile. It carries no revision
// or CAS data, delivery epoch, ceremony, auth URL, attempt id, token or secret.
type Account struct {
	Harness        string          `json:"harness"`
	ProfileID      string          `json:"profile_id"`
	Source         string          `json:"source"`
	State          string          `json:"state"`
	Mode           string          `json:"mode,omitempty"`
	SupportedModes []string        `json:"supported_modes,omitempty"`
	LoginMethod    string          `json:"login_method,omitempty"`
	Error          string          `json:"error,omitempty"` // a fixed code, never native error text
	Identity       AccountIdentity `json:"identity"`
	Limits         AccountLimits   `json:"limits"`
}

// AccountIdentity is the profile's account identity as the daemon reports it.
// account_id stays: Codex limits are attributed to it.
type AccountIdentity struct {
	State     string `json:"state"`
	AccountID string `json:"account_id,omitempty"`
	Email     string `json:"email,omitempty"`
	Plan      string `json:"plan,omitempty"`
	Evidence  string `json:"evidence,omitempty"`
}

// AccountLimits is the profile's usage budget. Per-session attribution
// (session id, runtime generation) and the internal profile/epoch fields are
// dropped, so the server has already applied the attribution check
// (limitsAttributable): windows and resets_available appear only for an
// observation of this profile and delivery epoch with its required evidence;
// otherwise state is "unknown" with no windows. Unknown never means unlimited.
type AccountLimits struct {
	State           string               `json:"state"`
	Source          string               `json:"source,omitempty"`
	ObservedAt      int64                `json:"observed_at,omitempty"`
	AttemptedAt     int64                `json:"attempted_at,omitempty"`
	RefreshError    string               `json:"refresh_error,omitempty"`
	RetryAt         int64                `json:"retry_at,omitempty"`
	FreshUntil      int64                `json:"fresh_until,omitempty"`
	ResetsAvailable *int64               `json:"resets_available,omitempty"`
	Windows         []AccountLimitWindow `json:"windows,omitempty"`
}

// AccountLimitWindow is one usage window.
type AccountLimitWindow struct {
	Name          string  `json:"name"`
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int64   `json:"window_minutes,omitempty"`
	ResetsAt      int64   `json:"resets_at,omitempty"`
}

// SessionAccountBinding is a row's account routing (which machine profile the
// session runs under). Rows carry it only under mayReadAccounts.
type SessionAccountBinding struct {
	ProfileID string `json:"profile_id,omitempty"`
	Source    string `json:"source"`
}

// Fixed error codes the daemon sets on a profile (internal/daemon/accounts.go)
// and on a limits refresh (internal/daemon/account_limits.go). Anything else,
// which could be native or provider text, is reported as CodeAccountUnrecognized:
// the client still learns that there is an error, never its raw text.
var (
	accountErrorCodes = []string{
		"account_login_failed", "account_login_cleanup_failed", "account_native_unavailable",
		"credential_mode_mismatch", "credential_unavailable", "native_status_unavailable", "native_unavailable",
	}
	refreshErrorCodes = []string{
		"claude_usage_unavailable", "claude_usage_rate_limited", "claude_usage_scope", "claude_usage_unauthorized",
		"claude_usage_invalid", "claude_probe_unavailable", "credential_unavailable", "credential_expired",
		"native_limits_unavailable",
	}
)

// CodeAccountUnrecognized stands in for an error value outside the allowlist.
const CodeAccountUnrecognized = "unrecognized"

func fixedCode(v string, allowed []string) string {
	if v == "" || slices.Contains(allowed, v) {
		return v
	}
	return CodeAccountUnrecognized
}

// centralProfileID reports a managed central profile id (claude:central,
// codex:central): the only profile ids direct ever sends.
func centralProfileID(id string) bool { return id == "claude:central" || id == "codex:central" }

// safeWord is a short lowercase vocabulary value (sources, states, modes).
func safeWord(v string) string {
	if len(v) > 32 {
		return ""
	}
	for _, r := range v {
		if (r < 'a' || r > 'z') && r != '-' && r != '_' {
			return ""
		}
	}
	return v
}

// limitsAttributable is the cloud client's attribution rule
// (harness frontend MachineAccounts.tsx, AccountLimits), applied here because
// the DTO drops the fields it reads. A profile's limits are shown only when
// they were observed for this exact profile and delivery epoch, from the
// harness's expected source, with the evidence that source needs: a Claude
// statusline observation names its session and runtime generation; a Codex
// native read requires a known account id.
func limitsAttributable(p hp.AccountProfile) bool {
	l := p.Limits
	expected := map[string]string{"claude": "claude-statusline", "codex": "codex-native"}[p.Harness]
	if expected == "" || l.Source != expected || l.ProfileID != p.ID || l.DeliveryEpoch != p.DeliveryEpoch {
		return false
	}
	if p.Harness == "claude" && (l.SessionID == "" || l.RuntimeGeneration == "") {
		return false
	}
	if l.Source == "codex-native" && (p.Identity.State != "known" || p.Identity.AccountID == "") {
		return false
	}
	return true
}

// projectAccounts is the allowlisted view of a daemon account snapshot: only
// the managed central profiles, field by field.
func projectAccounts(serverID string, snap hp.AccountSnapshot) AccountsResponse {
	managed := snap.ManagedOnly()
	out := AccountsResponse{ServerID: serverID, Defaults: map[string]string{}, Accounts: []Account{}}
	// The daemon's actual defaults, never invented: a harness with no central
	// default simply has no entry.
	for h, id := range managed.Defaults {
		if (h == "claude" || h == "codex") && centralProfileID(id) {
			out.Defaults[h] = id
		}
	}
	for _, p := range managed.Profiles {
		if !centralProfileID(p.ID) {
			continue
		}
		var modes []string
		for _, m := range p.SupportedModes {
			if w := safeWord(m); w != "" {
				modes = append(modes, w)
			}
		}
		a := Account{
			Harness: p.Harness, ProfileID: p.ID, Source: safeWord(p.Source), State: safeWord(p.State), Mode: safeWord(p.Mode),
			SupportedModes: modes, LoginMethod: safeWord(p.LoginMethod), Error: fixedCode(p.Error, accountErrorCodes),
			Identity: AccountIdentity{State: safeWord(p.Identity.State), AccountID: p.Identity.AccountID, Email: p.Identity.Email,
				Plan: p.Identity.Plan, Evidence: safeWord(p.Identity.Evidence)},
			Limits: AccountLimits{State: safeWord(p.Limits.State), Source: safeWord(p.Limits.Source), ObservedAt: p.Limits.ObservedAt,
				AttemptedAt: p.Limits.AttemptedAt, RefreshError: fixedCode(p.Limits.RefreshError, refreshErrorCodes), RetryAt: p.Limits.RetryAt,
				FreshUntil: p.Limits.FreshUntil, ResetsAvailable: p.Limits.ResetsAvailable},
		}
		if !limitsAttributable(p) {
			// Never another profile's or epoch's numbers: unknown, no windows
			// and no resets; the source stays so a client can say why.
			a.Limits.State, a.Limits.ResetsAvailable = "unknown", nil
		} else {
			for _, w := range p.Limits.Windows {
				a.Limits.Windows = append(a.Limits.Windows, AccountLimitWindow{Name: w.Name, UsedPercent: w.UsedPercent,
					WindowMinutes: w.WindowMinutes, ResetsAt: w.ResetsAt})
			}
		}
		out.Accounts = append(out.Accounts, a)
	}
	return out
}

// accountBinding is a row's binding under the grant, or nil.
// Only a central profile id is ever sent; a binding to anything else carries
// its source alone, so the row stays truthful without naming a legacy profile.
func accountBinding(g Grant, b *hp.AccountBinding) *SessionAccountBinding {
	if b == nil || !mayReadAccounts(g) {
		return nil
	}
	out := &SessionAccountBinding{Source: safeWord(b.Source)}
	if centralProfileID(b.ProfileID) {
		out.ProfileID = b.ProfileID
	}
	return out
}

// Accounts reads the daemon's machine account status: one status request on a
// dedicated account connection, ManagedOnly, ceremony discarded.
func (b HostBackend) Accounts(ctx context.Context) (hp.AccountSnapshot, error) {
	acc, err := b.H.OpenAccounts(ctx)
	if err != nil {
		return hp.AccountSnapshot{}, err
	}
	defer acc.Close()
	res, err := acc.Call(ctx, hp.AccountRequest{Operation: hp.AccountStatus}, false)
	if err != nil {
		return hp.AccountSnapshot{}, err
	}
	if !res.OK || res.Snapshot == nil {
		return hp.AccountSnapshot{}, errAccountsUnavailable
	}
	return res.Snapshot.ManagedOnly(), nil
}

var errAccountsUnavailable = errors.New("account status unavailable")

func (s *Server) accounts(w http.ResponseWriter, r *http.Request) {
	l := s.admit(w, r, PermAccounts)
	if l == nil {
		return
	}
	defer l.Done()
	if !mayReadAccounts(l.Grant) {
		writeDenied(w, PermAccounts, "machine account status needs an all-sessions client")
		return
	}
	snap, err := s.cfg.Backend.Accounts(l.Context())
	if code := l.AuthEnded(); code != "" {
		writeErr(w, http.StatusUnauthorized, code, "access ended")
		return
	}
	if err != nil {
		if !errors.Is(err, hostbackend.ErrUnavailable) {
			s.cfg.Logf("direct: accounts: %v", err)
		}
		writeErr(w, http.StatusServiceUnavailable, CodeUnavailable, "machine account status is not available")
		return
	}
	writeJSON(w, http.StatusOK, projectAccounts(s.cfg.Authority.ServerID(), snap))
}
