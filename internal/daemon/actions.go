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
	case "", "refresh":
		d.triggerPoll()
		return ok()
	case core.ActionStart:
		if err := d.startEngineFor(ctx, a.ID); err != nil {
			return fail("%v", err)
		}
		d.triggerPoll()
		return ok()
	default:
		// Capture the agents to stop before the store record disappears (for a
		// root delete, killEngineFor reads the children from the current snapshot).
		switch a.Action {
		case "delete", "kill", "archive":
			d.killEngineFor(a.ID)
		}
		newID, err := wsops.ApplyResult(ctx, a)
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
