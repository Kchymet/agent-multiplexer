package direct

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// Updating a grant in place (amux direct clients update).
//
// A grant's permissions and resource scope can change without re-enrollment:
// the client keeps its client id, refresh token and access tokens. Admission
// (Authority.Admit) always reads the grant's current permissions, so:
//
//   - Widening applies to the client's next request (and its next token
//     refresh reports the new scope). Nothing open is interrupted.
//   - Narrowing applies to every new request at once, and the open work that
//     depended on what was dropped is cut before UpdateGrant returns: every
//     lease admitted under a dropped permission, every read lease (streams,
//     watches and reads whose filters were computed from the old grant) and,
//     when the resource scope shrank, every lease of the client. They end
//     with auth.grant_changed (CodeGrantChanged); the client renews if it
//     must, re-reads capabilities and reconnects under the new grant. A cut
//     action that was already handed to the daemon reports an unknown
//     outcome, exactly as under revocation.
//
// create and accounts only mean anything over all_sessions. Narrowing the scope
// to workgroups drops them when they were inherited from the old grant, and
// refuses them when they were asked for explicitly.
//
// Every change is recorded on the grant (newest maxGrantChanges, shown by
// clients list) and appended to the direct audit log (route "grant").

// AuditRouteGrant is the audit route of grant changes: verb "update" (host),
// "cloud_raise" (the cloud broker, within the host ceiling) or "replace"
// (enroll --replace revoked a superseded grant).
const AuditRouteGrant = "grant"

// Who changed a grant (GrantChange.Source).
const (
	GrantSourceHost  = "host"
	GrantSourceCloud = "cloud"
)

// maxGrantChanges bounds the change records kept on a grant.
const maxGrantChanges = 20

// grantChange is one stored in-place change.
type grantChange struct {
	At          int64         `json:"at"`
	Source      string        `json:"source"`       // GrantSourceHost | GrantSourceCloud
	By          string        `json:"by,omitempty"` // host actor, or the cloud requester
	OldPerms    []string      `json:"oldPerms"`
	OldResource ResourceScope `json:"oldResource"`
	NewPerms    []string      `json:"newPerms"`
	NewResource ResourceScope `json:"newResource"`
}

// GrantChange is the operator's view of one in-place grant change.
type GrantChange struct {
	At          int64         `json:"at"`
	Source      string        `json:"source"`
	By          string        `json:"by,omitempty"`
	OldPerms    []string      `json:"old_permissions"`
	OldResource ResourceScope `json:"old_resource_scope"`
	NewPerms    []string      `json:"new_permissions"`
	NewResource ResourceScope `json:"new_resource_scope"`
}

func publicChanges(cs []*grantChange) []GrantChange {
	if len(cs) == 0 {
		return nil
	}
	out := make([]GrantChange, 0, len(cs))
	for _, c := range cs {
		out = append(out, GrantChange{At: c.At, Source: c.Source, By: c.By,
			OldPerms: slices.Clone(c.OldPerms), OldResource: cloneScope(c.OldResource),
			NewPerms: slices.Clone(c.NewPerms), NewResource: cloneScope(c.NewResource)})
	}
	return out
}

func cloneScope(r ResourceScope) ResourceScope {
	return ResourceScope{Kind: r.Kind, Workgroups: slices.Clone(r.Workgroups)}
}

// GrantUpdate is one requested change. A nil Permissions keeps the current
// permissions; a nil Resource keeps the current scope; at least one is set.
type GrantUpdate struct {
	ClientID    string         `json:"client_id"`
	Permissions []string       `json:"permissions,omitempty"`
	Resource    *ResourceScope `json:"resource,omitempty"`
	// add (CloudRaiseGrant) asks for the current permissions plus these,
	// resolved under the authority lock so a concurrent narrowing is never
	// undone. Permissions must be nil with it.
	add []string
}

// GrantUpdateResult reports what an update did.
type GrantUpdateResult struct {
	Client ClientInfo `json:"client"`
	// Changed is false when the request matched the current grant (nothing
	// was written or audited).
	Changed bool `json:"changed"`
	// Old is the grant before the change (equal to Client's when unchanged).
	OldPermissions []string      `json:"old_permissions"`
	OldResource    ResourceScope `json:"old_resource_scope"`
	// Narrowed is set when anything was taken away; CutOperations counts the
	// open streams and calls that were ended with auth.grant_changed.
	Narrowed      bool `json:"narrowed"`
	CutOperations int  `json:"cut_operations"`
	// Dropped lists inherited permissions removed because the new scope
	// cannot carry them (create and accounts need all_sessions).
	Dropped []string `json:"dropped,omitempty"`
}

