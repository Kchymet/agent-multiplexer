package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"amux/internal/access"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/launchenv"
	"amux/internal/panespec"
	"amux/internal/wsops"
)

// actionDaemonShutdown is intentionally absent from core.ControlActions. It is
// a protected operator operation accepted only on the TLS-authenticated host
// stream; restricted session RPC has a closed vocabulary that excludes it.
const actionDaemonShutdown = "daemon.shutdown"
const actionSessionRecreate = "session.recreate"

const shutdownResponseGrace = 250 * time.Millisecond

// handle executes a control action and returns a Result. State-changing actions
// share wsops.Apply with the multiplexer server and CLI; refresh just re-polls;
// start and steer are engine-only (no store change) and served here.
func (d *Daemon) handle(ctx context.Context, a core.Action) core.Result {
	if !effectAdmissionHeld(ctx) {
		// Use the same lock order as mailbox serving: dispatch ownership, then
		// effect admission. For an explicit restore, drain the old completion
		// owner before taking effectMu; its cleanup may already be waiting there.
		// Holding dispatchMu prevents another old-principal call entering between
		// that drain and the final admission lock.
		if d.sessionRPC != nil {
			d.sessionRPC.dispatchMu.Lock()
			defer d.sessionRPC.dispatchMu.Unlock()
			if restore, _ := hostRestoreRequested(a); restore {
				d.sessionRPC.completions.cancelAndWait(a.ID)
			}
		}
		d.effectMu.Lock()
		defer d.effectMu.Unlock()
		ctx = withEffectAdmission(ctx)
	}
	if err := revalidateDeferred(ctx); err != nil {
		return fail("authorization changed before execution: %v", err)
	}
	switch a.Action {
	case actionSessionRecreate:
		if a.ID == "" || a.Kind != "" || a.Cwd != "" || a.Target != "" || a.Query != "" ||
			len(a.Fields) != 0 || a.PaneID != "" || a.Tab != 0 || a.Cols != 0 || a.Rows != 0 || len(a.Data) != 0 {
			return fail("session recreation accepts exactly one session id")
		}
		if err := d.recreateSession(ctx, a.ID); err != nil {
			return fail("%v", err)
		}
		d.triggerPoll()
		return ok()
	case actionDaemonShutdown:
		if a.ID != "" || a.Kind != "" || a.Cwd != "" || a.Target != "" || a.Query != "" ||
			len(a.Fields) != 0 || a.PaneID != "" || a.Tab != 0 || a.Cols != 0 || a.Rows != 0 || len(a.Data) != 0 {
			return fail("daemon shutdown accepts no arguments")
		}
		// connState writes the successful result asynchronously. Give the
		// authenticated caller a bounded opportunity to observe it before Run
		// tears down the authority and listener.
		// Acceptance is final once this result is returned. The client normally
		// closes immediately after observing it, so its connection context must
		// not cancel the already-authorized shutdown during the response grace.
		time.AfterFunc(shutdownResponseGrace, d.requestShutdown)
		return ok()
	case "", core.ActionRefresh:
		d.triggerPoll()
		return ok()
	case core.ActionStart:
		if err := d.startEngineFor(ctx, a.ID); err != nil {
			return fail("%v", err)
		}
		d.triggerPoll()
		return ok()
	case core.ActionAuthReload:
		if err := d.queueAuthReload(a); err != nil {
			return fail("%v", err)
		}
		d.triggerPoll()
		return ok()
	case core.ActionSteer:
		// Engine-only like start: it drives the agent inside a running session and
		// changes no store state, so it never reaches wsops.
		if err := d.steer(ctx, a); err != nil {
			return fail("%v", err)
		}
		return ok()
	default:
		// One descriptor-driven path: Dispatch stops the engine (via killEngineFor)
		// for the verbs whose descriptor says so — including set-archived, which the
		// old inline switch missed, so the CLI's archive left the process running
		// while the TUI's stopped it. killEngineFor runs before the store mutation so
		// a root delete still reads its children from the pre-deletion snapshot.
		var newID string
		var err error
		if restore, restoreErr := hostRestoreRequested(a); restoreErr != nil {
			return fail("%v", restoreErr)
		} else if restore {
			newID, err = d.restoreSessionAccess(ctx, a)
		} else {
			newID, err = wsops.Dispatch(ctx, a, d.killEngineFor)
		}
		if err != nil {
			return fail("%v", err)
		}
		d.triggerPoll()
		r := ok()
		r.NewID = newID
		if desc := core.DescriptorFor(a.Action); desc.CreatesSession && newID != "" {
			// Start the exact session returned by every creation verb. The native TUI
			// also starts a new session by attaching to it, but remote/headless clients
			// do not necessarily attach after creation. In particular, an add-agent
			// prompt must begin running without waiting for the user to switch to it.
			// For a root, the returned id is the coordinator itself; startAgent avoids
			// startEngineFor's root fan-out to the members.
			if err := d.startAgent(ctx, newID); err != nil {
				kind := "agent"
				if desc.TargetsRoot {
					kind = "workgroup"
				}
				r.OK = false
				r.Error = fmt.Sprintf("%s %s created, but failed to start: %v; open it to retry", kind, newID, err)
			}
			d.triggerPoll()
		}
		return r
	}
}

