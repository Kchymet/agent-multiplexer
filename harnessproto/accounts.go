package harnessproto

// AccountsV1 is independent of session/compute grants. Account status is safe
// to cache; ceremony messages MUST NOT enter durable queues, logs, broadcasts,
// replay buffers, or session events. Ceremony types remain for compatibility
// and local amux CLI/TUI use; providers reject remote ceremonies and mutations.
// Local ceremonies belong to their authenticated initiating connection.
// Disconnect cancels that connection's pending ceremonies.
const (
	AccountsV1                   = "accounts-v1"
	MAccountRequest              = "account-request"
	HAccountResult               = "account-result"
	MAccountCeremony             = "account-ceremony"
	HAccountCeremony             = "account-ceremony"
	AccountStatus                = "status"
	AccountDefault               = "default"
	AccountLogin                 = "login"
	AccountCancel                = "cancel"
	AccountLogout                = "logout"
	AccountComplete              = "complete"
	AccountPoll                  = "poll"
	AccountDisconnect            = "disconnect"
	AccountRebind                = "rebind"
	AccountLimitsTTLMillis int64 = 5 * 60 * 1000
)

// AccountRequest uses profile revisions for mutation CAS. Status takes no
// profile/revision. Login/cancel/complete travel ONLY in MAccountCeremony.
// Initiator is supplied by the authenticated relay, never browser form input.
type AccountRequest struct {
	DefaultRevision uint64 `json:"defaultRevision,omitempty"` // required only for default; snapshot.defaultRevisions[harness]
	WorkloadID      string `json:"workloadId,omitempty"`
	Operation       string `json:"operation"`
	Mode            string `json:"mode,omitempty"` // login only: subscription | api; empty preserves subscription compatibility
	ProfileID       string `json:"profileId,omitempty"`
	Revision        uint64 `json:"revision,omitempty"`
	AttemptID       string `json:"attemptId,omitempty"`
	Initiator       string `json:"initiator,omitempty"`
	// Secret is accepted only by complete, for protected token/API-key input.
	Secret string `json:"secret,omitempty"`
}

// AccountSnapshot exposes only the amux-owned central configuration per harness.
// Defaults may reference a preserved legacy source until an explicit local change;
// such references are compatibility metadata, not selectable configurations.
type AccountSnapshot struct {
	DefaultRevisions map[string]uint64 `json:"defaultRevisions"`
	Version          int               `json:"version"`
	Profiles         []AccountProfile  `json:"profiles"`
	Defaults         map[string]string `json:"defaults"`
}

type AccountProfile struct {
	SupportedModes []string        `json:"supportedModes,omitempty"` // explicit local API-login capability; absence means unsupported
	ID             string          `json:"id"`
	Mode           string          `json:"mode,omitempty"` // subscription | api; empty legacy value means subscription
	Harness        string          `json:"harness"`
	Source         string          `json:"source"`
	Revision       uint64          `json:"revision"`
	DeliveryEpoch  uint64          `json:"deliveryEpoch"`
	State          string          `json:"state"` // unconfigured | login-pending | ready | blocked | logged-out
	Identity       AccountIdentity `json:"identity"`
	Limits         AccountLimits   `json:"limits"`
	LoginMethod    string          `json:"loginMethod"`     // browser | setup-token | external
	Error          string          `json:"error,omitempty"` // fixed code; never native error text
}

type AccountIdentity struct {
	State     string `json:"state"` // known | unknown | unavailable
	AccountID string `json:"accountId,omitempty"`
	Email     string `json:"email,omitempty"`
	Plan      string `json:"plan,omitempty"`
	Evidence  string `json:"evidence"` // native | opaque-token | external
}

type AccountLimits struct {
	State      string `json:"state"`                // known | unknown | unavailable
	Source     string `json:"source"`               // codex-native | claude-usage | claude-statusline | unsupported
	ObservedAt int64  `json:"observedAt,omitempty"` // Unix milliseconds
	// AttemptedAt and RefreshError describe the latest refresh. A failed
	// refresh keeps the previous windows and ObservedAt so clients can show the
	// last-known budget with its age; RefreshError is a fixed code.
	AttemptedAt  int64  `json:"attemptedAt,omitempty"` // Unix milliseconds
	RefreshError string `json:"refreshError,omitempty"`
	// RetryAt is when the provider allows the next refresh after it asked
	// amux to pause (for example a rate-limited read). Unix milliseconds.
	RetryAt           int64                `json:"retryAt,omitempty"`
	ProfileID         string               `json:"profileId,omitempty"`
	DeliveryEpoch     uint64               `json:"deliveryEpoch,omitempty"`
	SessionID         string               `json:"sessionId,omitempty"`
	RuntimeGeneration string               `json:"runtimeGeneration,omitempty"`
	Windows           []AccountLimitWindow `json:"windows,omitempty"`
	// ResetsAvailable is how many rate-limit resets the provider says the
	// account can still redeem (Codex rate-limit reset credits, Claude reset
	// grants). Absent means the provider did not report a count, not zero.
	ResetsAvailable *int64 `json:"resetsAvailable,omitempty"`
}

