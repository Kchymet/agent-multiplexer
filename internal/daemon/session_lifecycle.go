package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"amux/internal/access"
	"amux/internal/console"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

const (
	sessionMailboxPoll     = 50 * time.Millisecond
	credentialRenewBefore  = 7 * 24 * time.Hour
	completionReceiptGrace = 3 * time.Second
	completionAbandonGrace = sessionrpc.MaxReceiptGrace
	completionMinimumGrace = 500 * time.Millisecond
	completionRuntimeGrace = 10 * time.Second
	completionActivityPoll = 50 * time.Millisecond
)

type sessionRuntime struct {
	d           *Daemon
	resolver    *daemonAccessResolver
	policy      access.Policy
	poll        time.Duration
	now         func() time.Time
	dispatchMu  sync.Mutex
	servers     map[string]*sessionrpc.Server
	completions *completionRegistry
}

func newSessionRuntime(d *Daemon) *sessionRuntime {
	r := &sessionRuntime{
		d: d, resolver: newDaemonAccessResolver(), poll: sessionMailboxPoll,
		now: time.Now, servers: make(map[string]*sessionrpc.Server),
	}
	r.policy = access.Policy{Resolver: r.resolver}
	r.completions = newCompletionRegistry(d)
	return r
}

func (r *sessionRuntime) start(ctx context.Context) error {
	return r.reconcile(ctx)
}

