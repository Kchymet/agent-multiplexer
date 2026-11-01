package direct

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Lifetimes. Configuration may only shorten them.
const (
	MaxAccessTTL      = 10 * time.Minute
	DefaultIdleTTL    = 30 * 24 * time.Hour
	DefaultAbsoluteTT = 90 * 24 * time.Hour
	DefaultCodeTTL    = 10 * time.Minute
	MaxCodeTTL        = time.Hour
	idemRetention     = 24 * time.Hour
	revokedRetention  = 30 * 24 * time.Hour
	revokeDrain       = 10 * time.Second
)

// maxUsedRefresh bounds a grant's consumed-refresh history. Reaching it ends
// the grant (fail closed) instead of evicting history. A var for tests.
var maxUsedRefresh = 20000

// maxIdemPerGrant bounds a grant's idempotency records. Unresolved (admitted or
// ambiguous) records are never pruned, so a grant that accumulates this many
// fails closed for new actions. A var for tests.
var maxIdemPerGrant = 1024

// Authorization failures. Each maps to one wire code.
var (
	errAuthInvalid = errors.New(CodeAuthInvalid)
	errAuthExpired = errors.New(CodeAuthExpired)
	errAuthRevoked = errors.New(CodeAuthRevoked)
	errDenied      = errors.New(CodeDenied)
	errInvalidCode = errors.New(OAuthInvalidGrant)
)

// Lifetimes configures an Authority. Zero fields take the defaults; values
// above the defaults are refused.
type Lifetimes struct {
	Access, Idle, Absolute time.Duration
}

func (l Lifetimes) resolve() (Lifetimes, error) {
	pick := func(v, def time.Duration, name string) (time.Duration, error) {
		if v == 0 {
			return def, nil
		}
		if v < 0 || v > def {
			return 0, fmt.Errorf("%s lifetime must be between 0 and %s", name, def)
		}
		return v, nil
	}
	var err error
	if l.Access, err = pick(l.Access, MaxAccessTTL, "access"); err != nil {
		return l, err
	}
	if l.Idle, err = pick(l.Idle, DefaultIdleTTL, "idle"); err != nil {
		return l, err
	}
	if l.Absolute, err = pick(l.Absolute, DefaultAbsoluteTT, "absolute"); err != nil {
		return l, err
	}
	return l, nil
}

type accessRec struct {
	clientID string
	exp      int64 // unix seconds
}

// Authority is the single owner of the grant registry and the in-memory access
// tokens. Every admission and every revocation passes through mu, so a revoked
// grant can never admit new work after Revoke returns.
type Authority struct {
	mu       sync.Mutex
	st       *store
	state    *storeState
	identity *Identity
	life     Lifetimes
	now      func() time.Time
	boot     string

	access   map[string]accessRec // sha256(token) -> record; memory only
	tomb     map[string]int64     // revoked access-token hashes -> exp
	running  map[string]bool      // idempotency keys executing in this process
	leases   map[string]map[*Lease]struct{}
	closed   bool
	degraded error                 // set after an ambiguous commit; blocks issuance until restart
	onRevoke func(clientID string) // test hook, called after the barrier
}

// errDegraded refuses issuance after a commit whose durability is unknown.
var errDegraded = errors.New("direct store durability is uncertain after a failed sync; restart `amux direct serve` to re-read it")

// commitLocked persists the registry. On errNotCommitted the caller rolls its
// change back; on errAmbiguousCommit the caller keeps the change (it is on disk
// unless the directory entry is lost), returns failure without handing out
// anything that depends on it, and the authority refuses further issuance.
func (a *Authority) commitLocked() error {
	err := a.st.saveState(a.state)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errAmbiguousCommit):
		a.degraded = err
		return err
	case errors.Is(err, errNotCommitted):
		return err
	default:
		return fmt.Errorf("%w: %w", errNotCommitted, err)
	}
}

func rolledBack(err error) bool { return errors.Is(err, errNotCommitted) }

