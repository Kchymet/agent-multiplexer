package direct

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Cloud-assisted enrollment. A cloud application (Harness) brokers discovery
// and setup: an authenticated owner asks it for an enrollment code, it relays a
// typed request through the host's provider connection, and this authority
// mints a short-lived code bound to the client's RFC 7636 S256 challenge. The
// client redeems it directly with its verifier. The cloud never sees the
// verifier, a token or session traffic.
//
// Issuance is off until the host operator enables it with an explicit
// permission and resource ceiling and explicit pins for the Harness owner and
// provider. Enabling it means trusting the cloud to initiate grants up to that
// ceiling: PKCE does not stop a compromised but authorized issuer from
// requesting codes for a verifier it holds itself. Every cloud grant records its
// provenance (ClientInfo.Origin / CloudRequester).

// OriginCloud marks a cloud-issued code and the grant redeemed from it.
const OriginCloud = "cloud"

// Cloud issuance bounds.
const (
	CloudCodeTTL                   = 5 * time.Minute
	DefaultCloudOutstanding        = 3
	MaxCloudOutstanding            = 20
	DefaultCloudPerHour            = 10
	MaxCloudPerHour                = 60
	MaxCloudIssueKeyLen            = 128
	MaxCloudPrincipalLen           = 128
	CodeChallengeMethodS256        = "S256"
	cloudIssueRetention            = 24 * time.Hour // tombstones refuse a same-key remint this long
	maxCloudIssues                 = 4096           // bound on retained issuance records (fail closed)
	cloudRateWindow                = time.Hour
	minVerifierLen, maxVerifierLen = 43, 128
)

// Cloud issuance refusal codes (AdminResponse.Code, and the relay result's
// error). Stable wire vocabulary.
const (
	CloudErrDisabled         = "cloud_disabled"
	CloudErrOwnerMismatch    = "owner_mismatch"
	CloudErrProviderMismatch = "provider_mismatch"
	CloudErrInvalidRequest   = "invalid_request"
	CloudErrInvalidChallenge = "invalid_challenge"
	CloudErrScope            = "scope_exceeds_ceiling"
	CloudErrIdemConflict     = "idem_conflict"
	CloudErrIssueExpired     = "issue_expired"
	CloudErrCapacity         = "capacity_exhausted"
	CloudErrRateLimited      = "rate_limited"
	CloudErrUnavailable      = "unavailable"
)

// CloudError is a typed issuance refusal.
type CloudError struct {
	Code string
	Msg  string
}

func (e *CloudError) Error() string { return e.Code + ": " + e.Msg }

func cloudErr(code, msg string) error { return &CloudError{Code: code, Msg: msg} }

// CloudPolicyRequest enables (or re-pins) cloud issuance. Every field is the
// host operator's explicit choice; nothing is derived from a relayed request.
type CloudPolicyRequest struct {
	Permissions    []string      `json:"permissions"`
	Resource       ResourceScope `json:"resource"`
	OwnerPin       string        `json:"owner_pin"`
	ProviderPin    string        `json:"provider_pin"`
	MaxOutstanding int           `json:"max_outstanding,omitempty"`
	MaxPerHour     int           `json:"max_per_hour,omitempty"`
}

// CloudStatus is the operator's view of the cloud policy.
type CloudStatus struct {
	Enabled        bool          `json:"enabled"`
	Generation     uint64        `json:"generation"`
	Permissions    []string      `json:"permissions,omitempty"`
	Resource       ResourceScope `json:"resource"`
	OwnerPin       string        `json:"owner_pin,omitempty"`
	ProviderPin    string        `json:"provider_pin,omitempty"`
	MaxOutstanding int           `json:"max_outstanding,omitempty"`
	MaxPerHour     int           `json:"max_per_hour,omitempty"`
	Outstanding    int           `json:"outstanding"`
	IssuedLastHour int           `json:"issued_last_hour"`
	ChangedAt      int64         `json:"changed_at,omitempty"`
	DisableReason  string        `json:"disable_reason,omitempty"`
	// LastAttempt is the latest relayed issuance request that reached this
	// direct server (nil: none since AttemptsSince, when this server started),
	// and Attempts counts them. They let an operator tell "Harness never
	// reached this host" from "it reached this host and was refused".
	// Memory only, so a refused request still writes nothing.
	LastAttempt   *CloudAttempt `json:"last_attempt,omitempty"`
	Attempts      int           `json:"attempts"`
	AttemptsSince int64         `json:"attempts_since,omitempty"`
}

