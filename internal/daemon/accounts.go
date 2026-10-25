package daemon

import (
	"amux/internal/accounts"
	"amux/internal/agent"
	"amux/internal/centralauth"
	"amux/internal/codexapp"
	"amux/internal/codexauth"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/store"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
	"strconv"
	"strings"
	"time"
)

type accountAttempt struct {
	profile, initiator string
	epoch              uint64
	retiredEpoch       uint64
	prior              hp.AccountProfile
	guard              context.Context
	ceremony           hp.AccountCeremony
	cancel             context.CancelFunc
}

func accountRequest(a core.Action) (hp.AccountRequest, error) {
	var r hp.AccountRequest
	if a.ID != "" || a.Target != "" || a.Kind != "" || len(a.Fields) != 1 || a.Fields["request"] == "" {
		return r, errors.New("account_request_invalid")
	}
	if len(a.Fields["request"]) > 8192 || json.Unmarshal([]byte(a.Fields["request"]), &r) != nil {
		return r, errors.New("account_request_invalid")
	}
	return r, nil
}

// accountCall is reached only from the authenticated host channel. Restricted
// session RPC has no account status/manage operation in its closed vocabulary.
// Provider grants are checked before forwarding onto this host channel.
func (d *Daemon) accountCall(ctx context.Context, r hp.AccountRequest, ceremony bool) hp.AccountResult {
	fail := func(e string) hp.AccountResult { return hp.AccountResult{Error: e} }
	if !hp.ValidateAccountRequest(r, ceremony) {
		return fail("account_request_invalid")
	}
	if !effectAdmissionHeld(ctx) && r.Operation != hp.AccountStatus {
		d.effectMu.Lock()
		defer d.effectMu.Unlock()
	}
	if ctx.Err() != nil || revalidateDeferred(ctx) != nil {
		return fail("account_access_denied")
	}
	if r.Operation == hp.AccountDisconnect {
		d.disconnectAccounts(r.Initiator, false)
		return hp.AccountResult{OK: true}
	}
	if r.Operation == hp.AccountStatus && !ceremony {
		d.accountMu.Lock()
		d.expireAccountAttemptsLocked()
		d.accountMu.Unlock()
		s, e := d.accountSnapshot(ctx)
		if e != nil {
			return fail("account_status_unavailable")
		}
		s = s.ManagedOnly()
		return hp.AccountResult{OK: true, Snapshot: &s}
	}
	if r.ProfileID == "" {
		return fail("account_profile_required")
	}
	if r.Operation == hp.AccountLogin || r.Operation == hp.AccountPoll || r.Operation == hp.AccountCancel || r.Operation == hp.AccountComplete {
		if !ceremony || r.Initiator == "" || len(r.Initiator) > 512 {
			return fail("account_ceremony_required")
		}
	} else if ceremony || r.Secret != "" || r.AttemptID != "" || r.Initiator != "" {
		return fail("account_request_invalid")
	}
	d.accountMu.Lock()
	defer d.accountMu.Unlock()
	if d.accountAttempts == nil {
		d.accountAttempts = map[string]*accountAttempt{}
	}
	d.expireAccountAttemptsLocked()
	if r.Operation == hp.AccountPoll {
		a := d.accountAttempts[r.AttemptID]
		if a == nil || a.profile != r.ProfileID || a.initiator != r.Initiator {
			return fail("account_attempt_unknown")
		}
		c := a.ceremony
		return hp.AccountResult{OK: true, Ceremony: &c}
	}
	if r.Operation == hp.AccountCancel || r.Operation == hp.AccountComplete {
		a := d.accountAttempts[r.AttemptID]
		if a == nil || a.profile != r.ProfileID || a.initiator != r.Initiator {
			return fail("account_attempt_unknown")
		}
		if a.ceremony.State != "pending" {
			return fail("account_attempt_finished")
		}
		if r.Operation == hp.AccountComplete {
			if a.ceremony.Method != "setup-token" {
				return fail("account_operation_unsupported")
			}
			secret, e := centralauth.NewSecret(r.Secret)
			if e != nil {
				return fail("account_secret_invalid")
			}
			e = accounts.Update(func(s *accounts.State) error {
				if revalidateDeferred(ctx) != nil || revalidateDeferred(a.guard) != nil {
					return errors.New("account_access_denied")
				}
				p := s.Profiles[a.profile]
				if p.DeliveryEpoch != a.epoch {
					return accounts.ErrStale
				}
				cred, e := centralauth.ClaudeStore().Save(secret, time.Now())
				if e != nil {
					return e
				}
				s.CredentialVersion[p.ID] = cred.Generation
				p.State = "ready"
				p.Error = ""
				p.Revision++
				s.Profiles[p.ID] = p
				return nil
			})
			if e != nil {
				return fail("account_completion_failed")
			}
			a.ceremony.State = "succeeded"
			a.cancel()
			c := a.ceremony
			return hp.AccountResult{OK: true, Ceremony: &c}
		}
		d.retireAccountAttemptLocked(a, "cancelled")
		c := a.ceremony
		return hp.AccountResult{OK: true, Ceremony: &c}
	}
	if r.Revision == 0 {
		return fail("account_revision_required")
	}
	if r.Operation == hp.AccountDefault || r.Operation == hp.AccountLogin || r.Operation == hp.AccountLogout {
		db, err := store.Open()
		if err != nil {
			return fail("account_store_unavailable")
		}
		rows, err := db.AllSessions()
		db.Close()
		if err != nil {
			return fail("account_store_unavailable")
		}
		// The built-in console is synthetic and absent from SQL inventory.
		rows = append(rows, console.Session())
		for _, row := range rows {
			if err := accounts.Bind(row.ID, agent.Canonical(row.Agent)); err != nil {
				return fail("account_binding_invalid")
			}
		}
	}
	if r.Operation == hp.AccountRebind {
		db, err := store.Open()
		if err != nil {
			return fail("account_workload_unknown")
		}
		row, ok, err := lookupPolicySession(db, r.WorkloadID)
		db.Close()
		if err != nil || !ok || row.Archived {
			return fail("account_workload_unknown")
		}
		s, err := accounts.Read()
		if err != nil {
			return fail("account_status_unavailable")
		}
		p, err := s.Profile(r.ProfileID, r.Revision)
		if err != nil || p.Harness != agent.Canonical(row.Agent) || p.State != "ready" {
			return fail("account_binding_invalid")
		}
		if p.Harness == "codex" {
			identity, exists := codexapp.LoadIdentity(r.WorkloadID)
			if exists && identity.ThreadID != "" && identity.ChatGPTAccount != "" && identity.ChatGPTAccount != p.Identity.AccountID {
				return fail("account_rebind_requires_new_workload")
			}
		}
		// Runtime teardown may wait for startup/native auth. Do not hold the
		// attempt mutex needed by native completion while waiting for that gate.
		d.accountMu.Unlock()
		d.killRuntimeFor(r.WorkloadID)
		d.closeCodex(r.WorkloadID)
		d.accountMu.Lock()
	}
	var p, prior hp.AccountProfile
	e := accounts.Update(func(s *accounts.State) error {
		if revalidateDeferred(ctx) != nil {
			return errors.New("account_access_denied")
		}
		var err error
		p, err = s.Profile(r.ProfileID, r.Revision)
		if err != nil {
			return err
		}
		prior = p
		switch r.Operation {
		case hp.AccountRebind:
			b, ok := s.Bindings[r.WorkloadID]
			if !ok || b.Harness != p.Harness {
				return accounts.ErrBlocked
			}
			if p.State != "ready" {
				return accounts.ErrBlocked
			}
			b.ProfileID = p.ID
			b.Source = p.Source
			b.ExpectedAccount = p.Identity.AccountID
			b.CredentialVersion = s.CredentialVersion[p.ID]
			b.CredentialUnknown = false
			b.Revision++
			b.Origin = "explicit-rebind"
			s.Bindings[r.WorkloadID] = b
		case hp.AccountDefault:
			if s.DefaultRevisions[p.Harness] != r.DefaultRevision {
				return accounts.ErrStale
			}
			s.DefaultRevisions[p.Harness]++
			s.Defaults[p.Harness] = p.ID
			p.Revision++
		case hp.AccountLogin, hp.AccountLogout:
			if p.Source != "central" {
				return errors.New("account_operation_unsupported")
			}
			if r.Operation == hp.AccountLogin && p.State == "login-pending" {
				return errors.New("account_login_busy")
			}
			p.DeliveryEpoch++
			p.Revision++
			p.Limits.State = "unavailable"
			p.Limits.Windows = nil
			p.Identity = hp.AccountIdentity{State: "unknown", Evidence: p.Identity.Evidence}
			if r.Operation == hp.AccountLogin {
				p.State = "login-pending"
			} else {
				p.State = "logged-out"
			}
		default:
			return errors.New("account_operation_unsupported")
		}
		s.Profiles[p.ID] = p
		return nil
	})
	if e != nil {
		if errors.Is(e, accounts.ErrStale) {
			return fail("account_revision_changed")
		}
		return fail("account_operation_failed")
	}
	if r.Operation == hp.AccountDefault || r.Operation == hp.AccountRebind {
		return hp.AccountResult{OK: true}
	}
	for _, a := range d.accountAttempts {
		if a.profile == p.ID {
			a.cancel()
			a.ceremony.State = "cancelled"
			a.ceremony.AuthURL = ""
		}
	}
	if r.Operation == hp.AccountLogout {
		// The durable fence precedes native IO. Failure cannot reopen delivery.
		cleanupCode := "account_logout_cleanup_failed"
		if p.Harness == "claude" {
			e = centralauth.ClaudeStore().Delete()
		} else if d.centralCodex != nil && d.centralCodex.provider != nil {
			d.accountMu.Unlock()
			e = d.centralCodex.provider.Logout(ctx)
			d.accountMu.Lock()
		} else {
			e = errCentralCodexUnavailable
			cleanupCode = "account_logout_cleanup_unavailable"
		}
		if e != nil {
			_ = accounts.Update(func(s *accounts.State) error {
				q := s.Profiles[p.ID]
				if q.DeliveryEpoch == p.DeliveryEpoch {
					q.Error = cleanupCode
					q.Revision++
					s.Profiles[q.ID] = q
				}
				return nil
			})
			return fail(cleanupCode)
		}
		if e = accounts.Update(func(s *accounts.State) error {
			q := s.Profiles[p.ID]
			if q.DeliveryEpoch == p.DeliveryEpoch && q.Error != "" {
				q.Error = ""
				q.Revision++
				s.Profiles[q.ID] = q
			}
			return nil
		}); e != nil {
			return fail("account_status_unavailable")
		}
		return hp.AccountResult{OK: true}
	}
	var nonce [24]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return fail("account_login_unavailable")
	}
	id := hex.EncodeToString(nonce[:])
	lifetime := context.Background()
	if d.centralCodex != nil && d.centralCodex.lifetime != nil {
		lifetime = d.centralCodex.lifetime
	}
	loginCtx, cancel := context.WithTimeout(lifetime, codexLoginTimeout)
	stopOwner := context.AfterFunc(ctx, cancel)
	a := &accountAttempt{prior: prior, guard: ctx, profile: p.ID, initiator: r.Initiator, epoch: p.DeliveryEpoch, cancel: cancel, ceremony: hp.AccountCeremony{AttemptID: id, State: "pending", Method: p.LoginMethod, ExpiresAt: d.accountTime().Add(codexLoginTimeout).UnixMilli()}}
	d.accountAttempts[id] = a
	go func() {
		<-loginCtx.Done()
		stopOwner()
		d.accountMu.Lock()
		defer d.accountMu.Unlock()
		if a.ceremony.State == "pending" {
			d.finishExpiredAccountLocked(a)
		}
	}()
	if p.Harness == "codex" {
		if d.centralCodex == nil || d.centralCodex.provider == nil {
			cancel()
			a.ceremony.State = "failed"
			_ = accounts.Update(func(s *accounts.State) error {
				q := s.Profiles[p.ID]
				q.State = "blocked"
				if q.Error == "" {
					q.Error = "account_native_unavailable"
				}
				q.Revision++
				s.Profiles[q.ID] = q
				return nil
			})
			return fail("account_native_unavailable")
		}
		go d.runAccountLogin(loginCtx, a)
	}
	c := a.ceremony
	return hp.AccountResult{OK: true, Ceremony: &c}
}
func (d *Daemon) runAccountLogin(ctx context.Context, a *accountAttempt) {
	err := d.centralCodex.provider.Login(ctx, func(url string) {
		d.accountMu.Lock()
		defer d.accountMu.Unlock()
		if ctx.Err() == nil && revalidateDeferred(a.guard) == nil && a.ceremony.State == "pending" {
			a.ceremony.AuthURL = url
		}
	}, func() error {
		// This callback runs before the native owner releases its operation gate.
		// Failure causes it to reap the writer and restore the prior auth file.
		d.accountMu.Lock()
		defer d.accountMu.Unlock()
		if ctx.Err() != nil || revalidateDeferred(a.guard) != nil || a.ceremony.State != "pending" {
			return errors.New("account_login_retired")
		}
		if err := accounts.Update(func(s *accounts.State) error {
			if revalidateDeferred(a.guard) != nil {
				return errors.New("account_access_denied")
			}
			p := s.Profiles[a.profile]
			if p.DeliveryEpoch != a.epoch {
				return accounts.ErrStale
			}
			p.State = "ready"
			p.Error = ""
			p.Revision++
			s.Profiles[p.ID] = p
			return nil
		}); err != nil {
			return err
		}
		a.ceremony.State = "succeeded"
		a.ceremony.AuthURL = ""
		return nil
	})
	d.accountMu.Lock()
	defer d.accountMu.Unlock()
	defer a.cancel()
	if a.ceremony.State == "pending" {
		d.retireAccountAttemptLocked(a, "failed")
	}
	// Native rollback is finished before a prior ready credential becomes usable.
	if err != nil && !errors.Is(err, codexauth.ErrLoginCleanup) {
		d.restoreAccountAttemptLocked(a)
	} else if errors.Is(err, codexauth.ErrLoginCleanup) {
		_ = accounts.Update(func(s *accounts.State) error {
			p := s.Profiles[a.profile]
			if p.DeliveryEpoch == a.retiredEpoch {
				p.Error = "account_login_cleanup_failed"
				p.Revision++
				s.Profiles[p.ID] = p
			}
			return nil
		})
	}
}
func (d *Daemon) accountSnapshot(ctx context.Context) (hp.AccountSnapshot, error) {
	// Seed once, preserving legacy explicit modes. Status never exposes a login URL.
	if e := accounts.Update(func(*accounts.State) error { return nil }); e != nil {
		return hp.AccountSnapshot{}, e
	}
	s, e := accounts.Read()
	if e != nil {
		return hp.AccountSnapshot{}, e
	}
	for id, p := range s.Profiles {
		if p.Source != "central" || p.State == "logged-out" || p.State == "login-pending" || p.State == "blocked" {
			continue
		}
		p.Error = ""
		if p.Harness == "claude" {
			st, err := centralauth.ClaudeStore().Status()
			if err != nil {
				p.State = "blocked"
				p.Error = "credential_unavailable"
			} else if st.Configured {
				p.State = "ready"
			} else {
				p.State = "unconfigured"
			}
			p.Identity = hp.AccountIdentity{State: "unknown", Evidence: "opaque-token"}
		} else if c := d.centralCodex; c != nil && c.provider != nil {
			qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			acct, err := c.provider.Status(qctx)
			if err != nil {
				p.Error = "native_status_unavailable"
				p.Identity.State = "unavailable"
			} else if acct.Type == "chatgpt" {
				p.State = "ready"
				p.Identity = hp.AccountIdentity{State: "unknown", Email: acct.Email, Plan: acct.PlanType, Evidence: "native"}
				if t, err := c.provider.SessionToken(qctx, "account-status", false, ""); err == nil {
					p.Identity.AccountID = t.AccountID
					p.Identity.State = "known"
				}
			}
			if err == nil && acct.Type == "" {
				p.State = "unconfigured"
				p.Identity = hp.AccountIdentity{State: "unknown", Evidence: "native"}
			}
			limits, err := c.provider.RateLimits(qctx)
			if err == nil {
				p.Limits = limits
			} else {
				p.Limits = hp.AccountLimits{State: "unavailable", Source: "codex-native"}
			}
			cancel()
		} else {
			p.Error = "native_unavailable"
			p.Limits.State = "unavailable"
		}
		p.Limits.ProfileID = p.ID
		p.Limits.DeliveryEpoch = p.DeliveryEpoch
		s.Profiles[id] = p
	}
	// IO may overlap logout: only publish observations of the same epoch.
	e = accounts.Update(func(current *accounts.State) error {
		for id, p := range s.Profiles {
			q := current.Profiles[id]
			if q.DeliveryEpoch == p.DeliveryEpoch && q.Revision == p.Revision {
				if q.Identity.AccountID != "" && p.Identity.AccountID != "" && q.Identity.AccountID != p.Identity.AccountID {
					p.DeliveryEpoch++
					p.Revision++
					p.Limits.DeliveryEpoch = p.DeliveryEpoch
				}
				current.Profiles[id] = p
			}
		}
		s = *current
		return nil
	})
	return s.Snapshot(), e
}

