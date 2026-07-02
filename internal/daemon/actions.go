package daemon

import (
	"context"
	"fmt"

	"amux/internal/core"
	"amux/internal/wsops"
)

// handle executes a control action and returns a Result. State-changing actions
// share wsops.Apply with the multiplexer server and CLI; refresh just re-polls.
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
		return r
	}
}

func ok() core.Result { return core.Result{Type: "result", OK: true} }

func fail(format string, args ...any) core.Result {
	return core.Result{Type: "result", OK: false, Error: fmt.Sprintf(format, args...)}
}
