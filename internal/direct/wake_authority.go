package direct

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"time"

	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Direct wake-ups, authority half (docs/direct-wake-contract.md §6.2). The
// low-frequency authority facts live in grants.json under the existing commit
// semantics: the host policy (with a random nonce that changes on every policy
// change) and each grant's wake subscriptions. High-frequency wake data
// (marks, records, outbox) lives in wake.json (wake_store.go) and is inert
// unless the facts here still vouch for it.

// Wake bounds.
const (
	MaxWakeSubsPerGrant = 4
	// MaxWakeFailedPerGrant bounds retained failure records per grant (they
	// also expire after wakeFailedRetention), so repeated failed setups cannot
	// grow grants.json without bound.
	MaxWakeFailedPerGrant = 16
	WakeActivationTTL     = 10 * time.Minute
	wakeFailedRetention   = 24 * time.Hour
	wakeGrantRefLen       = 22
	wakeSubStateActivate  = hp.DirectWakeStateActivating
	wakeSubStateActive    = hp.DirectWakeStateActive
	wakeSubStateFailed    = "failed"
)

// maxWakeSubsPerHost bounds activating+active subscriptions across all grants
// so one route-sync epoch (MaxDirectWakeRouteParts parts of
// MaxDirectWakeRoutes routes) always lists every one: a sync is never
// truncated. A variable only so tests can exercise the bound.
var maxWakeSubsPerHost = hp.MaxDirectWakeRouteParts * hp.MaxDirectWakeRoutes

// Wake subscription failure reasons (wakeSub.Failure).
const (
	WakeFailRouteGone         = "route_gone"
	WakeFailActivationExpired = "activation_expired"
)

// Wake API refusal codes.
const (
	CodeWakeDisabled      = "wake_disabled"
	CodeWakeAlreadyActive = "already_active"
	CodeWakeIncarnation   = "incarnation_changed"
)

var (
	errWakeDisabled      = errors.New(CodeWakeDisabled)
	errWakeAlreadyActive = errors.New(CodeWakeAlreadyActive)
	errWakeCapacity      = errors.New(CodeCapacity)
)

// wakePolicy is storeState.Wake. Nonce is 16 random bytes, regenerated on every
// enable, re-pin and disable, so wake data written under an earlier policy (or
// before an older binary dropped this field) can never match again. Key is the
// HMAC key for grant_ref, regenerated with the nonce.
type wakePolicy struct {
	Enabled       bool   `json:"enabled"`
	Nonce         string `json:"nonce"`
	Key           string `json:"key,omitempty"`
	OwnerPin      string `json:"ownerPin,omitempty"`
	ProviderPin   string `json:"providerPin,omitempty"`
	ChangedAt     int64  `json:"changedAt"`
	DisableReason string `json:"disableReason,omitempty"`
}

// wakeSub is one grant's subscription to a Harness route (grantRecord.WakeSubs,
// keyed by route id). Secret is the phone's activation secret: host-only
// replay material for the activation frame, erased as soon as activation
// reaches a terminal state, expires, or the server restarts past its TTL.
// SecretDigest (SHA-256 of the secret) survives so a repeated subscribe with
// the same secret stays idempotent.
type wakeSub struct {
	CreatedAt    int64  `json:"createdAt"`
	ActivatingAt int64  `json:"activatingAt"`
	Nonce        string `json:"nonce"`
	State        string `json:"state"`
	Failure      string `json:"failure,omitempty"`
	FailedAt     int64  `json:"failedAt,omitempty"`
	Secret       string `json:"secret,omitempty"`
	SecretDigest string `json:"secretDigest"`
}