// allSessionsOnly are the permissions that only mean anything over all
// sessions (mayCreateRoots, mayReadAccounts).
var allSessionsOnly = []string{PermCreate, PermAccounts}

// covers reports whether scope a reaches at least every session scope b does.
func covers(a, b ResourceScope) bool { return withinCeiling(b, a) }

func scopeString(r ResourceScope) string {
	if r.Kind == ResourceWorkgroups {
		return "workgroups:" + strings.Join(r.Workgroups, ",")
	}
	return r.Kind
}

func grantString(perms []string, r ResourceScope) string {
	return strings.Join(perms, ",") + " " + scopeString(r)
}

// resolveUpdate computes the new permissions and scope of g under u.
func resolveUpdate(g *grantRecord, u GrantUpdate) (perms []string, res ResourceScope, dropped []string, err error) {
	if u.Permissions == nil && u.Resource == nil && u.add == nil {
		return nil, res, nil, errors.New("nothing to change: give permissions, a resource scope, or both")
	}
	asked := u.Permissions
	perms = slices.Clone(g.Perms)
	if u.add != nil {
		asked = u.add
		if perms, err = ValidatePerms(append(slices.Clone(g.Perms), u.add...)); err != nil {
			return nil, res, nil, err
		}
	} else if asked != nil {
		if perms, err = ValidatePerms(asked); err != nil {
			return nil, res, nil, err
		}
	}
	res = cloneScope(g.Resource)
	if u.Resource != nil {
		if res, err = ValidateResource(*u.Resource); err != nil {
			return nil, res, nil, err
		}
	}
	if res.Kind == ResourceWorkgroups {
		for _, p := range allSessionsOnly {
			if !slices.Contains(perms, p) {
				continue
			}
			if slices.Contains(asked, p) {
				return nil, res, nil, fmt.Errorf("%s needs the all_sessions scope (a workgroups grant cannot use it)", p)
			}
			dropped = append(dropped, p)
		}
		perms = slices.DeleteFunc(perms, func(p string) bool { return slices.Contains(dropped, p) })
	}
	return perms, res, dropped, nil
}

// UpdateGrant changes a live grant's permissions and/or resource scope in
// place (see the file comment). by names the host actor for the record. When
// it returns nil, the change is durable and every operation that depended on
// a narrowed right has ended.
func (a *Authority) UpdateGrant(u GrantUpdate, by string) (GrantUpdateResult, error) {
	return a.applyGrantChange(u, GrantSourceHost, by, nil)
}