// CloudAttempt is one relayed issuance request as this host saw it: when, of
// which kind, and how it ended. It deliberately carries no requester, issue
// key, challenge, label or code.
type CloudAttempt struct {
	At      int64  `json:"at"`
	Kind    string `json:"kind"`             // CloudKindCode | CloudKindDeviceTicket | CloudKindOther
	Outcome string `json:"outcome"`          // CloudOutcomeIssued | CloudOutcomeRefused
	Code    string `json:"code,omitempty"`   // the refusal code (CloudErr*) when refused
	Reason  string `json:"reason,omitempty"` // CloudReason(Code) when refused
}

// CloudReason maps an issuance refusal code to its host_refused reason.
func CloudReason(code string) string {
	switch code {
	case CloudErrScope:
		return ReasonCeiling
	case CloudErrIssueExpired:
		return ReasonTicketExpired
	}
	return ReasonOther
}

// CloudAttempt kinds and outcomes.
const (
	CloudKindCode         = "pkce_code"
	CloudKindDeviceTicket = "device_ticket"
	CloudKindOther        = "other"
	CloudOutcomeIssued    = "issued"
	CloudOutcomeRefused   = "refused"
)

func (c CloudAttempt) String() string {
	at := time.Unix(c.At, 0).Format(time.RFC3339)
	if c.Outcome == CloudOutcomeIssued {
		return fmt.Sprintf("%s issued (%s)", at, c.Kind)
	}
	return fmt.Sprintf("%s refused: %s (%s)", at, c.Code, c.Kind)
}