// WakeStatus is the operator's view of the wake policy.
type WakeStatus struct {
	Enabled       bool   `json:"enabled"`
	OwnerPin      string `json:"owner_pin,omitempty"`
	ProviderPin   string `json:"provider_pin,omitempty"`
	ChangedAt     int64  `json:"changed_at,omitempty"`
	DisableReason string `json:"disable_reason,omitempty"`
	Subscriptions int    `json:"subscriptions"`
	Active        int    `json:"active"`
	// Degraded is set by the wake store (an ambiguous wake.json write) or the
	// detector (lag); never by the authority.
	Degraded bool   `json:"degraded,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// WakeSubView is a grant's view of one subscription.
type WakeSubView struct {
	RouteID   string `json:"route_id"`
	State     string `json:"state"`
	Failure   string `json:"failure,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

// wakeAuth is a consistent snapshot of what the authority vouches for, taken
// under the authority lock and used by the wake store without holding it.
type wakeAuth struct {
	Nonce string
	Subs  []wakeAuthSub
}

type wakeAuthSub struct {
	ClientID  string
	RouteID   string
	CreatedAt int64 // unix seconds the subscription was created
	State     string
	GrantRef  string
	Requester string
	Grant     Grant
	// Activation (State == activating, within TTL, secret present).
	Secret       string
	SecretDigest string
	ActivatingAt int64
}

func randomB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func secretDigest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (p *wakePolicy) grantRef(clientID string) string {
	key, _ := base64.RawURLEncoding.DecodeString(p.Key)
	m := hmac.New(sha256.New, key)
	m.Write([]byte("amux-direct-wake/grant-ref\x00"))
	m.Write([]byte(clientID))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:wakeGrantRefLen]
}

// grantLiveLocked reports whether g can still act now (the same tests refresh
// and admission apply).
func (a *Authority) grantLiveLocked(g *grantRecord, now int64) bool {
	return g != nil && g.RevokedAt == 0 && g.IdentityGeneration == a.identity.Generation &&
		now < g.AbsoluteExpiry && now < g.LastRefreshAt+g.IdleTTL
}

func (a *Authority) wakeEnabledLocked() *wakePolicy {
	if p := a.state.Wake; p != nil && p.Enabled && p.Nonce != "" {
		return p
	}
	return nil
}

// clearWakeSubsLocked drops every grant's subscriptions (policy change).
func (a *Authority) clearWakeSubsLocked() {
	for _, g := range a.state.Grants {
		g.WakeSubs = nil
	}
}

