package harnessproto

// AccountsV1 is independent of session/compute grants. Account status is safe
// to cache; ceremony messages MUST NOT enter durable queues, logs, broadcasts,
// replay buffers, or session events. The orchestrator binds a ceremony to its
// authenticated initiating user and connection; possession of a machine ID is
// never authorization. Disconnect cancels that connection's pending ceremonies.
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
	ProfileID       string `json:"profileId,omitempty"`
	Revision        uint64 `json:"revision,omitempty"`
	AttemptID       string `json:"attemptId,omitempty"`
	Initiator       string `json:"initiator,omitempty"`
	// Secret is accepted only by complete, for Claude's setup-token ceremony.
	Secret string `json:"secret,omitempty"`
}

type AccountSnapshot struct {
	DefaultRevisions map[string]uint64 `json:"defaultRevisions"`
	Version          int               `json:"version"`
	Profiles         []AccountProfile  `json:"profiles"`
	Defaults         map[string]string `json:"defaults"`
}

type AccountProfile struct {
	ID            string          `json:"id"`
	Harness       string          `json:"harness"`
	Source        string          `json:"source"`
	Revision      uint64          `json:"revision"`
	DeliveryEpoch uint64          `json:"deliveryEpoch"`
	State         string          `json:"state"` // unconfigured | login-pending | ready | blocked | logged-out
	Identity      AccountIdentity `json:"identity"`
	Limits        AccountLimits   `json:"limits"`
	LoginMethod   string          `json:"loginMethod"`     // browser | setup-token | external
	Error         string          `json:"error,omitempty"` // fixed code; never native error text
}

type AccountIdentity struct {
	State     string `json:"state"` // known | unknown | unavailable
	AccountID string `json:"accountId,omitempty"`
	Email     string `json:"email,omitempty"`
	Plan      string `json:"plan,omitempty"`
	Evidence  string `json:"evidence"` // native | opaque-token | external
}

type AccountLimits struct {
	State             string               `json:"state"`                // known | unknown | unavailable
	Source            string               `json:"source"`               // codex-native | claude-statusline | unsupported
	ObservedAt        int64                `json:"observedAt,omitempty"` // Unix milliseconds
	ProfileID         string               `json:"profileId,omitempty"`
	DeliveryEpoch     uint64               `json:"deliveryEpoch,omitempty"`
	SessionID         string               `json:"sessionId,omitempty"`
	RuntimeGeneration string               `json:"runtimeGeneration,omitempty"`
	Windows           []AccountLimitWindow `json:"windows,omitempty"`
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

// ValidateAccountRequest enforces the safe/ephemeral split before any effect.
// A safe status request cannot be used to poll or complete a ceremony.
func ValidateAccountRequest(r AccountRequest, ceremony bool) bool {
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

// AccountCapabilities are explicit independent host grants. Advertising a
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