// Accounts is the shared TUI/CLI/provider client API. Manage and ceremony use a
// query response so a result can carry typed, initiator-only data without logs.
func (c *Client) Accounts(r hp.AccountRequest, ceremony bool) (hp.AccountResult, error) {
	b, e := json.Marshal(r)
	if e != nil {
		return hp.AccountResult{}, e
	}
	q := core.QueryAccounts
	if ceremony {
		q = core.QueryAccountCeremony
	} else if r.Operation != hp.AccountStatus {
		q = core.QueryAccountManage
	}
	raw, e := c.queryAction(core.Action{Action: core.ActionQuery, Query: q, Fields: map[string]string{"request": string(b)}})
	if e != nil {
		return hp.AccountResult{}, e
	}
	var out hp.AccountResult
	e = json.Unmarshal(raw, &out)
	return out, e
}

// Keep strconv linked to the local action mapping: revision is decimal text.
func accountManageFields(a core.Action) (hp.AccountRequest, error) {
	r := hp.AccountRequest{Operation: a.Fields["operation"], ProfileID: a.Fields["profile_id"]}
	rev, e := strconv.ParseUint(a.Fields["revision"], 10, 64)
	r.Revision = rev
	fields := 3
	if r.Operation == hp.AccountDefault {
		fields = 4
		r.DefaultRevision, e = strconv.ParseUint(a.Fields["default_revision"], 10, 64)
	}
	if len(a.Fields) != fields || a.ID != "" || a.Target != "" {
		return r, errors.New("account_request_invalid")
	}
	return r, e
}