// applyGrantChange is UpdateGrant for any source. check, when set, runs under
// the lock against the current grant and the resolved new grant and may
// refuse the change (the cloud ceiling).
func (a *Authority) applyGrantChange(u GrantUpdate, source, by string, check func(g *grantRecord, perms []string, res ResourceScope) error) (GrantUpdateResult, error) {
	a.mu.Lock()
	if err := a.issuableLocked(); err != nil {
		a.mu.Unlock()
		return GrantUpdateResult{}, err
	}
	g := a.state.Grants[u.ClientID]
	now := a.now().Unix()
	switch {
	case g == nil:
		a.mu.Unlock()
		return GrantUpdateResult{}, fmt.Errorf("no such client %q", u.ClientID)
	case g.RevokedAt != 0:
		a.mu.Unlock()
		return GrantUpdateResult{}, fmt.Errorf("client %s is revoked; enroll it again", u.ClientID)
	case now >= g.AbsoluteExpiry || now >= g.LastRefreshAt+g.IdleTTL || g.IdentityGeneration != a.identity.Generation:
		a.mu.Unlock()
		return GrantUpdateResult{}, fmt.Errorf("client %s has expired; enroll it again", u.ClientID)
	}
	perms, res, dropped, err := resolveUpdate(g, u)
	if err == nil && check != nil {
		err = check(g, perms, res)
	}
	if err != nil {
		a.mu.Unlock()
		return GrantUpdateResult{}, err
	}
	out := GrantUpdateResult{OldPermissions: slices.Clone(g.Perms), OldResource: cloneScope(g.Resource), Dropped: dropped}
	if slices.Equal(perms, g.Perms) && slices.Equal(res.Workgroups, g.Resource.Workgroups) && res.Kind == g.Resource.Kind {
		out.Client = a.clientInfoLocked(g)
		a.mu.Unlock()
		return out, nil
	}
	var lost []string
	for _, p := range g.Perms {
		if !slices.Contains(perms, p) {
			lost = append(lost, p)
		}
	}
	scopeShrank := !covers(res, g.Resource)
	out.Narrowed = len(lost) > 0 || scopeShrank

	prevPerms, prevRes, prevAt, prevChanges := g.Perms, g.Resource, g.UpdatedAt, g.Changes
	change := &grantChange{At: now, Source: source, By: by,
		OldPerms: slices.Clone(g.Perms), OldResource: cloneScope(g.Resource), NewPerms: slices.Clone(perms), NewResource: cloneScope(res)}
	g.Perms, g.Resource, g.UpdatedAt = perms, res, now
	g.Changes = append(slices.Clone(g.Changes), change)
	if len(g.Changes) > maxGrantChanges {
		g.Changes = g.Changes[len(g.Changes)-maxGrantChanges:]
	}
	err = a.commitLocked()
	if err != nil && (rolledBack(err) || !out.Narrowed) {
		// Not on disk (or durability unknown for a widening): the old grant
		// stands, in memory as on disk. A widening is never handed out on an
		// uncertain commit.
		g.Perms, g.Resource, g.UpdatedAt, g.Changes = prevPerms, prevRes, prevAt, prevChanges
		a.mu.Unlock()
		return GrantUpdateResult{}, fmt.Errorf("the grant change was not saved: %w", err)
	}
	// A narrowing stays in effect in memory even when its commit is
	// uncertain (like a revocation): failing open is never an option.
	var cut []*Lease
	if out.Narrowed {
		for l := range a.leases[g.ClientID] {
			if slices.Contains(lost, l.perm) || l.perm == PermRead || scopeShrank {
				cut = append(cut, l)
			}
		}
	}
	out.Changed, out.CutOperations = true, len(cut)
	out.Client = a.clientInfoLocked(g)
	label := g.Label
	a.mu.Unlock()

	verb := "update"
	if source == GrantSourceCloud {
		verb = "cloud_raise"
	}
	note := source
	if by != "" {
		note += ":" + by
	}
	if len(dropped) > 0 {
		note += "; dropped " + strings.Join(dropped, ",") + " (needs all_sessions)"
	}
	auditErr := a.Audit(AuditRecord{ClientID: u.ClientID, Label: label, Route: AuditRouteGrant, Verb: verb, Target: u.ClientID,
		Text: grantString(out.OldPermissions, out.OldResource) + " -> " + grantString(perms, res), Outcome: "ok", Note: note})

	drained := a.cut(cut)
	switch {
	case err != nil:
		return out, fmt.Errorf("the narrowing of %s is in effect in this running process but was NOT persisted (%w); "+
			"repair the store's storage and run the update again before restarting, or a restart will restore the old grant", u.ClientID, err)
	case !drained:
		return out, fmt.Errorf("grant changed and saved, but operations of %s did not finish within %s", u.ClientID, revokeDrain)
	case auditErr != nil:
		return out, fmt.Errorf("grant changed and saved, but the audit record could not be written: %w", auditErr)
	}
	return out, nil
}

