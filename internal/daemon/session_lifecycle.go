package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"sync"
	"time"

	"amux/internal/access"
	"amux/internal/console"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
	"amux/internal/sessionrpc"
	"amux/internal/store"
	"amux/internal/wsops"
)

const (
	sessionMailboxPoll     = 50 * time.Millisecond
	credentialRenewBefore  = 7 * 24 * time.Hour
	completionReceiptGrace = 3 * time.Second
	completionAbandonGrace = sessionrpc.MaxReceiptGrace
	completionMinimumGrace = 500 * time.Millisecond
	completionRuntimeGrace = 10 * time.Second
	completionActivityPoll = 50 * time.Millisecond
	sessionCallbackTimeout = 10 * time.Minute
)

type sessionRuntime struct {
	d               *Daemon
	resolver        *daemonAccessResolver
	policy          access.Policy
	applyResult     func(context.Context, core.Action) (string, error)
	encodeResult    func(core.Result) ([]byte, error)
	poll            time.Duration
	now             func() time.Time
	callbackTimeout time.Duration
	dispatchMu      sync.Mutex
	servers         map[string]*sessionrpc.Server
	completions     *completionRegistry
	responseBudget  sessionrpc.ResponseBudget
}

func newSessionRuntime(d *Daemon) *sessionRuntime {
	r := &sessionRuntime{
		d: d, resolver: newDaemonAccessResolver(), poll: sessionMailboxPoll,
		now: time.Now, callbackTimeout: sessionCallbackTimeout,
		servers:        make(map[string]*sessionrpc.Server),
		responseBudget: newSessionResponseBudget(),
		applyResult:    wsops.ApplyResult,
		encodeResult:   func(result core.Result) ([]byte, error) { return json.Marshal(result) },
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
		sessionrpc.Callbacks{Authorize: r.authorizeCallback, Dispatch: r.dispatchCallback},
		sessionrpc.ServerOptions{ResponseBudget: r.responseBudget})
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

// authorizeCallback and dispatchCallback are the only transport callback
// entrypoints. They never let an integration panic take down mailbox service,
// and they give every cooperative DB/process operation a fixed upper deadline.
// The callbacks stay synchronous: on timeout no detached mutation is left
// running and an accepted/uncertain effect is never retried by this boundary.
func (r *sessionRuntime) authorizeCallback(ctx context.Context, principal access.Principal, call sessionrpc.Call) (err error) {
	ctx, cancel := context.WithTimeout(ctx, r.callbackTimeout)
	defer cancel()
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("session authorization callback failed")
		}
	}()
	return r.authorize(ctx, principal, call)
}

func (r *sessionRuntime) dispatchCallback(ctx context.Context, request sessionrpc.DispatchRequest) (result sessionrpc.DispatchResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, r.callbackTimeout)
	defer cancel()
	defer func() {
		if recover() != nil {
			result = rpcFailed("callback_failed")
			err = nil
		}
	}()
	return r.dispatch(ctx, request)
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
	r.completions.close()
}

type completionRegistry struct {
	mu      sync.Mutex
	d       *Daemon
	entries map[string]*completionEntry
	closed  bool
	wg      sync.WaitGroup
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
	runtime   permissionRuntimeToken
	settling  bool
	cancelled bool
	settle    chan struct{}
	cancel    chan struct{}
	done      chan struct{}
}

func newCompletionRegistry(d *Daemon) *completionRegistry {
	return &completionRegistry{
		d: d, entries: make(map[string]*completionEntry), receipt: completionReceiptGrace,
		abandon: completionAbandonGrace, minimum: completionMinimumGrace,
		runtime: completionRuntimeGrace, poll: completionActivityPoll,
	}
}