// EnrollAttempt is one enrollment exchange a client made directly with this
// server: claiming a device ticket, or redeeming an enrollment code at the
// token endpoint. With the tailnet design this is the request that proves a
// client reached the host. It carries no code, verifier, challenge, label,
// token or client address.
type EnrollAttempt struct {
	At         int64  `json:"at"`
	Step       string `json:"step"`                  // EnrollStepClaim | EnrollStepRedeem
	ClientKind string `json:"client_kind,omitempty"` // web | android | other (redeem only)
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`  // the OAuth error code when refused
	Reason     string `json:"reason,omitempty"` // Reason* when refused
}

// Enrollment exchange steps.
const (
	EnrollStepClaim  = "claim"
	EnrollStepRedeem = "redeem"
)

func (e EnrollAttempt) String() string {
	at := time.Unix(e.At, 0).Format(time.RFC3339)
	step := e.Step
	if e.ClientKind != "" {
		step += " (" + e.ClientKind + ")"
	}
	if e.OK {
		return fmt.Sprintf("%s %s succeeded", at, step)
	}
	return fmt.Sprintf("%s %s refused: %s, reason %s", at, step, e.Error, e.Reason)
}

// EnrollActivity is the operator's view of enrollment exchanges since Since
// (when this server started): the latest one and how many there were.
type EnrollActivity struct {
	Last  *EnrollAttempt `json:"last,omitempty"`
	Count int            `json:"count"`
	Since int64          `json:"since"`
}

// clientKindClass reduces a client-supplied client_kind to a fixed set
// before it is recorded or logged.
func clientKindClass(kind string) string {
	switch kind {
	case ClientWeb, ClientAndroid:
		return kind
	}
	return "other"
}

func (a *Authority) noteEnroll(at EnrollAttempt) EnrollAttempt {
	a.mu.Lock()
	defer a.mu.Unlock()
	at.At = a.now().Unix()
	a.enrollLast = &at
	a.enrollCount++
	return at
}

// EnrollActivity reports enrollment exchanges since this server started.
func (a *Authority) EnrollActivity() EnrollActivity {
	a.mu.Lock()
	defer a.mu.Unlock()
	act := EnrollActivity{Count: a.enrollCount, Since: a.enrollSince}
	if a.enrollLast != nil {
		last := *a.enrollLast
		act.Last = &last
	}
	return act
}

// noteCloudAttempt records the outcome of one relayed issuance request (the
// admin answer resp to req) and returns it. Known refusal codes are kept as
// they are; anything else is reported as unavailable, like the relay does.
func (a *Authority) noteCloudAttempt(req *CloudIssueRequest, resp AdminResponse) CloudAttempt {
	at := CloudAttempt{Kind: CloudKindOther, Outcome: CloudOutcomeRefused}
	if req != nil {
		switch req.Handoff {
		case "":
			at.Kind = CloudKindCode
		case HandoffDeviceTicket:
			at.Kind = CloudKindDeviceTicket
		}
	}
	if resp.OK && resp.Bundle != "" {
		at.Outcome = CloudOutcomeIssued
	} else if _, known := relayCodes[resp.Code]; known {
		at.Code = resp.Code
	} else {
		at.Code = CloudErrUnavailable
	}
	if at.Outcome == CloudOutcomeRefused {
		at.Reason = CloudReason(at.Code)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	at.At = a.now().Unix()
	a.cloudLast = &at
	a.cloudAttempts++
	return at
}

// CloudIssueRequest is one relayed issuance. Requester is the authenticated
// Harness user the cloud vouches for; ProviderID is filled by the local
// provider from its own registration, never from the relayed frame.
type CloudIssueRequest struct {
	IssueKey            string        `json:"issue_key"`
	Requester           string        `json:"requester"`
	ProviderID          string        `json:"provider_id"`
	CodeChallenge       string        `json:"code_challenge"`
	CodeChallengeMethod string        `json:"code_challenge_method"`
	Permissions         []string      `json:"permissions"`
	Resource            ResourceScope `json:"resource"`
	Label               string        `json:"label"`
	// Handoff is empty for a PKCE-bound code, or HandoffDeviceTicket for a
	// device ticket (minted with no challenge; see ClaimTicket).
	Handoff string `json:"handoff,omitempty"`
}

// HandoffDeviceTicket is CloudIssueRequest.Handoff for a device ticket
// (harnessproto.DirectHandoffDeviceTicket).
const HandoffDeviceTicket = "device_ticket"

// requestError is a malformed token request (OAuth invalid_request). It never
// consumes the code.
type requestError struct{ msg string }

func (e *requestError) Error() string { return e.msg }

func validPrincipal(s string) bool {
	if s == "" || len(s) > MaxCloudPrincipalLen {
		return false
	}
	for _, r := range s {
		if r <= 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// ValidVerifier reports whether v is an RFC 7636 §4.1 code_verifier.
func ValidVerifier(v string) bool {
	if len(v) < minVerifierLen || len(v) > maxVerifierLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~') {
			return false
		}
	}
	return true
}

// S256Challenge is the RFC 7636 S256 code_challenge of a verifier.
func S256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func normalizePolicy(req CloudPolicyRequest) (*cloudPolicy, error) {
	if len(req.Permissions) == 0 {
		// The default ceiling: read, steer and the execute set (approve).
		req.Permissions = slices.Clone(DefaultPermissions)
	}
	perms, err := ValidatePerms(req.Permissions)
	if err != nil {
		return nil, err
	}
	if req.Resource.Kind == "" && len(req.Resource.Workgroups) == 0 {
		// The default ceiling is every ordinary session; the console is in
		// no scope (ValidateResource refuses it by id).
		req.Resource = ResourceScope{Kind: ResourceAllSessions}
	}
	res, err := ValidateResource(req.Resource)
	if err != nil {
		return nil, err
	}
	if !validPrincipal(req.OwnerPin) {
		return nil, errors.New("the Harness owner pin is required (1-128 printable bytes)")
	}
	if !validPrincipal(req.ProviderPin) {
		return nil, errors.New("the Harness provider pin is required (1-128 printable bytes)")
	}
	pick := func(v, def, max int, name string) (int, error) {
		switch {
		case v == 0:
			return def, nil
		case v < 0 || v > max:
			return 0, fmt.Errorf("%s must be between 1 and %d", name, max)
		}
		return v, nil
	}
	out, err := pick(req.MaxOutstanding, DefaultCloudOutstanding, MaxCloudOutstanding, "max outstanding")
	if err != nil {
		return nil, err
	}
	hour, err := pick(req.MaxPerHour, DefaultCloudPerHour, MaxCloudPerHour, "max per hour")
	if err != nil {
		return nil, err
	}
	return &cloudPolicy{Enabled: true, Perms: perms, Resource: res, OwnerPin: req.OwnerPin, ProviderPin: req.ProviderPin,
		MaxOutstanding: out, MaxPerHour: hour}, nil
}

// EnableCloud sets the cloud policy. Every call, including one repeating the
// current settings, starts a new generation: outstanding cloud codes from an
// earlier generation stop being redeemable. Existing grants are unaffected.
func (a *Authority) EnableCloud(req CloudPolicyRequest) (CloudStatus, error) {
	next, err := normalizePolicy(req)
	if err != nil {
		return CloudStatus{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.issuableLocked(); err != nil {
		return CloudStatus{}, err
	}
	now := a.now().Unix()
	prev := a.state.Cloud
	next.Generation = a.cloudGenLocked() + 1
	next.ChangedAt = now
	if prev != nil {
		next.Issued = slices.Clone(prev.Issued) // the hourly rate spans re-pins
	}
	a.state.Cloud = next
	if err := a.commitLocked(); err != nil {
		if rolledBack(err) {
			a.state.Cloud = prev
		}
		return CloudStatus{}, err
	}
	return a.cloudStatusLocked(now), nil
}

// DisableCloud turns cloud issuance off and invalidates every outstanding cloud
// code. Grants already redeemed stay valid until revoked explicitly. A failed
// write still leaves issuance off in this process.
func (a *Authority) DisableCloud(reason string) (CloudStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now().Unix()
	a.disableCloudLocked(reason, now)
	if err := a.commitLocked(); err != nil {
		return CloudStatus{}, fmt.Errorf("cloud issuance is off in this running process but the change was NOT persisted (%w); repair the store's storage and disable again before restarting", err)
	}
	return a.cloudStatusLocked(now), nil
}

// CloudStatus reports the cloud policy.
func (a *Authority) CloudStatus() CloudStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cloudStatusLocked(a.now().Unix())
}

func (a *Authority) cloudGenLocked() uint64 {
	if a.state.Cloud == nil {
		return 0
	}
	return a.state.Cloud.Generation
}

// disableCloudLocked turns issuance off under a new generation and consumes
// every outstanding cloud code with its replay material. A no-op when the
// policy was never enabled.
func (a *Authority) disableCloudLocked(reason string, now int64) {
	c := a.state.Cloud
	if c == nil {
		return
	}
	if c.Enabled {
		c.Enabled = false
		c.Generation++
		c.ChangedAt = now
		c.DisableReason = reason
	}
	for _, rec := range a.state.Codes {
		if rec.Origin == OriginCloud && rec.ConsumedAt == 0 {
			rec.ConsumedAt = now
		}
		if rec.Origin == OriginCloud {
			a.takeReplayLocked(rec)
		}
	}
}

func (a *Authority) cloudStatusLocked(now int64) CloudStatus {
	c := a.state.Cloud
	if c == nil {
		st := CloudStatus{}
		a.cloudAttemptsLocked(&st)
		return st
	}
	st := CloudStatus{
		Enabled: c.Enabled, Generation: c.Generation, Permissions: slices.Clone(c.Perms), Resource: c.Resource,
		OwnerPin: c.OwnerPin, ProviderPin: c.ProviderPin, MaxOutstanding: c.MaxOutstanding, MaxPerHour: c.MaxPerHour,
		ChangedAt: c.ChangedAt, DisableReason: c.DisableReason,
		Outstanding: a.cloudOutstandingLocked(now), IssuedLastHour: issuedSince(c.Issued, now),
	}
	if !c.Enabled {
		st.Outstanding = 0
	}
	a.cloudAttemptsLocked(&st)
	return st
}

func (a *Authority) cloudAttemptsLocked(st *CloudStatus) {
	st.Attempts, st.AttemptsSince = a.cloudAttempts, a.cloudSince
	if a.cloudLast != nil {
		last := *a.cloudLast
		st.LastAttempt = &last
	}
}

func issuedSince(issued []int64, now int64) int {
	n := 0
	for _, t := range issued {
		if t > now-int64(cloudRateWindow/time.Second) {
			n++
		}
	}
	return n
}

func (a *Authority) cloudOutstandingLocked(now int64) int {
	n := 0
	for _, rec := range a.state.Codes {
		if rec.Origin == OriginCloud && rec.ConsumedAt == 0 && now < rec.ExpiresAt && rec.PolicyGen == a.cloudGenLocked() {
			n++
		}
	}
	return n
}

// cloudReqHash binds an issuance key to its exact request.
func cloudReqHash(req CloudIssueRequest) string {
	b, _ := json.Marshal(struct {
		R, P, C, M, L string
		Perms         []string
		Res           ResourceScope
	}{req.Requester, req.ProviderID, req.CodeChallenge, req.CodeChallengeMethod, req.Label, req.Permissions, req.Resource})
	if req.Handoff != "" {
		// Only a handoff request extends the hashed tuple, so the hashes of
		// requests stored before handoff existed are unchanged.
		b = append(b, "\x00handoff="+req.Handoff...)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// withinCeiling reports whether a validated resource scope lies inside the
// validated ceiling. An all_sessions ceiling admits any valid ordinary scope (the
// console is never valid); a workgroups ceiling admits only a subset of its
// workgroups.
func withinCeiling(req, ceiling ResourceScope) bool {
	switch ceiling.Kind {
	case ResourceAllSessions:
		return true
	case ResourceWorkgroups:
		if req.Kind != ResourceWorkgroups {
			return false
		}
		for _, w := range req.Workgroups {
			if !slices.Contains(ceiling.Workgroups, w) {
				return false
			}
		}
		return true
	}
	return false
}

// IssueCloudCode mints one PKCE-bound enrollment code for a relayed request and
// returns it with its expiry. Every check runs under the authority lock before
// anything is written, so a refused request writes nothing:
//
//  1. the authority can issue; 2. cloud issuance is enabled; 3. the requester is
//     the pinned owner; 4. the provider is the pinned provider; 5. the challenge
//     is S256 over 32 bytes; 6. permissions and 7. resource lie within the
//     ceiling; 8. the issue key is new, or replays its own unexpired code; 9. the
//     outstanding and hourly caps hold.
//
// A same-key, same-request retry returns the stored code (no second code); a
// same-key different request is idem_conflict; a same-key retry after the code
// was consumed or expired is issue_expired. An ambiguous commit consumes the
// code in memory and returns an error, exactly like NewCode.
func (a *Authority) IssueCloudCode(req CloudIssueRequest) (string, int64, error) {
	if len(req.IssueKey) == 0 || len(req.IssueKey) > MaxCloudIssueKeyLen || !validPrincipal(req.IssueKey) {
		return "", 0, cloudErr(CloudErrInvalidRequest, "issue key must be 1-128 printable bytes")
	}
	if !validPrincipal(req.Requester) || !validPrincipal(req.ProviderID) {
		return "", 0, cloudErr(CloudErrInvalidRequest, "requester and provider are required")
	}
	if req.Label == "" {
		req.Label = "cloud enrollment"
	}
	if err := validLabel(req.Label); err != nil {
		return "", 0, cloudErr(CloudErrInvalidRequest, err.Error())
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", 0, err
	}
	code := base64.RawURLEncoding.EncodeToString(b)

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.issuableLocked(); err != nil {
		return "", 0, cloudErr(CloudErrUnavailable, err.Error())
	}
	c := a.state.Cloud
	if c == nil || !c.Enabled {
		return "", 0, cloudErr(CloudErrDisabled, "cloud enrollment is not enabled on this host")
	}
	if subtle.ConstantTimeCompare([]byte(req.Requester), []byte(c.OwnerPin)) != 1 {
		return "", 0, cloudErr(CloudErrOwnerMismatch, "the requester is not the owner this host pinned")
	}
	if subtle.ConstantTimeCompare([]byte(req.ProviderID), []byte(c.ProviderPin)) != 1 {
		return "", 0, cloudErr(CloudErrProviderMismatch, "the provider is not the one this host pinned")
	}
	ticket := false
	switch req.Handoff {
	case "":
		if req.CodeChallengeMethod != CodeChallengeMethodS256 || !validNonce(req.CodeChallenge) {
			return "", 0, cloudErr(CloudErrInvalidChallenge, "code_challenge must be S256: base64url of a 32-byte SHA-256")
		}
	case HandoffDeviceTicket:
		// The receiving device supplies its own challenge at the host. A ticket
		// request carrying one is malformed, never silently a PKCE code.
		if req.CodeChallenge != "" || req.CodeChallengeMethod != "" {
			return "", 0, cloudErr(CloudErrInvalidChallenge, "a device ticket is minted without a code_challenge")
		}
		ticket = true
	default:
		return "", 0, cloudErr(CloudErrInvalidRequest, "unknown handoff")
	}
	if len(req.Permissions) == 0 {
		// A request that names no permissions asks for the default
		// (DefaultPermissions: read, steer and the execute set), limited to
		// what this host's ceiling allows: read always, steer and approve when
		// the ceiling has them. manage, create and accounts are never implied.
		req.Permissions = []string{PermRead}
		for _, p := range DefaultPermissions {
			if p != PermRead && slices.Contains(c.Perms, p) {
				req.Permissions = append(req.Permissions, p)
			}
		}
	}
	perms, err := ValidatePerms(req.Permissions)
	if err != nil {
		return "", 0, cloudErr(CloudErrInvalidRequest, err.Error())
	}
	for _, p := range perms {
		if !slices.Contains(c.Perms, p) {
			return "", 0, cloudErr(CloudErrScope, "permissions exceed this host's cloud ceiling")
		}
	}
	if req.Resource.Kind == "" && len(req.Resource.Workgroups) == 0 {
		// A request that names no resource asks for the host's ceiling, which
		// defaults to every ordinary session and never includes the console.
		req.Resource = ResourceScope{Kind: c.Resource.Kind, Workgroups: slices.Clone(c.Resource.Workgroups)}
	}
	res, err := ValidateResource(req.Resource)
	if err != nil {
		return "", 0, cloudErr(CloudErrInvalidRequest, err.Error())
	}
	if !withinCeiling(res, c.Resource) {
		return "", 0, cloudErr(CloudErrScope, "resource scope exceeds this host's cloud ceiling")
	}
	req.Permissions, req.Resource = perms, res
	now := a.now().Unix()
	keyHash := hashToken(req.IssueKey)
	reqHash := cloudReqHash(req)
	if is := a.state.CloudIssues[keyHash]; is != nil {
		if subtle.ConstantTimeCompare([]byte(is.ReqHash), []byte(reqHash)) != 1 {
			return "", 0, cloudErr(CloudErrIdemConflict, "this issue key was used for a different request")
		}
		rec := a.state.Codes[is.CodeHash]
		if is.Code == "" || rec == nil || rec.ConsumedAt != 0 || now >= rec.ExpiresAt || rec.PolicyGen != c.Generation {
			return "", 0, cloudErr(CloudErrIssueExpired, "the code for this issue key was used or expired; start a new enrollment")
		}
		return is.Code, rec.ExpiresAt, nil
	}
	if a.cloudOutstandingLocked(now) >= c.MaxOutstanding {
		return "", 0, cloudErr(CloudErrCapacity, "too many outstanding cloud enrollment codes; redeem or wait for one to expire")
	}
	if issuedSince(c.Issued, now) >= c.MaxPerHour {
		return "", 0, cloudErr(CloudErrRateLimited, "the hourly cloud enrollment limit is reached")
	}
	a.pruneLocked(now)
	if len(a.state.CloudIssues) >= maxCloudIssues {
		return "", 0, cloudErr(CloudErrCapacity, "the cloud issuance history is full")
	}
	exp := now + int64(CloudCodeTTL/time.Second)
	codeHash := hashToken(code)
	prevIssued := c.Issued
	a.state.Codes[codeHash] = &codeRecord{
		Label: req.Label, Perms: perms, Resource: res, CreatedAt: now, ExpiresAt: exp,
		Origin: OriginCloud, Challenge: req.CodeChallenge, PolicyGen: c.Generation,
		Requester: req.Requester, ProviderID: req.ProviderID, IssueKey: keyHash, Ticket: ticket,
	}
	a.state.CloudIssues[keyHash] = &cloudIssue{ReqHash: reqHash, CodeHash: codeHash, Code: code, CreatedAt: now, ExpiresAt: exp}
	c.Issued = append(slices.Clone(c.Issued), now)
	if err := a.commitLocked(); err != nil {
		if rolledBack(err) {
			delete(a.state.Codes, codeHash)
			delete(a.state.CloudIssues, keyHash)
			c.Issued = prevIssued
		} else {
			// Never hand out a code whose record may be lost; consume it and
			// drop its replay material so this process cannot replay it either.
			a.state.Codes[codeHash].ConsumedAt = now
			a.state.CloudIssues[keyHash].Code = ""
		}
		return "", 0, cloudErr(CloudErrUnavailable, err.Error())
	}
	return code, exp, nil
}

// checkVerifierLocked enforces PKCE at redemption, and the policy generation
// for cloud codes. A device ticket (cloud or local) always needs a claim and
// the claiming device's verifier. Request errors leave the code usable; a
// wrong verifier or a stale policy consumes it (committed best-effort; it stays
// consumed in this process).
func (a *Authority) checkVerifierLocked(key string, rec *codeRecord, verifier string, now int64) error {
	if rec.Origin != OriginCloud && !rec.Ticket {
		if verifier != "" {
			return &requestError{"code_verifier is only valid for cloud-issued codes and device tickets"}
		}
		return nil
	}
	if rec.Origin != OriginCloud && rec.Origin != "" {
		// Not a shape any binary mints: refuse rather than guess its rules.
		return errInvalidCode
	}
	if rec.Ticket && rec.Challenge == "" {
		// Unclaimed: the device must bind its own challenge first. Leaves the
		// ticket usable for that device.
		return &requestError{"this device ticket must be claimed with a code_challenge before it is redeemed"}
	}
	if verifier == "" {
		return &requestError{"code_verifier is required for this code"}
	}
	if !ValidVerifier(verifier) {
		return &requestError{"code_verifier must be 43-128 characters of [A-Za-z0-9-._~]"}
	}
	ok := rec.Challenge != "" && subtle.ConstantTimeCompare([]byte(S256Challenge(verifier)), []byte(rec.Challenge)) == 1
	reason := ReasonOther
	if rec.Origin == OriginCloud {
		c := a.state.Cloud
		if c == nil || !c.Enabled || c.Generation != rec.PolicyGen {
			ok, reason = false, ReasonTicketExpired
		}
	}
	if ok {
		return nil
	}
	rec.ConsumedAt = now
	a.takeReplayLocked(rec)
	_ = a.commitLocked()
	return invalidCode(reason)
}

// takeReplayLocked clears a cloud code's replay material and returns it.
func (a *Authority) takeReplayLocked(rec *codeRecord) string {
	if rec.IssueKey == "" {
		return ""
	}
	is := a.state.CloudIssues[rec.IssueKey]
	if is == nil {
		return ""
	}
	code := is.Code
	is.Code = ""
	return code
}

func (a *Authority) restoreReplayLocked(rec *codeRecord, code string) {
	if is := a.state.CloudIssues[rec.IssueKey]; is != nil && code != "" {
		is.Code = code
	}
}

// pruneCloudLocked drops tombstones past retention and hourly samples past the
// window. A tombstone is kept while its code record exists.
func (a *Authority) pruneCloudLocked(now int64) {
	for k, is := range a.state.CloudIssues {
		if now >= is.ExpiresAt {
			is.Code = ""
		}
		if now > is.ExpiresAt+int64(cloudIssueRetention/time.Second) {
			if _, live := a.state.Codes[is.CodeHash]; !live {
				delete(a.state.CloudIssues, k)
			}
		}
	}
	if c := a.state.Cloud; c != nil {
		kept := c.Issued[:0:0]
		for _, t := range c.Issued {
			if t > now-int64(cloudRateWindow/time.Second) {
				kept = append(kept, t)
			}
		}
		c.Issued = kept
	}
}

// ClaimTicket binds a device ticket to the redeeming device's own RFC 7636
// S256 challenge, at the host, before redemption. It is the only way a
// ticket's challenge is ever set, and it sets it exactly once:
//
//   - the code must be a live device ticket, not consumed and not expired:
//     either cloud-issued as a ticket under the current enabled cloud policy,
//     or minted locally by the host operator (NewLocalTicket), which no cloud
//     policy governs;
//   - an unclaimed ticket takes challenge and is committed durably before
//     success is reported (a failed commit leaves it unclaimed);
//   - a ticket already claimed with the SAME challenge succeeds again, so a
//     device that lost the answer can retry; any other challenge is refused
//     with errInvalidCode and changes nothing.
//
// After a claim, redemption needs the claiming device's verifier: whoever else
// saw the ticket (the issuing browser, the orchestrator, a QR photo) can no
// longer redeem it. Before a claim, the ticket is registration authority: one
// new grant at its minted scope, for whoever claims it first.
func (a *Authority) ClaimTicket(code, challenge, method string) (int64, error) {
	if code == "" {
		return 0, &requestError{"code is required"}
	}
	if method != CodeChallengeMethodS256 || !validNonce(challenge) {
		return 0, &requestError{"code_challenge must be S256: base64url of a 32-byte SHA-256"}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.issuableLocked(); err != nil {
		return 0, err
	}
	now := a.now().Unix()
	rec := a.state.Codes[hashToken(code)]
	if rec == nil || !rec.Ticket {
		return 0, invalidCode(ReasonOther)
	}
	if rec.ConsumedAt != 0 || now >= rec.ExpiresAt {
		return 0, invalidCode(codeStateReason(rec, now))
	}
	switch rec.Origin {
	case OriginCloud:
		// A cloud ticket lives only as long as the policy it was issued under.
		if c := a.state.Cloud; c == nil || !c.Enabled || c.Generation != rec.PolicyGen {
			return 0, invalidCode(ReasonTicketExpired)
		}
	case "":
		// A local ticket (NewLocalTicket) was minted by the host operator
		// directly; no cloud policy governs it.
	default:
		return 0, errInvalidCode
	}
	if rec.Challenge != "" {
		if subtle.ConstantTimeCompare([]byte(rec.Challenge), []byte(challenge)) == 1 {
			return rec.ExpiresAt, nil
		}
		return 0, invalidCode(ReasonTicketUsed) // claimed by another device
	}
	rec.Challenge = challenge
	if err := a.commitLocked(); err != nil {
		if rolledBack(err) {
			rec.Challenge = ""
		}
		// Not rolled back: the claim may be durable; the device retries with the
		// same challenge, which then succeeds.
		return 0, err
	}
	return rec.ExpiresAt, nil
}
