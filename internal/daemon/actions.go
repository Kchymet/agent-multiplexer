package daemon

import (
	"context"
	"fmt"

	"amux/internal/core"
	"amux/internal/wsops"
)

// handle executes a control action and returns a Result. The rail drives the
// workspace lifecycle: open (start/switch) and delete.
func (d *Daemon) handle(ctx context.Context, a core.Action) core.Result {
	switch a.Action {
	case "", "refresh":
		d.triggerPoll()
		return ok()
	case "open", "attach":
		if err := wsops.OpenByID(ctx, a.ID); err != nil {
			return fail("%v", err)
		}
		d.triggerPoll()
		return ok()
	case "delete", "kill":
		if err := wsops.DeleteByID(ctx, a.ID); err != nil {
			return fail("%v", err)
		}
		d.triggerPoll()
		return ok()
	case "new-repo-agent": // a.ID is the repo name
		if _, err := wsops.CreateRepoWorkgroup(ctx, a.ID, wsops.AgentSpec{}); err != nil {
			return fail("%v", err)
		}
		d.triggerPoll()
		return ok()
	case "move": // a.ID is the agent; a.Target is the destination root ("" = new work-scoped)
		if err := wsops.MoveAgent(ctx, a.ID, a.Target); err != nil {
			return fail("%v", err)
		}
		d.triggerPoll()
		return ok()
	default:
		return fail("unknown action %q", a.Action)
	}
}

func ok() core.Result { return core.Result{Type: "result", OK: true} }

func fail(format string, args ...any) core.Result {
	return core.Result{Type: "result", OK: false, Error: fmt.Sprintf(format, args...)}
}