// cut ends leases with auth.grant_changed and waits (bounded) for them.
func (a *Authority) cut(leases []*Lease) bool {
	for _, l := range leases {
		l.cancel(errGrantChanged)
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
	return true
}

// CloudRaiseGrant raises a grant on the cloud broker's authority, per the
// cloud-brokered delegation model: the requester is the authenticated Harness
// user, whom the host's owner pin must match, and the result must stay within
// the host's cloud ceiling. It only ever adds: the new permissions are the
// current ones plus perms, each of which the ceiling must hold, and the new
// scope (when resource is set) must cover the current one and lie within the
// ceiling's. Permissions the host granted beyond the ceiling stay as they
// are; the cloud can never take anything away. Refusals are CloudErrors with
// the cloud issuance codes.
//
// The wire route that reaches it (a client posting a Harness approval for an
// upgrade, verified over the provider connection like an enrollment
// approval) builds on the direct-approval-v1 bridge; see
// docs/direct-access.md, "Changing a grant".
func (a *Authority) CloudRaiseGrant(clientID string, perms []string, resource *ResourceScope, requester string) (GrantUpdateResult, error) {
	if requester == "" {
		return GrantUpdateResult{}, cloudErr(CloudErrInvalidRequest, "a raise needs the requester")
	}
	perms = ExpandPermAliases(perms)
	for _, p := range perms {
		if !slices.Contains(Permissions, p) {
			return GrantUpdateResult{}, cloudErr(CloudErrInvalidRequest, fmt.Sprintf("unknown permission %q", p))
		}
	}
	check := func(g *grantRecord, newPerms []string, res ResourceScope) error {
		c := a.state.Cloud
		switch {
		case c == nil || !c.Enabled:
			return cloudErr(CloudErrDisabled, "cloud enrollment is disabled on this host")
		case c.OwnerPin != "" && c.OwnerPin != requester:
			return cloudErr(CloudErrOwnerMismatch, "the requester is not this host's cloud owner")
		}
		for _, p := range perms {
			if !slices.Contains(g.Perms, strings.TrimSpace(p)) && !slices.Contains(c.Perms, strings.TrimSpace(p)) {
				return cloudErr(CloudErrScope, "permissions exceed this host's cloud ceiling")
			}
		}
		if resource != nil {
			if !covers(res, g.Resource) {
				return cloudErr(CloudErrScope, "the cloud can only widen a grant's scope")
			}
			if !withinCeiling(res, c.Resource) && !covers(g.Resource, res) {
				return cloudErr(CloudErrScope, "scope exceeds this host's cloud ceiling")
			}
		}
		return nil
	}
	add := slices.Clone(perms)
	if add == nil {
		add = []string{}
	}
	out, err := a.applyGrantChange(GrantUpdate{ClientID: clientID, Resource: resource, add: add}, GrantSourceCloud, requester, check)
	var ce *CloudError
	if err != nil && !errors.As(err, &ce) {
		return out, cloudErr(CloudErrInvalidRequest, err.Error())
	}
	return out, err
}

// supersededLocked lists the active grants a new grant g replaces under
// enroll --replace: same enrollment label (the host's --label; an older grant
// without one is matched by its client label) and the same device.
func (a *Authority) supersededLocked(g *grantRecord) []*grantRecord {
	var out []*grantRecord
	for _, o := range a.state.Grants {
		if o == g || o.RevokedAt != 0 {
			continue
		}
		label := o.EnrollLabel
		if label == "" {
			label = o.Label
		}
		if label == g.EnrollLabel && sameDevice(o, g) {
			out = append(out, o)
		}
	}
	return out
}

// sameDevice reports whether two grants came from the same device: equal
// device ids when both clients sent one, otherwise the same client kind and
// client label (the name the client gives itself).
func sameDevice(a, b *grantRecord) bool {
	if a.DeviceHash != "" && b.DeviceHash != "" {
		return a.DeviceHash == b.DeviceHash
	}
	return a.ClientKind == b.ClientKind && a.Label == b.Label
}

// DefaultStaleDays is clients list --stale's idle threshold.
const DefaultStaleDays = 14

// Stale lists the active grants that look unused, newest first: superseded
// by a newer active grant from the same device (same client kind and label,
// or device id), or not seen (no token refresh) for idleDays.
func (a *Authority) Stale(idleDays int) []ClientInfo {
	if idleDays <= 0 {
		idleDays = DefaultStaleDays
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now().Unix()
	var out []ClientInfo
	for _, g := range a.state.Grants {
		if g.RevokedAt != 0 {
			continue
		}
		why := ""
		var newer *grantRecord
		for _, o := range a.state.Grants {
			if o != g && o.RevokedAt == 0 && o.CreatedAt > g.CreatedAt && sameDevice(o, g) &&
				(g.EnrollLabel == "" || o.EnrollLabel == "" || g.EnrollLabel == o.EnrollLabel) {
				if newer == nil || o.CreatedAt > newer.CreatedAt {
					newer = o
				}
			}
		}
		if newer != nil {
			why = "superseded by " + newer.ClientID
		} else if idle := now - g.LastRefreshAt; idle >= int64(idleDays)*86400 {
			why = fmt.Sprintf("not seen for %d days", idle/86400)
		}
		if why == "" {
			continue
		}
		ci := a.clientInfoLocked(g)
		ci.Stale = why
		out = append(out, ci)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out
}

func (a *Authority) clientInfoLocked(g *grantRecord) ClientInfo {
	return ClientInfo{
		ClientID: g.ClientID, Label: g.Label, ClientKind: g.ClientKind, Permissions: slices.Clone(g.Perms),
		ResourceScope: cloneScope(g.Resource), CreatedAt: g.CreatedAt, LastRefreshAt: g.LastRefreshAt,
		GrantExpiresAt: g.AbsoluteExpiry, RevokedAt: g.RevokedAt, RevokeReason: g.RevokeReason,
		ActiveLeases: len(a.leases[g.ClientID]), Origin: g.Origin, CloudRequester: g.CloudRequester,
		EnrollLabel: g.EnrollLabel, UpdatedAt: g.UpdatedAt, Changes: publicChanges(g.Changes), ReplacedBy: g.ReplacedBy,
	}
}

// grantInfo is the lease's client's current grant, for capabilities. It reads
// the live grant, not the lease's admission snapshot, so a widening shows on
// the client's next capabilities read.
func (a *Authority) grantInfo(l *Lease) *GrantInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	g := a.state.Grants[l.ClientID]
	if g == nil {
		return nil
	}
	return &GrantInfo{ClientID: g.ClientID, Label: g.Label, Permissions: slices.Clone(g.Perms), ResourceScope: cloneScope(g.Resource),
		CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt, ExpiresAt: g.AbsoluteExpiry, Origin: g.Origin}
}