type AccountLimitWindow struct {
	Name          string  `json:"name"`
	UsedPercent   float64 `json:"usedPercent"`
	WindowMinutes int64   `json:"windowMinutes,omitempty"`
	ResetsAt      int64   `json:"resetsAt,omitempty"` // Unix seconds
}

// Ceremony states: pending | succeeded | failed | cancelled. ExpiresAt is Unix milliseconds.
// AccountCeremony is ephemeral and initiator-only. Poll using the returned
// attemptId and the same initiator; status snapshots never include these fields.
type AccountCeremony struct {
	Mode      string `json:"mode,omitempty"` // requested mode, confirmed by the daemon
	AttemptID string `json:"attemptId"`
	State     string `json:"state"`
	Method    string `json:"method"`
	AuthURL   string `json:"authUrl,omitempty"`
	ExpiresAt int64  `json:"expiresAt"` // Unix milliseconds
}

type AccountResult struct {
	OK       bool             `json:"ok"`
	Error    string           `json:"error,omitempty"`
	Snapshot *AccountSnapshot `json:"snapshot,omitempty"`
	Ceremony *AccountCeremony `json:"ceremony,omitempty"`
}

// ValidateAccountRequest checks syntax and the safe/ephemeral split; it grants
// no authority. Providers additionally permit only remote status requests.
// A safe status request cannot be used to poll or complete a ceremony.
func ValidateAccountRequest(r AccountRequest, ceremony bool) bool {
	if r.Mode != "" && (r.Operation != AccountLogin || (r.Mode != "subscription" && r.Mode != "api")) {
		return false
	}
	if (r.Operation == AccountDefault) != (r.DefaultRevision > 0) {
		return false
	}
	if len(r.ProfileID) > 128 || len(r.AttemptID) > 128 || len(r.Initiator) > 512 || len(r.Secret) > 4096 || len(r.WorkloadID) > 128 {
		return false
	}
	switch r.Operation {
	case AccountStatus:
		return !ceremony && r.ProfileID == "" && r.Revision == 0 && r.AttemptID == "" && r.Initiator == "" && r.Secret == "" && r.WorkloadID == ""
	case AccountDefault, AccountLogout, AccountRebind:
		return !ceremony && r.ProfileID != "" && r.Revision > 0 && r.AttemptID == "" && r.Initiator == "" && r.Secret == "" && ((r.Operation == AccountRebind) == (r.WorkloadID != ""))
	case AccountLogin:
		return ceremony && r.ProfileID != "" && r.Revision > 0 && r.AttemptID == "" && r.Secret == "" && r.WorkloadID == ""
	case AccountPoll, AccountCancel, AccountComplete:
		return ceremony && r.ProfileID != "" && r.AttemptID != "" && r.Revision == 0 && r.WorkloadID == "" && ((r.Operation == AccountComplete) == (r.Secret != ""))
	case AccountDisconnect:
		return ceremony && r.ProfileID == "" && r.AttemptID == "" && r.Revision == 0 && r.Secret == "" && r.WorkloadID == ""
	}
	return false
}

// AccountCapabilities are explicit independent host grants. Manage is retained
// for wire compatibility; amux providers always advertise false. Configuration
// belongs to the local amux CLI/TUI. Advertising a
// feature name alone grants nothing. Use controls workload creation/start.
type AccountCapabilities struct {
	Status bool `json:"status"`
	Manage bool `json:"manage"`
	Use    bool `json:"use"`
}

// AccountBinding is the workload's persisted routing selection. It carries no
// upstream account identifier, credential version, email, or ceremony material.
type AccountBinding struct {
	ProfileID string `json:"profileId"`
	Source    string `json:"source"`
}

// ManagedOnly removes legacy source choices from product account projections.
// Keep actual defaults and their CAS revisions: projecting a central default
// that has not been selected would misrepresent routing for new workloads.
// Callers retain their full internal snapshot for readiness and migration.
func (s AccountSnapshot) ManagedOnly() AccountSnapshot {
	out := s
	out.Profiles = []AccountProfile{}
	for _, harness := range []string{"claude", "codex"} {
		for _, p := range s.Profiles {
			if p.Harness == harness && p.ID == harness+":central" && p.Source == "central" {
				out.Profiles = append(out.Profiles, p)
				break
			}
		}
	}
	return out
}