func hostRestoreRequested(action core.Action) (bool, error) {
	switch action.Action {
	case core.ActionSetArchived:
		return action.Fields["archived"] == "false", nil
	case core.ActionArchive:
		session, found, err := lookupSession(action.ID)
		if err != nil {
			return false, err
		}
		if !found {
			return false, fmt.Errorf("session %s not found", action.ID)
		}
		return session.Archived, nil
	default:
		return false, nil
	}
}

// restoreSessionAccess is reached only through the authenticated host stream.
// It drains any completion owner before changing archive state, then explicitly
// regrants exactly the latest revoked credential generation. A publication
// error is resolved only by observing the expected committed generation; the
// mutation is never retried automatically.
func (d *Daemon) restoreSessionAccess(ctx context.Context, action core.Action) (string, error) {
	session, found, err := lookupSession(action.ID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("session %s not found", action.ID)
	}
	if !session.Archived {
		return wsops.Dispatch(ctx, action, d.killEngineFor)
	}
	expected, regrant, err := d.restoreCredentialState(ctx, action.ID)
	if err != nil {
		return "", err
	}
	var newID string
	d.authMu.Lock()
	d.permissions.retireAnd(action.ID, func() {
		if err = d.quiesceSessionRuntimeLocked(action.ID); err != nil {
			return
		}
		var applyErr error
		newID, applyErr = wsops.Dispatch(ctx, action, nil)
		if applyErr != nil {
			current, currentFound, lookupErr := lookupSession(action.ID)
			if lookupErr != nil || !currentFound || current.Archived {
				err = applyErr
				return
			}
		}
		if regrant {
			if _, regrantErr := d.authority.Regrant(ctx, access.SubjectSession, action.ID, expected); regrantErr != nil {
				current, currentErr := d.authority.Current(ctx, access.SubjectSession, action.ID)
				if currentErr != nil || current.Generation != expected+1 {
					err = fmt.Errorf("restore credential regrant: %w", regrantErr)
				}
			}
		}
	})
	d.authMu.Unlock()
	if err != nil {
		return "", err
	}
	return newID, nil
}

// quiesceSessionRuntimeLocked terminates every process that could retain this
// session's stable credential-directory mount. Engine.Kill and Supervisor.Close
// are synchronous: their production implementations return only after the
// launched process has been reaped. We additionally verify the exact captured
// handles are no longer live before a caller may publish a successor key.
// d.authMu and the permission gate are held by the caller.
func (d *Daemon) quiesceSessionRuntimeLocked(subjectID string) error {
	delete(d.authPending, engine.Key{AgentID: subjectID, Tab: panespec.TabAgent})
	var instances []engine.Instance
	if d.engine != nil {
		for tab := 0; tab < 3; tab++ {
			key := engine.Key{AgentID: subjectID, Tab: tab}
			if instance, ok := d.engine.Lookup(key); ok {
				instances = append(instances, instance)
			}
			d.engine.Kill(key)
		}
	}
	var supervisor *codexapp.Supervisor
	if d.codex != nil {
		supervisor, _ = d.codex.Get(subjectID)
		d.codex.Close(subjectID)
	}
	for _, instance := range instances {
		if instance.Alive() {
			return fmt.Errorf("session %s runtime did not terminate", subjectID)
		}
		if current, ok := d.engine.Lookup(instance.Key()); ok && current == instance {
			return fmt.Errorf("session %s runtime remains published", subjectID)
		}
	}
	if supervisor != nil {
		if current, ok := d.codex.Get(subjectID); ok && current == supervisor {
			return fmt.Errorf("session %s App Server did not terminate", subjectID)
		}
	}
	return nil
}

