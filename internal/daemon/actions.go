package daemon

import (
	"context"
	"fmt"
	"strings"

	"amux/internal/core"
	"amux/internal/store"
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
	case "new-repo-agent": // a.ID is the repo name; a.Fields carries the settings form
		if _, err := wsops.CreateRepoWorkgroup(ctx, a.ID, wsops.AgentSpec{
			Prompt: a.Fields["prompt"], Mode: a.Fields["mode"], Model: a.Fields["model"],
		}); err != nil {
			return fail("%v", err)
		}
		d.triggerPoll()
		return ok()
	case "new-workgroup": // a.Fields: name, repos (comma), prompt/description, linear
		repos := store.SplitRepos(a.Fields["repos"])
		prompt := workgroupPrompt(a.Fields["prompt"], a.Fields["linear"])
		var def *wsops.AgentSpec
		if len(repos) > 0 || prompt != "" {
			def = &wsops.AgentSpec{Prompt: prompt}
		}
		if _, err := wsops.CreateWorkspace(ctx, a.Fields["name"], repos, def); err != nil {
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
	case "archive": // toggle done/archived for the agent in a.ID
		if err := wsops.ToggleArchived(ctx, a.ID); err != nil {
			return fail("%v", err)
		}
		d.triggerPoll()
		return ok()
	default:
		return fail("unknown action %q", a.Action)
	}
}

// workgroupPrompt weaves a Linear issue link and a free-text description into a
// single baseline prompt for the workgroup's first agent. Connecting to Linear is
// MVP: the issue URL is handed to the agent in its prompt (no API sync yet).
func workgroupPrompt(description, linear string) string {
	var parts []string
	if linear = strings.TrimSpace(linear); linear != "" {
		parts = append(parts, "Linear issue to work on: "+linear)
	}
	if d := strings.TrimSpace(description); d != "" {
		parts = append(parts, d)
	}
	return strings.Join(parts, "\n\n")
}

func ok() core.Result { return core.Result{Type: "result", OK: true} }

func fail(format string, args ...any) core.Result {
	return core.Result{Type: "result", OK: false, Error: fmt.Sprintf(format, args...)}
}