func (r *sessionRuntime) run(ctx context.Context) {
	ticker := time.NewTicker(r.poll)
	defer ticker.Stop()
	for {
		if err := r.serve(ctx); err != nil && ctx.Err() == nil {
			log.Printf("session RPC: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *sessionRuntime) serve(ctx context.Context) error {
	if err := r.reconcile(ctx); err != nil {
		return err
	}
	ids := make([]string, 0, len(r.servers))
	for id := range r.servers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, err := r.servers[id].ServeOnce(ctx); err != nil && !errors.Is(err, sessionrpc.ErrQueueFull) && !errors.Is(err, sessionrpc.ErrClosed) {
			log.Printf("session RPC %s: %v", id, err)
		}
	}
	return nil
}

func (r *sessionRuntime) reconcile(ctx context.Context) error {
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	if err := r.d.ensureHostCredential(ctx, r.now()); err != nil {
		return fmt.Errorf("renew host credential: %w", err)
	}
	db, err := store.Open()
	if err != nil {
		return err
	}
	sessions, err := db.AllSessions()
	closeErr := db.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	sessions = append(sessions, console.Session())
	current := make(map[string]store.Session, len(sessions))
	for _, session := range sessions {
		current[session.ID] = session
		if session.Archived && !r.completions.has(session.ID) {
			r.closeAndRevoke(ctx, session.ID)
			continue
		}
		if session.Archived { // pending completion retains only its existing server/key
			continue
		}
		if _, ok := r.servers[session.ID]; ok {
			r.renewCurrent(ctx, session)
			continue
		}
		if err := r.open(ctx, session); err != nil {
			log.Printf("session RPC %s unavailable: %v", session.ID, err)
		}
	}
	for id := range r.servers {
		if _, ok := current[id]; !ok && !r.completions.has(id) {
			r.closeAndRevoke(ctx, id)
		}
	}
	return nil
}

func (d *Daemon) ensureHostCredential(ctx context.Context, now time.Time) error {
	credential, err := access.LoadCredential(d.authority.CredentialDir(access.SubjectHost, access.LocalHostSubject))
	if errors.Is(err, os.ErrNotExist) {
		_, err = d.authority.EnsureHost(ctx)
		return err
	}
	if err != nil {
		return err
	}
	if err := d.authority.Valid(ctx, credentialPrincipal(credential)); err != nil {
		return err // an explicitly revoked or expired host is never auto-reissued
	}
	if now.Add(credentialRenewBefore).Before(time.UnixMilli(credential.NotAfter)) {
		return nil
	}
	_, err = d.authority.Rotate(ctx, access.SubjectHost, access.LocalHostSubject)
	return err
}

func (r *sessionRuntime) open(ctx context.Context, session store.Session) error {
	if err := r.validateOrRenewCredential(ctx, session); err != nil {
		return err
	}
	spec, err := r.d.launchSpec(ctx, session.ID)
	if err != nil {
		return err
	}
	server, err := sessionrpc.OpenServerMailbox(session.ID, spec.Access.MailboxHostDir, r.d.authority, r.d.authority,
		sessionrpc.Callbacks{Authorize: r.authorize, Dispatch: r.dispatch}, sessionrpc.ServerOptions{})
	if err != nil {
		return err
	}
	if err := server.PublishService(ctx); err != nil {
		_ = server.Close()
		return err
	}
	r.servers[session.ID] = server
	return nil
}

func (r *sessionRuntime) renewCurrent(ctx context.Context, session store.Session) {
	if err := r.validateOrRenewCredential(ctx, session); err != nil {
		log.Printf("session RPC %s credential: %v", session.ID, err)
		if credential, loadErr := r.loadCredential(session.ID); loadErr == nil {
			principal := credentialPrincipal(credential)
			if r.d.authority.Valid(ctx, principal) != nil {
				r.closeServer(session.ID)
			}
		}
	}
}

func (r *sessionRuntime) validateOrRenewCredential(ctx context.Context, session store.Session) error {
	credential, err := r.loadCredential(session.ID)
	if errors.Is(err, os.ErrNotExist) {
		return nil // launchSpec performs first provisioning
	}
	if err != nil {
		return err
	}
	if err := r.d.authority.Valid(ctx, credentialPrincipal(credential)); err != nil {
		return err // revoked/expired is never silently re-issued
	}
	if r.now().Add(credentialRenewBefore).Before(time.UnixMilli(credential.NotAfter)) {
		return nil
	}
	resource, ok, err := r.resolver.Lookup(ctx, session.ID)
	if err != nil || !ok || resource.Archived {
		if err != nil {
			return err
		}
		return access.ErrDenied
	}
	_, err = r.d.authority.Rotate(ctx, access.SubjectSession, session.ID)
	return err
}

func (r *sessionRuntime) loadCredential(subjectID string) (access.Credential, error) {
	return access.LoadCredential(r.d.authority.CredentialDir(access.SubjectSession, subjectID))
}

func credentialPrincipal(credential access.Credential) access.Principal {
	return access.Principal{KeyID: credential.KeyID, SubjectID: credential.SubjectID, Kind: credential.Kind, Generation: credential.Generation}
}

func (r *sessionRuntime) closeServer(id string) {
	if server := r.servers[id]; server != nil {
		_ = server.Close()
		delete(r.servers, id)
	}
}

func (r *sessionRuntime) closeAndRevoke(ctx context.Context, id string) {
	r.closeServer(id)
	if err := r.d.authority.Revoke(ctx, access.SubjectSession, id); err != nil {
		log.Printf("session RPC %s revoke: %v", id, err)
	}
}

func (r *sessionRuntime) close() {
	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	for id := range r.servers {
		r.closeServer(id)
	}
}

type completionRegistry struct {
	mu      sync.Mutex
	d       *Daemon
	entries map[string]*completionEntry
	receipt time.Duration
	abandon time.Duration
	minimum time.Duration
	runtime time.Duration
	poll    time.Duration
}

type completionEntry struct {
	principal access.Principal
	requestID string
	digest    string
	settling  bool
}

func newCompletionRegistry(d *Daemon) *completionRegistry {
	return &completionRegistry{
		d: d, entries: make(map[string]*completionEntry), receipt: completionReceiptGrace,
		abandon: completionAbandonGrace, minimum: completionMinimumGrace,
		runtime: completionRuntimeGrace, poll: completionActivityPoll,
	}
}

func (c *completionRegistry) begin(principal access.Principal, requestID, subjectID string) *sessionrpc.ReceiptHooks {
	entry := &completionEntry{principal: principal, requestID: requestID}
	c.mu.Lock()
	c.entries[subjectID] = entry
	c.mu.Unlock()
	time.AfterFunc(c.abandon, func() { c.settle(subjectID, requestID) })
	return &sessionrpc.ReceiptHooks{
		Grace: c.receipt,
		ResponsePersisted: func(p sessionrpc.PersistedResponse) {
			c.mu.Lock()
			if current := c.entries[subjectID]; current == entry && current.requestID == p.RequestID && !current.settling {
				current.digest = p.ResponseDigest
			}
			c.mu.Unlock()
		},
		Settled: func(s sessionrpc.ReceiptSettlement) { c.settle(subjectID, s.RequestID) },
	}
}

func (c *completionRegistry) authorizeReceipt(principal access.Principal, receipt sessionrpc.Receipt) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[principal.SubjectID]
	return entry != nil && !entry.settling && entry.requestID == receipt.RequestID && entry.digest != "" && entry.digest == receipt.ResponseDigest &&
		entry.principal.KeyID == principal.KeyID && entry.principal.Generation == principal.Generation && entry.principal.Kind == principal.Kind
}

func (c *completionRegistry) has(subjectID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[subjectID] != nil
}

func (c *completionRegistry) settle(subjectID, requestID string) {
	c.mu.Lock()
	entry := c.entries[subjectID]
	if entry == nil || entry.requestID != requestID || entry.settling {
		c.mu.Unlock()
		return
	}
	entry.settling = true
	c.mu.Unlock()
	go c.finish(subjectID, entry)
}

func (c *completionRegistry) finish(subjectID string, entry *completionEntry) {
	minimum := time.NewTimer(c.minimum)
	<-minimum.C
	deadline := time.NewTimer(c.runtime)
	defer deadline.Stop()
	ticker := time.NewTicker(c.poll)
	defer ticker.Stop()
	for c.runtimeLive(subjectID) && c.d.instanceActivity(engine.Key{AgentID: subjectID, Tab: panespec.TabAgent}) != engine.ActivitySafe {
		select {
		case <-deadline.C:
			goto stop
		case <-ticker.C:
		}
	}
stop:
	c.d.killEngineFor(subjectID)
	if c.d.authority != nil {
		_ = c.d.authority.Revoke(context.Background(), access.SubjectSession, subjectID)
	}
	c.mu.Lock()
	if c.entries[subjectID] == entry {
		delete(c.entries, subjectID)
	}
	c.mu.Unlock()
}

func (c *completionRegistry) runtimeLive(subjectID string) bool {
	if c.d.engine != nil {
		if instance, ok := c.d.engine.Lookup(engine.Key{AgentID: subjectID, Tab: panespec.TabAgent}); ok && instance.Alive() {
			return true
		}
	}
	if c.d.codex != nil {
		_, ok := c.d.codex.Get(subjectID)
		return ok
	}
	return false
}