// disconnectAccounts retires all pending attempts even when the start response
// never reached its initiator. Prefix retirement is reserved for socket close.
func (d *Daemon) disconnectAccounts(initiator string, prefix bool) {
	d.accountMu.Lock()
	defer d.accountMu.Unlock()
	for id, a := range d.accountAttempts {
		if a.initiator != initiator && !(prefix && strings.HasPrefix(a.initiator, initiator)) {
			continue
		}
		d.retireAccountAttemptLocked(a, "cancelled")
		delete(d.accountAttempts, id)
	}
}

func (d *Daemon) accountTime() time.Time {
	if d.accountNow != nil {
		return d.accountNow()
	}
	return time.Now()
}
func (d *Daemon) expireAccountAttemptsLocked() {
	for id, a := range d.accountAttempts {
		if d.accountTime().UnixMilli() < a.ceremony.ExpiresAt {
			continue
		}
		if a.ceremony.State == "pending" {
			d.finishExpiredAccountLocked(a)
		} else {
			a.cancel()
			delete(d.accountAttempts, id)
		}
	}
}
func (d *Daemon) finishExpiredAccountLocked(a *accountAttempt) {
	d.retireAccountAttemptLocked(a, "failed")
}

func (d *Daemon) retireAccountAttemptLocked(a *accountAttempt, state string) {
	if a.ceremony.State != "pending" {
		return
	}
	a.cancel()
	a.ceremony.State = state
	a.ceremony.AuthURL = ""
	_ = accounts.Update(func(s *accounts.State) error {
		p := s.Profiles[a.profile]
		if p.DeliveryEpoch == a.epoch {
			p.DeliveryEpoch++
			p.Revision++
			p.State = "blocked"
			a.retiredEpoch = p.DeliveryEpoch
			s.Profiles[p.ID] = p
		}
		return nil
	})
	if a.prior.Harness == "claude" {
		d.restoreAccountAttemptLocked(a)
	}
}
func (d *Daemon) restoreAccountAttemptLocked(a *accountAttempt) {
	if a.prior.State != "ready" || a.retiredEpoch == 0 {
		return
	}
	_ = accounts.Update(func(s *accounts.State) error {
		p := s.Profiles[a.profile]
		if p.DeliveryEpoch != a.retiredEpoch || p.State != "blocked" {
			return nil
		}
		prior := a.prior
		prior.DeliveryEpoch = p.DeliveryEpoch
		prior.Revision = p.Revision + 1
		prior.Limits = hp.AccountLimits{State: "unavailable", Source: prior.Limits.Source}
		s.Profiles[p.ID] = prior
		return nil
	})
}