func (d *Daemon) restoreCredentialState(ctx context.Context, subjectID string) (uint64, bool, error) {
	if d.authority == nil {
		return 0, false, fmt.Errorf("credential authority unavailable")
	}
	if _, err := d.authority.Current(ctx, access.SubjectSession, subjectID); err == nil {
		return 0, false, nil
	} else if errors.Is(err, access.ErrNotProvisioned) {
		return 0, false, nil
	} else if errors.Is(err, access.ErrExpired) {
		credential, loadErr := access.LoadCredential(d.authority.CredentialDir(access.SubjectSession, subjectID))
		if loadErr != nil {
			return 0, false, loadErr
		}
		if revokeErr := d.authority.RevokeCurrent(ctx, credentialPrincipal(credential)); revokeErr != nil {
			return 0, false, revokeErr
		}
	} else if !errors.Is(err, access.ErrRevoked) {
		return 0, false, err
	}
	revoked, err := d.authority.LastRevoked(ctx, access.SubjectSession, subjectID)
	if err != nil {
		return 0, false, err
	}
	return revoked.Generation, true, nil
}

// recreateSession is the explicit compatibility boundary for a process that
// predates fixed session access mounts. It validates the complete replacement
// launch before stopping the old runtime, touches no worktree/config/transcript
// files, and replaces only the named runtime (a coordinator does not cascade to
// its members). The socket transport admitting this verb is host-only.
func (d *Daemon) recreateSession(ctx context.Context, id string) error {
	spec, err := d.launchSpec(ctx, id)
	if err != nil {
		return err
	}
	if d.structuredControl(spec.Session) {
		dir, env, argv, endpoint, err := panespec.AppServerCommand(spec)
		if err != nil {
			return err
		}
		if err := revalidateDeferred(ctx); err != nil {
			return err
		}
		d.killRuntimeFor(id)
		session := spec.Session
		published, _, err := d.publishPermissionRuntime(id, func() (any, error) {
			return d.codex.Ensure(id, dir, env, argv, endpoint, session.Model, session.Prompt, session.ClaudeID)
		})
		if err != nil {
			return err
		}
		if _, ok := published.(*codexapp.Supervisor); !ok {
			return fmt.Errorf("App Server returned no runtime")
		}
		return nil
	}
	if d.engine == nil {
		return fmt.Errorf("engine unavailable")
	}
	dir, env, argv, err := d.resolve(spec, panespec.TabAgent)
	if err != nil {
		return err
	}
	if err := revalidateDeferred(ctx); err != nil {
		return err
	}
	d.killRuntimeFor(id)
	published, _, err := d.publishPermissionRuntime(id, func() (any, error) {
		return d.engine.Ensure(ctx, engine.Spec{
			Key: engine.Key{AgentID: id, Tab: panespec.TabAgent}, Dir: dir, Env: env,
			ModelAccess: launchenv.ForRuntime(spec.Session.Agent), Argv: argv,
		})
	})
	if err != nil {
		return err
	}
	if _, ok := published.(engine.Instance); !ok {
		return fmt.Errorf("engine returned no runtime")
	}
	return nil
}

func ok() core.Result { return core.Result{Type: "result", OK: true} }

func fail(format string, args ...any) core.Result {
	return core.Result{Type: "result", OK: false, Error: fmt.Sprintf(format, args...)}
}