func (a *Authority) issuableLocked() error {
	if a.closed {
		return errors.New("authority closed")
	}
	if a.degraded != nil {
		return errDegraded
	}
	return nil
}

// OpenAuthority takes exclusive ownership of the store at root, creating the
// identity on first use.
func OpenAuthority(root string, life Lifetimes, now func() time.Time) (*Authority, error) {
	life, err := life.resolve()
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	st, err := openStore(root)
	if err != nil {
		return nil, err
	}
	a, err := loadAuthority(st, life, now)
	if err != nil {
		st.close()
		return nil, err
	}
	return a, nil
}

func loadAuthority(st *store, life Lifetimes, now func() time.Time) (*Authority, error) {
	id, err := st.loadIdentity()
	if err != nil {
		return nil, err
	}
	state, err := st.loadState()
	if err != nil {
		return nil, err
	}
	if id == nil {
		if id, err = newIdentity(); err != nil {
			return nil, err
		}
		if err := st.saveIdentity(id); err != nil {
			return nil, err
		}
	}
	boot := make([]byte, 8)
	if _, err := rand.Read(boot); err != nil {
		return nil, err
	}
	a := &Authority{
		st: st, state: state, identity: id, life: life, now: now, boot: hex.EncodeToString(boot),
		access: map[string]accessRec{}, tomb: map[string]int64{}, running: map[string]bool{}, leases: map[string]map[*Lease]struct{}{},
	}
	if state.IdentityGeneration != id.Generation {
		// A grant issued under another identity can never be valid again.
		a.revokeAllLocked("identity changed")
		state.IdentityGeneration = id.Generation
		if err := st.saveState(state); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// Close releases the store lock. Open leases are cancelled.
func (a *Authority) Close() {
	a.mu.Lock()
	a.closed = true
	var all []*Lease
	for _, set := range a.leases {
		for l := range set {
			all = append(all, l)
		}
	}
	a.mu.Unlock()
	for _, l := range all {
		l.cancel(errAuthRevoked)
	}
	a.st.close()
}

// ServerID is the identity's RFC 7638 thumbprint.
func (a *Authority) ServerID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.identity.ServerID
}

// Identity returns the current identity.
func (a *Authority) Identity() *Identity {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.identity
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func randomToken(prefix string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// ValidatePerms normalizes and checks a permission list: read is mandatory,
// steer optional, nothing else allowed.
func ValidatePerms(perms []string) ([]string, error) {
	set := map[string]bool{}
	for _, p := range perms {
		p = strings.TrimSpace(p)
		if p != PermRead && p != PermSteer {
			return nil, fmt.Errorf("unknown permission %q (allowed: read, steer)", p)
		}
		set[p] = true
	}
	if !set[PermRead] {
		return nil, errors.New("permissions must include read")
	}
	out := []string{PermRead}
	if set[PermSteer] {
		out = append(out, PermSteer)
	}
	return out, nil
}

// ValidateResource checks an explicit resource scope.
func ValidateResource(r ResourceScope) (ResourceScope, error) {
	switch r.Kind {
	case ResourceAllSessions:
		if len(r.Workgroups) != 0 {
			return r, errors.New("all_sessions takes no workgroups")
		}
		return ResourceScope{Kind: ResourceAllSessions}, nil
	case ResourceWorkgroups:
		if len(r.Workgroups) == 0 {
			return r, errors.New("workgroups scope needs at least one workgroup id")
		}
		ids := slices.Clone(r.Workgroups)
		for _, id := range ids {
			if !validSessionID(id) {
				return r, fmt.Errorf("invalid workgroup id %q", id)
			}
			if id == ConsoleID {
				return r, errors.New("the console cannot be granted over direct access")
			}
		}
		sort.Strings(ids)
		return ResourceScope{Kind: ResourceWorkgroups, Workgroups: slices.Compact(ids)}, nil
	}
	return r, errors.New("resource scope must be all_sessions or workgroups")
}

// NewCode mints a single-use enrollment code for an explicit scope.
func (a *Authority) NewCode(label string, perms []string, resource ResourceScope, ttl time.Duration) (string, int64, error) {
	if ttl == 0 {
		ttl = DefaultCodeTTL
	}
	if ttl < 0 || ttl > MaxCodeTTL {
		return "", 0, fmt.Errorf("code lifetime must be at most %s", MaxCodeTTL)
	}
	if err := validLabel(label); err != nil {
		return "", 0, err
	}
	perms, err := ValidatePerms(perms)
	if err != nil {
		return "", 0, err
	}
	resource, err = ValidateResource(resource)
	if err != nil {
		return "", 0, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", 0, err
	}
	code := base64.RawURLEncoding.EncodeToString(b)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.issuableLocked(); err != nil {
		return "", 0, err
	}
	now := a.now().Unix()
	exp := now + int64(ttl/time.Second)
	a.pruneLocked(now)
	a.state.Codes[hashToken(code)] = &codeRecord{Label: label, Perms: perms, Resource: resource, CreatedAt: now, ExpiresAt: exp}
	if err := a.commitLocked(); err != nil {
		if rolledBack(err) {
			delete(a.state.Codes, hashToken(code))
		} else {
			// Never hand out a code whose record may be lost; consume it here.
			a.state.Codes[hashToken(code)].ConsumedAt = now
		}
		return "", 0, err
	}
	return code, exp, nil
}

func validLabel(label string) error {
	if label == "" || len(label) > MaxLabelLen {
		return fmt.Errorf("label must be 1-%d bytes", MaxLabelLen)
	}
	for _, r := range label {
		if r < 0x20 || r == 0x7f {
			return errors.New("label must not contain control characters")
		}
	}
	return nil
}

// RedeemCode exchanges a code for a new grant. The code is consumed and the
// grant committed durably before the tokens are returned; a second redemption,
// concurrent or later, fails with invalid_grant.
func (a *Authority) RedeemCode(code, label, kind string) (TokenResponse, error) {
	if label == "" {
		label = "direct client"
	}
	if err := validLabel(label); err != nil {
		return TokenResponse{}, err
	}
	if kind != ClientWeb && kind != ClientAndroid {
		return TokenResponse{}, fmt.Errorf("client_kind must be %s or %s", ClientWeb, ClientAndroid)
	}
	access, err := randomToken(AccessTokenPrefix)
	if err != nil {
		return TokenResponse{}, err
	}
	refresh, err := randomToken(RefreshTokenPrefix)
	if err != nil {
		return TokenResponse{}, err
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return TokenResponse{}, err
	}
	clientID := "c_" + base64.RawURLEncoding.EncodeToString(idBytes)

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.issuableLocked(); err != nil {
		return TokenResponse{}, err
	}
	now := a.now().Unix()
	key := hashToken(code)
	rec := a.state.Codes[key]
	if rec == nil || rec.ConsumedAt != 0 || now >= rec.ExpiresAt {
		return TokenResponse{}, errInvalidCode
	}
	g := &grantRecord{
		ClientID: clientID, Label: label, ClientKind: kind, Perms: slices.Clone(rec.Perms), Resource: rec.Resource,
		IdentityGeneration: a.identity.Generation, CreatedAt: now,
		AbsoluteExpiry: now + int64(a.life.Absolute/time.Second), IdleTTL: int64(a.life.Idle / time.Second),
		LastRefreshAt: now, RefreshHash: hashToken(refresh),
	}
	rec.ConsumedAt, rec.ClientID = now, clientID
	a.state.Grants[clientID] = g
	if err := a.commitLocked(); err != nil {
		if rolledBack(err) {
			rec.ConsumedAt, rec.ClientID = 0, ""
			delete(a.state.Grants, clientID)
		} else {
			// The code stays consumed; the unreturned grant is revoked so no
			// copy of it can ever authorize anything.
			a.markRevokedLocked(g, "enrollment commit uncertain", now)
		}
		return TokenResponse{}, err
	}
	return a.issueLocked(g, access, refresh, now), nil
}

func (a *Authority) issueLocked(g *grantRecord, access, refresh string, now int64) TokenResponse {
	a.pruneLocked(now)
	exp := now + int64(a.life.Access/time.Second)
	if exp > g.AbsoluteExpiry {
		exp = g.AbsoluteExpiry
	}
	a.access[hashToken(access)] = accessRec{clientID: g.ClientID, exp: exp}
	return TokenResponse{
		AccessToken: access, TokenType: "Bearer", ExpiresIn: exp - now, RefreshToken: refresh,
		Scope: strings.Join(g.Perms, " "), ClientID: g.ClientID, ServerID: a.identity.ServerID,
		GrantExpiresAt: g.AbsoluteExpiry, ResourceScope: g.Resource,
	}
}

// Refresh rotates a refresh token. The presented token is consumed and its
// successor committed durably before the response. A consumed token presented
// again revokes the whole grant: there is no grace window.
func (a *Authority) Refresh(refresh string) (TokenResponse, error) {
	access, err := randomToken(AccessTokenPrefix)
	if err != nil {
		return TokenResponse{}, err
	}
	next, err := randomToken(RefreshTokenPrefix)
	if err != nil {
		return TokenResponse{}, err
	}
	a.mu.Lock()
	if err := a.issuableLocked(); err != nil {
		a.mu.Unlock()
		return TokenResponse{}, err
	}
	now := a.now().Unix()
	h := hashToken(refresh)
	var g *grantRecord
	reused := false
	for _, cand := range a.state.Grants {
		if cand.RefreshHash == h {
			g = cand
			break
		}
		if slices.Contains(cand.UsedRefresh, h) {
			g, reused = cand, true
			break
		}
	}
	switch {
	case g == nil:
		a.mu.Unlock()
		return TokenResponse{}, errInvalidCode
	case reused:
		return a.revokeAndUnlock(g, "refresh token reuse", now)
	case g.RevokedAt != 0 || now >= g.AbsoluteExpiry || now >= g.LastRefreshAt+g.IdleTTL || g.IdentityGeneration != a.identity.Generation:
		a.mu.Unlock()
		return TokenResponse{}, errInvalidCode
	}
	if len(g.UsedRefresh) >= maxUsedRefresh {
		// Reuse detection must cover the grant's whole life, so the history is
		// never truncated: at the bound the grant ends and the client enrolls again.
		return a.revokeAndUnlock(g, "refresh rotation limit reached", now)
	}
	prevHash, prevAt, prevLen := g.RefreshHash, g.LastRefreshAt, len(g.UsedRefresh)
	g.UsedRefresh = append(g.UsedRefresh, g.RefreshHash)
	g.RefreshHash, g.LastRefreshAt = hashToken(next), now
	if err := a.commitLocked(); err != nil {
		if rolledBack(err) {
			g.UsedRefresh = g.UsedRefresh[:prevLen]
			g.RefreshHash, g.LastRefreshAt = prevHash, prevAt
		}
		// On an ambiguous commit the old token stays consumed and the successor
		// is never returned: the client must enroll again.
		a.mu.Unlock()
		return TokenResponse{}, err
	}
	resp := a.issueLocked(g, access, next, now)
	a.mu.Unlock()
	return resp, nil
}

// revokeAndUnlock revokes g (refresh reuse or rotation limit), commits,
// releases mu, drains its leases and reports invalid_grant.
func (a *Authority) revokeAndUnlock(g *grantRecord, reason string, now int64) (TokenResponse, error) {
	leases := a.markRevokedLocked(g, reason, now)
	err := a.commitLocked()
	a.mu.Unlock()
	drained := a.drain(g.ClientID, leases)
	switch {
	case err != nil:
		return TokenResponse{}, fmt.Errorf("revocation (%s) is in effect in this process but was NOT persisted (%w); a restart before the store is repaired will revive the grant", reason, err)
	case !drained:
		return TokenResponse{}, fmt.Errorf("revocation (%s) persisted but operations did not finish within %s", reason, revokeDrain)
	}
	return TokenResponse{}, errInvalidCode
}

// RevokeToken implements RFC 7009 for a client signing itself out: revoking
// either its access or its refresh token revokes the whole grant. Unknown tokens
// are not an error.
func (a *Authority) RevokeToken(token string) error {
	h := hashToken(token)
	a.mu.Lock()
	clientID := ""
	if rec, ok := a.access[h]; ok {
		clientID = rec.clientID
	} else {
		for id, g := range a.state.Grants {
			if g.RefreshHash == h {
				clientID = id
				break
			}
		}
	}
	a.mu.Unlock()
	if clientID == "" {
		return nil
	}
	return a.Revoke(clientID, "client sign-out")
}

// Revoke durably revokes a grant, invalidates its access and refresh tokens,
// cancels its in-flight work and waits for its streams and calls to finish.
// When it returns nil no admitted operation of that grant is still running and
// none can be admitted again. An effect that was already admitted may have
// completed; nothing is rolled back.
func (a *Authority) Revoke(clientID, reason string) error {
	a.mu.Lock()
	g := a.state.Grants[clientID]
	if g == nil {
		a.mu.Unlock()
		return fmt.Errorf("no such client %q", clientID)
	}
	leases := a.markRevokedLocked(g, reason, a.now().Unix())
	err := a.commitLocked()
	a.mu.Unlock()
	drained := a.drain(clientID, leases)
	if err != nil {
		return fmt.Errorf("revocation of %s is in effect in this running process but was NOT persisted (%w); "+
			"repair the store's storage and run the revoke again before restarting, or a restart will revive the grant", clientID, err)
	}
	if !drained {
		return fmt.Errorf("revocation persisted, but operations of %s did not finish within %s", clientID, revokeDrain)
	}
	return nil
}

// markRevokedLocked flips the grant and its tokens in memory (so admission fails
// from this instant) and returns the leases to cancel.
func (a *Authority) markRevokedLocked(g *grantRecord, reason string, now int64) []*Lease {
	if g.RevokedAt == 0 {
		g.RevokedAt, g.RevokeReason = now, reason
	}
	g.RefreshHash = ""
	for h, rec := range a.access {
		if rec.clientID == g.ClientID {
			a.tomb[h] = rec.exp
			delete(a.access, h)
		}
	}
	var out []*Lease
	for l := range a.leases[g.ClientID] {
		out = append(out, l)
	}
	return out
}

func (a *Authority) drain(clientID string, leases []*Lease) bool {
	for _, l := range leases {
		l.cancel(errAuthRevoked)
	}
	deadline := time.NewTimer(revokeDrain)
	defer deadline.Stop()
	for _, l := range leases {
		select {
		case <-l.done:
		case <-deadline.C:
			return false
		}
	}
	if a.onRevoke != nil {
		a.onRevoke(clientID)
	}
	return true
}

func (a *Authority) revokeAllLocked(reason string) []*Lease {
	now := a.now().Unix()
	var all []*Lease
	for _, g := range a.state.Grants {
		all = append(all, a.markRevokedLocked(g, reason, now)...)
	}
	for _, c := range a.state.Codes {
		if c.ConsumedAt == 0 {
			c.ConsumedAt = now
		}
	}
	return all
}

// ResetIdentity replaces the server identity and revokes every grant and code.
func (a *Authority) ResetIdentity() (string, error) {
	id, err := newIdentity()
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	leases := a.revokeAllLocked("identity reset")
	// Commit the revocations before the new key: a crash in between leaves old
	// grants revoked under the old identity, and loadAuthority revokes again.
	// The in-memory revocation stands on every exit, and every exit drains.
	a.state.IdentityGeneration = id.Generation
	err = a.commitLocked()
	if err == nil {
		if err = a.st.saveIdentity(id); err == nil {
			a.identity = id
		} else if errors.Is(err, errAmbiguousCommit) {
			a.degraded = err
		}
	}
	if err != nil && a.degraded == nil {
		// The old identity stays in use and every grant stays revoked; stop
		// issuing under a registry generation that no longer matches.
		a.degraded = err
	}
	a.mu.Unlock()
	byClient := map[string][]*Lease{}
	for _, l := range leases {
		byClient[l.ClientID] = append(byClient[l.ClientID], l)
	}
	undrained := 0
	for c, ls := range byClient {
		if !a.drain(c, ls) {
			undrained++
		}
	}
	switch {
	case err != nil:
		return "", fmt.Errorf("identity reset incomplete (all grants are revoked in this process; restart to recover): %w", err)
	case undrained > 0:
		return id.ServerID, fmt.Errorf("identity reset; %d clients' operations did not finish within %s", undrained, revokeDrain)
	}
	return id.ServerID, nil
}

// ClientInfo is the operator's view of one grant (no secrets).
type ClientInfo struct {
	ClientID       string        `json:"client_id"`
	Label          string        `json:"label"`
	ClientKind     string        `json:"client_kind"`
	Permissions    []string      `json:"permissions"`
	ResourceScope  ResourceScope `json:"resource_scope"`
	CreatedAt      int64         `json:"created_at"`
	LastRefreshAt  int64         `json:"last_refresh_at"`
	GrantExpiresAt int64         `json:"grant_expires_at"`
	RevokedAt      int64         `json:"revoked_at,omitempty"`
	RevokeReason   string        `json:"revoke_reason,omitempty"`
	ActiveLeases   int           `json:"active_operations"`
}

// List returns every grant, newest first.
func (a *Authority) List() []ClientInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]ClientInfo, 0, len(a.state.Grants))
	for _, g := range a.state.Grants {
		out = append(out, ClientInfo{
			ClientID: g.ClientID, Label: g.Label, ClientKind: g.ClientKind, Permissions: slices.Clone(g.Perms),
			ResourceScope: g.Resource, CreatedAt: g.CreatedAt, LastRefreshAt: g.LastRefreshAt,
			GrantExpiresAt: g.AbsoluteExpiry, RevokedAt: g.RevokedAt, RevokeReason: g.RevokeReason,
			ActiveLeases: len(a.leases[g.ClientID]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

func (a *Authority) pruneLocked(now int64) {
	for h, c := range a.state.Codes {
		if now > c.ExpiresAt+86400 {
			delete(a.state.Codes, h)
		}
	}
	for id, g := range a.state.Grants {
		if g.RevokedAt != 0 && now > g.RevokedAt+int64(revokedRetention/time.Second) && len(a.leases[id]) == 0 {
			delete(a.state.Grants, id)
		}
	}
	for h, exp := range a.tomb {
		if now > exp {
			delete(a.tomb, h)
		}
	}
	for h, rec := range a.access {
		if now > rec.exp+3600 {
			delete(a.access, h)
		}
	}
}

// Grant is the immutable authorization view a lease carries.
type Grant struct {
	ClientID string
	Perms    []string
	Resource ResourceScope
}

// Can reports whether the grant holds perm.
func (g Grant) Can(perm string) bool { return slices.Contains(g.Perms, perm) }

// Lease is one admitted operation or stream of one grant. Revoke cancels it
// and waits for Done, which is the revoke barrier.
//
// A lease has two contexts. Context ends on revocation, on access-token expiry
// and when the request goes away: every authorization read and every stream
// runs under it. ExecContext ends only on revocation (or its own bound): an
// action that has already been admitted and handed to the daemon is not
// abandoned because the token expired or the client disconnected mid-call.
type Lease struct {
	Grant
	ExpiresAt int64 // access token expiry (unix seconds)
	ctx       context.Context
	rctx      context.Context // revocation / release only
	rcancel   context.CancelCauseFunc
	release   func()
	done      chan struct{}
	once      sync.Once
	a         *Authority
}

// Context is cancelled on revocation (cause auth.revoked), on access-token
// expiry (cause auth.expired) and with the request.
func (l *Lease) Context() context.Context { return l.ctx }

// ExecContext bounds one already-admitted daemon call: it ends on revocation or
// after limit, never on token expiry or client disconnect.
func (l *Lease) ExecContext(limit time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(l.rctx, limit)
}

func (l *Lease) cancel(cause error) { l.rcancel(cause) }

// Revoked reports whether the lease was cancelled by revocation.
func (l *Lease) Revoked() bool { return errors.Is(context.Cause(l.rctx), errAuthRevoked) }

// Expired reports whether the lease ended because its access token expired.
func (l *Lease) Expired() bool {
	return !l.Revoked() && errors.Is(context.Cause(l.ctx), errAuthExpired)
}

// AuthEnded returns the wire code when the lease ended for an auth reason.
func (l *Lease) AuthEnded() string {
	switch {
	case l.Revoked():
		return CodeAuthRevoked
	case l.Expired():
		return CodeAuthExpired
	}
	return ""
}

// Done releases the lease. It is idempotent.
func (l *Lease) Done() {
	l.once.Do(func() {
		l.a.mu.Lock()
		if set := l.a.leases[l.ClientID]; set != nil {
			delete(set, l)
			if len(set) == 0 {
				delete(l.a.leases, l.ClientID)
			}
		}
		l.a.mu.Unlock()
		l.rcancel(context.Canceled)
		l.release()
		close(l.done)
	})
}

// Admit validates an access token and permission and registers a lease, in one
// critical section with revocation. It never touches the backend.
func (a *Authority) Admit(parent context.Context, token, perm string) (*Lease, error) {
	h := hashToken(token)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil, errAuthInvalid
	}
	now := a.now().Unix()
	if !strings.HasPrefix(token, AccessTokenPrefix) {
		return nil, errAuthInvalid
	}
	if _, ok := a.tomb[h]; ok {
		return nil, errAuthRevoked
	}
	rec, ok := a.access[h]
	if !ok {
		return nil, errAuthInvalid
	}
	g := a.state.Grants[rec.clientID]
	if g == nil || g.RevokedAt != 0 || g.IdentityGeneration != a.identity.Generation {
		return nil, errAuthRevoked
	}
	if now >= rec.exp {
		return nil, errAuthExpired
	}
	if !slices.Contains(g.Perms, perm) {
		return nil, errDenied
	}
	// The lease ends at the access token's expiry: a stalled backend call is
	// cancelled then, with a cause distinct from revocation.
	rctx, rcancel := context.WithCancelCause(context.Background())
	dctx, dcancel := context.WithDeadlineCause(parent, time.Now().Add(time.Unix(rec.exp, 0).Sub(a.now())), errAuthExpired)
	ctx, ccancel := context.WithCancelCause(dctx)
	stop := context.AfterFunc(rctx, func() { ccancel(context.Cause(rctx)) })
	l := &Lease{
		Grant:     Grant{ClientID: g.ClientID, Perms: slices.Clone(g.Perms), Resource: g.Resource},
		ExpiresAt: rec.exp, ctx: ctx, rctx: rctx, rcancel: rcancel, done: make(chan struct{}), a: a,
		release: func() { stop(); ccancel(context.Canceled); dcancel() },
	}
	if a.leases[g.ClientID] == nil {
		a.leases[g.ClientID] = map[*Lease]struct{}{}
	}
	a.leases[g.ClientID][l] = struct{}{}
	return l, nil
}

// idempotency outcomes
type idemOutcome int

const (
	idemProceed idemOutcome = iota
	idemReplay
	idemConflict
	idemInProgress
	idemAmbiguous
)

// errCapacity: the grant holds too many unresolved or recent actions.
var errCapacity = errors.New("this client has too many recent or unresolved actions; enroll again to continue")

func runKey(clientID, key string) string { return clientID + "\x00" + key }

// BeginAction records an action's idempotency key durably before it executes.
// It returns idemReplay with the stored result when the same key and body
// already completed. An admitted key that is not running in this process (it
// ended without a recorded outcome, or a previous process admitted it) is
// ambiguous and is never executed again. Ambiguous and admitted records are
// never pruned; at capacity new actions fail closed.
//
// With peek set it only reports an existing record and never creates one.
func (a *Authority) BeginAction(l *Lease, key, bodyHash string, peek bool) (idemOutcome, *idemRecord, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l.rctx.Err() != nil {
		return 0, nil, errAuthRevoked
	}
	g := a.state.Grants[l.ClientID]
	if g == nil || g.RevokedAt != 0 {
		return 0, nil, errAuthRevoked
	}
	now := a.now().Unix()
	if g.Idem == nil {
		g.Idem = map[string]*idemRecord{}
	}
	for k, r := range g.Idem {
		if r.State == idemDone && now > r.At+int64(idemRetention/time.Second) {
			delete(g.Idem, k)
		}
	}
	if r := g.Idem[key]; r != nil {
		switch {
		case r.BodyHash != bodyHash:
			return idemConflict, nil, nil
		case r.State == idemDone:
			copied := *r
			return idemReplay, &copied, nil
		case r.State == idemAdmitted && a.running[runKey(l.ClientID, key)]:
			return idemInProgress, nil, nil
		default:
			return idemAmbiguous, nil, nil
		}
	}
	if peek {
		return idemProceed, nil, nil
	}
	if a.degraded != nil {
		return 0, nil, errDegraded
	}
	if len(g.Idem) >= maxIdemPerGrant {
		return 0, nil, errCapacity
	}
	g.Idem[key] = &idemRecord{BodyHash: bodyHash, State: idemAdmitted, Boot: a.boot, At: now}
	if err := a.commitLocked(); err != nil {
		if rolledBack(err) {
			delete(g.Idem, key)
		}
		// Ambiguous commit: the admitted record stays and is not running, so a
		// retry reports ambiguous and the action never runs blind.
		return 0, nil, err
	}
	a.running[runKey(l.ClientID, key)] = true
	return idemProceed, nil, nil
}

// FinishAction records a known outcome (ok, or the daemon's refusal, or a
// failure before the daemon was reached). A persistence failure leaves the
// record admitted and no longer running, which reads as ambiguous.
func (a *Authority) FinishAction(l *Lease, key string, ok bool, code, msg string) {
	a.endAction(l, key, func(r *idemRecord) { r.State, r.OK, r.Code, r.Error = idemDone, ok, code, msg })
}

// AmbiguousAction records that the call's outcome is unknown: it may or may
// not have taken effect. The record is permanent for the grant.
func (a *Authority) AmbiguousAction(l *Lease, key string) {
	a.endAction(l, key, func(r *idemRecord) { r.State = idemUnknown })
}

// AbortAction removes an admitted record for an action that was never handed
// to the daemon (its lease ended first).
func (a *Authority) AbortAction(l *Lease, key string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.running, runKey(l.ClientID, key))
	g := a.state.Grants[l.ClientID]
	if g == nil || g.Idem[key] == nil || g.Idem[key].State != idemAdmitted {
		return
	}
	r := g.Idem[key]
	delete(g.Idem, key)
	if err := a.commitLocked(); rolledBack(err) {
		g.Idem[key] = r // stays admitted, not running: ambiguous, never re-run
	}
}

func (a *Authority) endAction(l *Lease, key string, set func(*idemRecord)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.running, runKey(l.ClientID, key))
	g := a.state.Grants[l.ClientID]
	if g == nil || g.Idem[key] == nil {
		return
	}
	r := g.Idem[key]
	prev := *r
	set(r)
	if err := a.commitLocked(); rolledBack(err) {
		*r = prev
	}
}
