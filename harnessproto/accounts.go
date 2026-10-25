package harnessproto

// AccountsV1 is independent of session/compute grants. Account status is safe
// to cache; ceremony messages MUST NOT enter durable queues, logs, broadcasts,
// replay buffers, or session events. The orchestrator binds a ceremony to its
// authenticated initiating user and connection; possession of a machine ID is
// never authorization. Disconnect cancels that connection's pending ceremonies.
const (
	AccountsV1       = "accounts-v1"
	MAccountRequest  = "account-request"
	HAccountResult   = "account-result"
	MAccountCeremony = "account-ceremony"
	HAccountCeremony = "account-ceremony"
	AccountStatus    = "status"
	AccountDefault   = "default"
	AccountLogin     = "login"
	AccountCancel    = "cancel"
	AccountLogout    = "logout"
	AccountComplete  = "complete"
)

// AccountRequest uses profile revisions for mutation CAS. Status takes no
// profile/revision. Login/cancel/complete travel ONLY in MAccountCeremony.
// Initiator is supplied by the authenticated relay, never browser form input.
type AccountRequest struct {
	Operation string `json:"operation"`
	ProfileID string `json:"profileId,omitempty"`
	Revision  uint64 `json:"revision,omitempty"`
	AttemptID string `json:"attemptId,omitempty"`
	Initiator string `json:"initiator,omitempty"`
	// Secret is accepted only by complete, for Claude's setup-token ceremony.
	Secret string `json:"secret,omitempty"`
}

type AccountSnapshot struct {
	Version  int               `json:"version"`
	Profiles []AccountProfile  `json:"profiles"`
	Defaults map[string]string `json:"defaults"`
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

// AccountCeremony is ephemeral and initiator-only. Poll using the returned
// attemptId and the same initiator; status snapshots never include these fields.
type AccountCeremony struct {
	AttemptID string `json:"attemptId"`
	State     string `json:"state"`
	Method    string `json:"method"`
	AuthURL   string `json:"authUrl,omitempty"`
	ExpiresAt int64  `json:"expiresAt"`
}

type AccountResult struct {
	OK       bool             `json:"ok"`
	Error    string           `json:"error,omitempty"`
	Snapshot *AccountSnapshot `json:"snapshot,omitempty"`
	Ceremony *AccountCeremony `json:"ceremony,omitempty"`
}
