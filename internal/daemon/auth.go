package daemon

import (
	"context"
	"fmt"
	"log"

	"amux/internal/agent"
	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/engine"
	"amux/internal/panespec"
)

type authReload struct {
	instance engine.Instance
	force    bool
}

func (d *Daemon) queueAuthReload(a core.Action) error {
	if a.ID != "" || a.Kind != "" || a.Target != "" {
		return fmt.Errorf("auth-reload targets all live Claude agent panes")
	}
	for k, v := range a.Fields {
		if k != "force" || (v != "true" && v != "false") {
			return fmt.Errorf("auth-reload accepts only force=true|false")
		}
	}
	if !claudecfg.SharedAuthEnabled() {
		return fmt.Errorf("no shared Claude login; run amux auth login first")
	}
	if d.engine == nil {
		return fmt.Errorf("engine unavailable")
	}
	d.authMu.Lock()
	defer d.authMu.Unlock()
	if d.authPending == nil {
		d.authPending = map[engine.Key]authReload{}
	}
	for _, k := range d.engine.Live() {
		if k.Tab != panespec.TabAgent {
			continue
		}
		s, found, err := lookupSession(k.AgentID)
		if err != nil {
			return err
		}
		if !found || agent.Canonical(s.Agent) != "claude" {
			continue
		}
		if in, ok := d.engine.Lookup(k); ok {
			previous := d.authPending[k]
			force := a.Fields["force"] == "true" || (previous.instance == in && previous.force)
			d.authPending[k] = authReload{instance: in, force: force}
		}
	}
	return nil
}

// resumeWithSharedAuth runs on the polling goroutine. Defer busy AND unknown
// sessions so a credential change never silently interrupts ongoing work.
// A forced request is explicit permission to interrupt a stuck login prompt.
func (d *Daemon) resumeWithSharedAuth(ctx context.Context) {
	d.authMu.Lock()
	defer d.authMu.Unlock()
	for k, pending := range d.authPending {
		if ctx.Err() != nil {
			return
		}
		in, ok := d.engine.Lookup(k)
		if !ok || in != pending.instance || !in.Alive() {
			delete(d.authPending, k)
			continue // closed/archived/restarted while waiting: never resurrect it
		}
		if !pending.force && d.instanceActivity(k) != engine.ActivitySafe {
			continue
		}
		// Resolve before stopping so a launch/config error preserves the old pane.
		dir, env, argv, err := d.resolve(k.AgentID, k.Tab)
		if err != nil {
			log.Printf("amux: auth reload %s: %v", k.AgentID, err)
			delete(d.authPending, k)
			continue
		}
		// Resolving a launch can take time; recheck before interrupting the pane.
		if !pending.force && d.instanceActivity(k) != engine.ActivitySafe {
			continue
		}
		d.engine.Kill(k)
		delete(d.authPending, k)
		if _, err := d.engine.Ensure(ctx, engine.Spec{Key: k, Dir: dir, Env: env, Argv: argv}); err != nil {
			log.Printf("amux: auth reload %s could not resume: %v; reopen its agent pane", k.AgentID, err)
		}
	}
}