func (c *completionRegistry) begin(principal access.Principal, requestID, subjectID string) *sessionrpc.ReceiptHooks {
	runtime, _ := c.d.permissions.token(subjectID)
	entry := &completionEntry{
		principal: principal, requestID: requestID, runtime: runtime,
		settle: make(chan struct{}), cancel: make(chan struct{}), done: make(chan struct{}),
	}
	c.mu.Lock()
	if !c.closed {
		if previous := c.entries[subjectID]; previous != nil {
			c.cancelEntryLocked(subjectID, previous)
		}
		c.entries[subjectID] = entry
		c.wg.Add(1)
		go c.run(subjectID, entry)
	}
	c.mu.Unlock()
	hooks := &sessionrpc.ReceiptHooks{
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
	return nonPanickingReceiptHooks(hooks)
}

func nonPanickingReceiptHooks(hooks *sessionrpc.ReceiptHooks) *sessionrpc.ReceiptHooks {
	if hooks == nil {
		return nil
	}
	return &sessionrpc.ReceiptHooks{
		Grace: hooks.Grace,
		ResponsePersisted: func(response sessionrpc.PersistedResponse) {
			defer func() { _ = recover() }()
			hooks.ResponsePersisted(response)
		},
		Settled: func(settlement sessionrpc.ReceiptSettlement) {
			defer func() { _ = recover() }()
			hooks.Settled(settlement)
		},
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
	if entry == nil || entry.requestID != requestID || entry.settling || entry.cancelled {
		c.mu.Unlock()
		return
	}
	entry.settling = true
	close(entry.settle)
	c.mu.Unlock()
}

func (c *completionRegistry) run(subjectID string, entry *completionEntry) {
	defer c.wg.Done()
	defer close(entry.done)
	abandon := time.NewTimer(c.abandon)
	defer abandon.Stop()
	select {
	case <-entry.settle:
	case <-abandon.C:
		c.mu.Lock()
		if current := c.entries[subjectID]; current != entry || entry.cancelled {
			c.mu.Unlock()
			return
		}
		entry.settling = true
		c.mu.Unlock()
	case <-entry.cancel:
		return
	}
	c.finish(subjectID, entry)
}

func (c *completionRegistry) finish(subjectID string, entry *completionEntry) {
	defer func() {
		c.mu.Lock()
		if c.entries[subjectID] == entry {
			delete(c.entries, subjectID)
		}
		c.mu.Unlock()
	}()
	minimum := time.NewTimer(c.minimum)
	defer minimum.Stop()
	select {
	case <-minimum.C:
	case <-entry.cancel:
		return
	}
	deadline := time.NewTimer(c.runtime)
	defer deadline.Stop()
	ticker := time.NewTicker(c.poll)
	defer ticker.Stop()
	for c.runtimeLive(subjectID, entry.runtime) && c.d.instanceActivity(engine.Key{AgentID: subjectID, Tab: panespec.TabAgent}) != engine.ActivitySafe {
		select {
		case <-deadline.C:
			goto stop
		case <-ticker.C:
		case <-entry.cancel:
			return
		}
	}
stop:
	if !c.owns(subjectID, entry) || !c.archived(subjectID) {
		return
	}
	if !c.d.killRuntimeToken(subjectID, entry.runtime) {
		return
	}
	// Revoke only the credential generation that authorized completion. The
	// explicit restore API will supersede/cancel this entry before issuing a new
	// generation; arbitrary reconciliation can never regrant it.
	if c.d.authority != nil {
		_ = c.d.authority.RevokeCurrent(context.Background(), entry.principal)
	}
}

func (c *completionRegistry) owns(subjectID string, entry *completionEntry) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed && !entry.cancelled && c.entries[subjectID] == entry
}

func (c *completionRegistry) archived(subjectID string) bool {
	session, ok, err := lookupSession(subjectID)
	return err == nil && ok && session.Archived
}

func (c *completionRegistry) runtimeLive(subjectID string, token permissionRuntimeToken) bool {
	if !c.d.permissions.matches(subjectID, token) {
		return false
	}
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

func (c *completionRegistry) cancel(subjectID string) {
	c.mu.Lock()
	if entry := c.entries[subjectID]; entry != nil {
		c.cancelEntryLocked(subjectID, entry)
	}
	c.mu.Unlock()
}

// cancelAndWait is the authenticated restore boundary. It prevents a stale
// completion from crossing the restore/regrant transition after it has already
// passed its final ownership check.
func (c *completionRegistry) cancelAndWait(subjectID string) {
	c.mu.Lock()
	entry := c.entries[subjectID]
	if entry != nil {
		c.cancelEntryLocked(subjectID, entry)
	}
	c.mu.Unlock()
	if entry != nil {
		<-entry.done
	}
}

func (c *completionRegistry) cancelEntryLocked(subjectID string, entry *completionEntry) {
	if entry.cancelled {
		return
	}
	entry.cancelled = true
	if c.entries[subjectID] == entry {
		delete(c.entries, subjectID)
	}
	close(entry.cancel)
}

func (c *completionRegistry) close() {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		for subjectID, entry := range c.entries {
			c.cancelEntryLocked(subjectID, entry)
		}
	}
	c.mu.Unlock()
	c.wg.Wait()
}
