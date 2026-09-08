package daemon

import (
	"context"
	"fmt"
	"time"

	"amux/internal/core"
	"amux/internal/engine"
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
		newID, err := wsops.Dispatch(ctx, a, d.killEngineFor)
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
	baseline, err := d.permissionBaseline(id)
	if err != nil {
		return fmt.Errorf("capture permission boundary: %w", err)
	}
	if d.structuredControl(spec.Session) {
		dir, env, argv, endpoint, err := panespec.AppServerCommand(spec)
		if err != nil {
			return err
		}
		d.killRuntimeFor(id)
		session := spec.Session
		supervisor, err := d.codex.Ensure(id, dir, env, argv, endpoint, session.Model, session.Prompt, session.ClaudeID)
		if err != nil {
			return err
		}
		_, err = d.permissions.observeExcluding(id, supervisor, baseline)
		return err
	}
	if d.engine == nil {
		return fmt.Errorf("engine unavailable")
	}
	dir, env, argv, err := d.resolve(spec, panespec.TabAgent)
	if err != nil {
		return err
	}
	d.killRuntimeFor(id)
	instance, err := d.engine.Ensure(ctx, engine.Spec{
		Key: engine.Key{AgentID: id, Tab: panespec.TabAgent}, Dir: dir, Env: env, Argv: argv,
	})
	if err != nil {
		return err
	}
	_, err = d.permissions.observeExcluding(id, instance, baseline)
	return err
}

func ok() core.Result { return core.Result{Type: "result", OK: true} }

func fail(format string, args ...any) core.Result {
	return core.Result{Type: "result", OK: false, Error: fmt.Sprintf(format, args...)}
}
