package daemon

import (
	"context"
	"fmt"

	"amux/internal/core"
	"amux/internal/wsops"
)

// handle executes a control action and returns a Result. State-changing actions
// share wsops.Apply with the multiplexer server and CLI; refresh just re-polls;
// start and steer are engine-only (no store change) and served here.
func (d *Daemon) handle(ctx context.Context, a core.Action) core.Result {
	switch a.Action {
	case "", core.ActionRefresh:
		d.triggerPoll()
		return ok()
	case core.ActionStart:
		if err := d.startEngineFor(ctx, a.ID); err != nil {
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
		if desc := core.DescriptorFor(a.Action); desc.CreatesSession && desc.TargetsRoot && newID != "" {
			// The root is the coordinator's session. Start it at creation even when
			// the client opens a member instead, or never attaches a UI at all.
			// startEngineFor would start the members, leaving the coordinator idle.
			if err := d.startAgent(ctx, newID); err != nil {
				r.OK = false
				r.Error = fmt.Sprintf("workgroup %s created, but coordinator failed to start: %v; open the workgroup to retry", newID, err)
			}
			d.triggerPoll()
		}
		return r
	}
}

func ok() core.Result { return core.Result{Type: "result", OK: true} }

func fail(format string, args ...any) core.Result {
	return core.Result{Type: "result", OK: false, Error: fmt.Sprintf(format, args...)}
}