// EnableWake enables (or re-pins) wake-ups. Every call starts a new nonce and
// key and clears all subscriptions: data written under the previous policy is
// inert from this commit on.
func (a *Authority) EnableWake(owner, provider string) (WakeStatus, error) {
	if !validPrincipal(owner) {
		return WakeStatus{}, errors.New("the Harness owner pin is required (1-128 printable bytes)")
	}
	if !validPrincipal(provider) {
		return WakeStatus{}, errors.New("the Harness provider pin is required (1-128 printable bytes)")
	}
	nonce, err := randomB64(16)
	if err != nil {
		return WakeStatus{}, err
	}
	key, err := randomB64(32)
	if err != nil {
		return WakeStatus{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.issuableLocked(); err != nil {
		return WakeStatus{}, err
	}
	prev := a.state.Wake
	prevSubs := a.snapshotWakeSubsLocked()
	a.state.Wake = &wakePolicy{Enabled: true, Nonce: nonce, Key: key, OwnerPin: owner, ProviderPin: provider, ChangedAt: a.now().Unix()}
	a.clearWakeSubsLocked()
	if err := a.commitLocked(); err != nil {
		if rolledBack(err) {
			a.state.Wake = prev
			a.restoreWakeSubsLocked(prevSubs)
		}
		return WakeStatus{}, err
	}
	return a.wakeStatusLocked(), nil
}

// DisableWake turns wake-ups off, clearing every subscription under a new
// nonce. A failed write still leaves wake off in this process.
func (a *Authority) DisableWake(reason string) (WakeStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.disableWakeLocked(reason, a.now().Unix())
	if err := a.commitLocked(); err != nil {
		return WakeStatus{}, fmt.Errorf("wake-ups are off in this running process but the change was NOT persisted (%w); repair the store's storage and disable again before restarting", err)
	}
	return a.wakeStatusLocked(), nil
}

func (a *Authority) disableWakeLocked(reason string, now int64) {
	p := a.state.Wake
	if p == nil {
		return
	}
	nonce, err := randomB64(16)
	if err != nil {
		nonce = "" // still disabled: an empty nonce matches no wake data
	}
	*p = wakePolicy{Enabled: false, Nonce: nonce, ChangedAt: now, DisableReason: reason}
	a.clearWakeSubsLocked()
}

func (a *Authority) snapshotWakeSubsLocked() map[string]map[string]*wakeSub {
	out := map[string]map[string]*wakeSub{}
	for id, g := range a.state.Grants {
		if len(g.WakeSubs) == 0 {
			continue
		}
		m := map[string]*wakeSub{}
		for r, s := range g.WakeSubs {
			c := *s
			m[r] = &c
		}
		out[id] = m
	}
	return out
}

func (a *Authority) restoreWakeSubsLocked(snap map[string]map[string]*wakeSub) {
	for id, g := range a.state.Grants {
		g.WakeSubs = snap[id]
	}
}

// WakeStatus reports the policy.
func (a *Authority) WakeStatus() WakeStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.wakeStatusLocked()
}

func (a *Authority) wakeStatusLocked() WakeStatus {
	p := a.state.Wake
	if p == nil {
		return WakeStatus{}
	}
	st := WakeStatus{Enabled: p.Enabled, OwnerPin: p.OwnerPin, ProviderPin: p.ProviderPin, ChangedAt: p.ChangedAt, DisableReason: p.DisableReason}
	if !p.Enabled {
		return st
	}
	now := a.now().Unix()
	for _, g := range a.state.Grants {
		if !a.grantLiveLocked(g, now) {
			continue
		}
		for _, s := range g.WakeSubs {
			if s.Nonce != p.Nonce || s.State == wakeSubStateFailed {
				continue
			}
			st.Subscriptions++
			if s.State == wakeSubStateActive {
				st.Active++
			}
		}
	}
	return st
}

// WakeSubscribe creates or updates the lease's grant's subscription to
// routeID with the phone's activation secret. Same secret: idempotent. New
// secret on a pending or failed subscription: replaces the pending activation.
// New secret on an active one: errWakeAlreadyActive. There is no global
// reservation: other grants may hold activating entries for the same route.
func (a *Authority) WakeSubscribe(l *Lease, routeID, secret string) (WakeSubView, error) {
	if !hp.ValidDirectWakeRouteID(routeID) || !hp.ValidDirectWakeSecret(secret) {
		return WakeSubView{}, errBadRequest
	}
	digest := secretDigest(secret)
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now().Unix()
	p := a.wakeEnabledLocked()
	if p == nil {
		return WakeSubView{}, errWakeDisabled
	}
	if err := a.issuableLocked(); err != nil {
		return WakeSubView{}, err
	}
	g := a.state.Grants[l.ClientID]
	if !a.grantLiveLocked(g, now) {
		return WakeSubView{}, errAuthRevoked
	}
	a.pruneWakeSubsLocked(g, p, now)
	if s := g.WakeSubs[routeID]; s != nil {
		switch {
		case s.SecretDigest == digest:
			return wakeView(routeID, s), nil
		case s.State == wakeSubStateActive:
			return WakeSubView{}, errWakeAlreadyActive
		}
	}
	if s := g.WakeSubs[routeID]; s == nil || s.State == wakeSubStateFailed {
		// This call adds a live subscription: both caps apply.
		if countLiveSubs(g) >= MaxWakeSubsPerGrant || a.countHostLiveSubsLocked(p, now) >= maxWakeSubsPerHost {
			return WakeSubView{}, errWakeCapacity
		}
	}
	prev := g.WakeSubs[routeID]
	next := &wakeSub{CreatedAt: now, ActivatingAt: now, Nonce: p.Nonce, State: wakeSubStateActivate, Secret: secret, SecretDigest: digest}
	if prev != nil {
		next.CreatedAt = prev.CreatedAt
	}
	if g.WakeSubs == nil {
		g.WakeSubs = map[string]*wakeSub{}
	}
	g.WakeSubs[routeID] = next
	if err := a.commitLocked(); err != nil {
		if rolledBack(err) {
			if prev == nil {
				delete(g.WakeSubs, routeID)
			} else {
				g.WakeSubs[routeID] = prev
			}
		}
		return WakeSubView{}, err
	}
	return wakeView(routeID, next), nil
}

func (a *Authority) countHostLiveSubsLocked(p *wakePolicy, now int64) int {
	n := 0
	for _, g := range a.state.Grants {
		if !a.grantLiveLocked(g, now) {
			continue
		}
		for _, s := range g.WakeSubs {
			if s.Nonce == p.Nonce && s.State != wakeSubStateFailed && !activationExpired(s, now) {
				n++
			}
		}
	}
	return n
}

func activationExpired(s *wakeSub, now int64) bool {
	return s.State == wakeSubStateActivate && now >= s.ActivatingAt+int64(WakeActivationTTL/time.Second)
}

func countLiveSubs(g *grantRecord) int {
	n := 0
	for _, s := range g.WakeSubs {
		if s.State != wakeSubStateFailed {
			n++
		}
	}
	return n
}

// pruneWakeSubsLocked erases expired activation secrets (failing the
// subscription), drops entries from another policy, and forgets failed entries
// older than wakeFailedRetention. In memory only: the caller commits with its
// own change, and a restart repeats it (startup also calls it explicitly).
func (a *Authority) pruneWakeSubsLocked(g *grantRecord, p *wakePolicy, now int64) bool {
	changed := false
	for r, s := range g.WakeSubs {
		switch {
		case p == nil || s.Nonce != p.Nonce:
			delete(g.WakeSubs, r)
			changed = true
		case activationExpired(s, now):
			s.State, s.Failure, s.FailedAt, s.Secret = wakeSubStateFailed, WakeFailActivationExpired, now, ""
			changed = true
		case s.State == wakeSubStateFailed && now >= s.FailedAt+int64(wakeFailedRetention/time.Second):
			delete(g.WakeSubs, r)
			changed = true
		case s.State != wakeSubStateActivate && s.Secret != "":
			s.Secret = ""
			changed = true
		}
	}
	// Keep only the newest MaxWakeFailedPerGrant failure records.
	var failed []string
	for r, s := range g.WakeSubs {
		if s.State == wakeSubStateFailed {
			failed = append(failed, r)
		}
	}
	if len(failed) > MaxWakeFailedPerGrant {
		sort.Slice(failed, func(i, j int) bool {
			fi, fj := g.WakeSubs[failed[i]].FailedAt, g.WakeSubs[failed[j]].FailedAt
			if fi != fj {
				return fi < fj
			}
			return failed[i] < failed[j]
		})
		for _, r := range failed[:len(failed)-MaxWakeFailedPerGrant] {
			delete(g.WakeSubs, r)
		}
		changed = true
	}
	return changed
}

func wakeView(routeID string, s *wakeSub) WakeSubView {
	return WakeSubView{RouteID: routeID, State: s.State, Failure: s.Failure, CreatedAt: s.CreatedAt}
}

// WakeUnsubscribe removes the lease's grant's subscription. Route sync then
// stops listing it, which deactivates the route at Harness.
func (a *Authority) WakeUnsubscribe(l *Lease, routeID string) (bool, error) {
	if !hp.ValidDirectWakeRouteID(routeID) {
		return false, errBadRequest
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.state.Grants[l.ClientID]
	if g == nil || g.WakeSubs[routeID] == nil {
		return false, nil
	}
	prev := g.WakeSubs[routeID]
	delete(g.WakeSubs, routeID)
	if err := a.commitLocked(); err != nil {
		if rolledBack(err) {
			g.WakeSubs[routeID] = prev
		}
		return false, err
	}
	return true, nil
}

// WakeSubscriptions lists the lease's grant's subscriptions.
func (a *Authority) WakeSubscriptions(l *Lease) []WakeSubView {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.state.Grants[l.ClientID]
	p := a.wakeEnabledLocked()
	out := []WakeSubView{}
	if g == nil || p == nil {
		return out
	}
	now := a.now().Unix()
	for r, s := range g.WakeSubs {
		if s.Nonce != p.Nonce {
			continue
		}
		v := wakeView(r, s)
		if s.State == wakeSubStateActivate && now >= s.ActivatingAt+int64(WakeActivationTTL/time.Second) {
			v.State, v.Failure = wakeSubStateFailed, WakeFailActivationExpired
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RouteID < out[j].RouteID })
	return out
}

// wakeAuthSnapshot returns what the authority vouches for now: the current
// nonce and every live grant's non-failed subscription. Nil when wake is off.
func (a *Authority) wakeAuthSnapshot() *wakeAuth {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.wakeEnabledLocked()
	if p == nil {
		return nil
	}
	now := a.now().Unix()
	out := &wakeAuth{Nonce: p.Nonce}
	for id, g := range a.state.Grants {
		if !a.grantLiveLocked(g, now) {
			continue
		}
		for r, s := range g.WakeSubs {
			if s.Nonce != p.Nonce || s.State == wakeSubStateFailed {
				continue
			}
			sub := wakeAuthSub{
				ClientID: id, RouteID: r, CreatedAt: s.CreatedAt, State: s.State, GrantRef: p.grantRef(id), Requester: g.CloudRequester,
				Grant: Grant{ClientID: id, Perms: append([]string(nil), g.Perms...), Resource: g.Resource},
			}
			if s.State == wakeSubStateActivate {
				if now >= s.ActivatingAt+int64(WakeActivationTTL/time.Second) || s.Secret == "" {
					continue
				}
				sub.Secret, sub.SecretDigest, sub.ActivatingAt = s.Secret, s.SecretDigest, s.ActivatingAt
			}
			out.Subs = append(out.Subs, sub)
		}
	}
	sort.Slice(out.Subs, func(i, j int) bool {
		if out.Subs[i].ClientID != out.Subs[j].ClientID {
			return out.Subs[i].ClientID < out.Subs[j].ClientID
		}
		return out.Subs[i].RouteID < out.Subs[j].RouteID
	})
	return out
}

// WakeSettleActivation records Harness's terminal answer to an activation
// frame. It applies only if the subscription is still the same activation
// (same nonce and secret digest); otherwise it is ignored. The secret is
// erased on every terminal transition.
func (a *Authority) WakeSettleActivation(clientID, routeID, nonce, digest string, accepted bool, failure string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.wakeEnabledLocked()
	g := a.state.Grants[clientID]
	if p == nil || p.Nonce != nonce || g == nil {
		return nil
	}
	s := g.WakeSubs[routeID]
	if s == nil || s.Nonce != nonce || s.SecretDigest != digest || s.State != wakeSubStateActivate {
		return nil
	}
	prev := *s
	now := a.now().Unix()
	if accepted {
		s.State, s.Failure, s.Secret = wakeSubStateActive, "", ""
	} else {
		s.State, s.Failure, s.FailedAt, s.Secret = wakeSubStateFailed, failure, now, ""
	}
	before := a.snapshotWakeSubsLocked()[clientID]
	before[routeID] = &prev
	a.pruneWakeSubsLocked(g, p, now)
	if err := a.commitLocked(); err != nil {
		if rolledBack(err) {
			g.WakeSubs = before
		}
		return err
	}
	return nil
}

// WakeFailRoute marks an active subscription failed after Harness reported its
// route gone for a wake.
func (a *Authority) WakeFailRoute(clientID, routeID, nonce string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.state.Grants[clientID]
	if g == nil {
		return nil
	}
	s := g.WakeSubs[routeID]
	if s == nil || s.Nonce != nonce || s.State == wakeSubStateFailed {
		return nil
	}
	before := a.snapshotWakeSubsLocked()[clientID]
	now := a.now().Unix()
	s.State, s.Failure, s.FailedAt, s.Secret = wakeSubStateFailed, WakeFailRouteGone, now, ""
	a.pruneWakeSubsLocked(g, a.wakeEnabledLocked(), now)
	if err := a.commitLocked(); err != nil {
		if rolledBack(err) {
			g.WakeSubs = before
		}
		return err
	}
	return nil
}

// pruneWakeLocked erases activation secrets past their TTL, drops entries of
// dead grants or another policy, and bounds failure records, in memory only.
// It never writes: opening the authority runs it without a commit (so wake
// cleanup can never stop direct from starting or degrade the authority), and
// commitLocked runs it before every real commit, so the next ordinary
// authority write persists the erasure. Disk erasure of an already expired
// secret is therefore best-effort: it is unusable at once (wakeAuthSnapshot,
// WakeSubscriptions and leasing skip it) and leaves the 0600 host-only file at
// the next authority commit. Successful activation and revocation erase it in
// their own commits.
func (a *Authority) pruneWakeLocked() bool {
	p := a.wakeEnabledLocked()
	now := a.now().Unix()
	changed := false
	for _, g := range a.state.Grants {
		if len(g.WakeSubs) == 0 {
			continue
		}
		if !a.grantLiveLocked(g, now) {
			g.WakeSubs = nil
			changed = true
			continue
		}
		if a.pruneWakeSubsLocked(g, p, now) {
			changed = true
		}
	}
	return changed
}
