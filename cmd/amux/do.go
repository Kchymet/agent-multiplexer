package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"amux/internal/core"
	"amux/internal/daemon"
)

// cmdDo sends a single control action to the daemon and prints the result. It is
// the scripting entrypoint to the daemon's action API — the same dispatch the
// rail and native TUI drive — so command-line callers go through the daemon
// (single writer, automatic re-poll) instead of opening the store directly.
//
// Usage:
//
//	amux do <action> [id] [kind]
//	amux do <action> [--id ID] [--target ROOT] [--kind KIND] [--cwd DIR] [-f k=v]...
//
// The positional [id] and [kind] forms are kept for back-compat; flags below set
// the same fields and additionally reach Target and the form Fields that the
// richer actions (add-agent, new-workgroup, new-repo-agent, add-repo, rename)
// need. Examples:
//
//	amux do refresh
//	amux do attach 3f7a
//	amux do rename 3f7a -f name="api spike"
//	amux do move 3f7a --target 9c1b
//	amux do add-agent 9c1b -f repos=api,web -f prompt="port the auth flow"
//	amux do new-workgroup -f name=infra -f repos=infra -f prompt="upgrade CI"
func cmdDo(args []string) error {
	a, err := parseDoArgs(args)
	if err != nil {
		return err
	}
	// `do` drives control actions, which reply with a Result. The query verb
	// replies with a Data frame instead, so routing it here would block waiting
	// for a Result that never comes — point callers at the read commands.
	if a.Action == core.ActionQuery {
		return fmt.Errorf("%q is a read action; use `amux repo ls` / `amux session ls`", a.Action)
	}
	if err := sendAction(a); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

// cmdRefresh asks the daemon to re-poll its sources now, rather than waiting for
// the next tick. Handy after an out-of-band change so `amux status` reflects it.
func cmdRefresh() error {
	if err := sendAction(core.Action{Action: "refresh"}); err != nil {
		return err
	}
	fmt.Println("refreshed")
	return nil
}

// parseDoArgs builds a daemon Action from `do` arguments. The first positional is
// the action (required); any further positionals fill ID then Kind, preserving
// the old `do <action> [id] [kind]` form. Flags set the same fields and the ones
// positionals can't reach:
//
//	--id ID              target session id (alternative to the 2nd positional)
//	--target, -t ROOT    destination root id (for "move"; "" = new work-scoped)
//	--kind KIND          agent kind (for "new")
//	--cwd DIR            working directory (for "new")
//	--field, -f key=val  form field, repeatable (add-agent, new-workgroup, …)
//
// A flag value may be given as `--flag value` or `--flag=value`.
func parseDoArgs(args []string) (core.Action, error) {
	var a core.Action
	var positional []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		name, inline, hasInline := strings.Cut(arg, "=")
		// value resolves the flag's argument from `=value` or the next token.
		value := func() (string, error) {
			if hasInline {
				return inline, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("flag %s needs a value", name)
			}
			i++
			return args[i], nil
		}
		switch name {
		case "--id":
			v, err := value()
			if err != nil {
				return core.Action{}, err
			}
			a.ID = v
		case "--target", "-t":
			v, err := value()
			if err != nil {
				return core.Action{}, err
			}
			a.Target = v
		case "--kind":
			v, err := value()
			if err != nil {
				return core.Action{}, err
			}
			a.Kind = v
		case "--cwd":
			v, err := value()
			if err != nil {
				return core.Action{}, err
			}
			a.Cwd = v
		case "--field", "-f":
			v, err := value()
			if err != nil {
				return core.Action{}, err
			}
			key, fv, ok := strings.Cut(v, "=")
			if !ok || key == "" {
				return core.Action{}, fmt.Errorf("--field expects key=value, got %q", v)
			}
			if a.Fields == nil {
				a.Fields = map[string]string{}
			}
			a.Fields[key] = fv
		default:
			return core.Action{}, fmt.Errorf("unknown flag %q", name)
		}
	}

	if len(positional) > 0 {
		a.Action = positional[0]
	}
	if len(positional) > 1 && a.ID == "" {
		a.ID = positional[1]
	}
	if len(positional) > 2 && a.Kind == "" {
		a.Kind = positional[2]
	}
	if len(positional) > 3 {
		return core.Action{}, fmt.Errorf("too many arguments: %s", strings.Join(positional[3:], " "))
	}
	if strings.TrimSpace(a.Action) == "" {
		return core.Action{}, fmt.Errorf("usage: amux do <action> [id] [kind] [--target r] [--field k=v]...")
	}
	return a, nil
}

// dialDaemon ensures the daemon is running (launching it if it isn't) and returns
// a connected client. Every CLI read and mutation goes through here, so the CLI
// talks only to the local orchestrator and never opens the store itself.
func dialDaemon() (*daemon.Client, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := ensureDaemon(self); err != nil {
		return nil, fmt.Errorf("daemon unavailable: %w", err)
	}
	c, err := daemon.Dial()
	if err != nil {
		return nil, fmt.Errorf("daemon offline: %w", err)
	}
	return c, nil
}

// sendAction sends one action and waits for its result. Snapshot frames that
// arrive first are skipped. It returns the daemon's error on a failed action, so
// callers surface the same message the rail would show.
func sendAction(a core.Action) error {
	_, err := sendActionID(a)
	return err
}

// sendActionID is sendAction plus the id of any session the action created (the
// daemon's Result.NewID), so a create command can start or switch to it.
func sendActionID(a core.Action) (string, error) {
	c, err := dialDaemon()
	if err != nil {
		return "", err
	}
	defer c.Close()
	if err := c.Send(a); err != nil {
		return "", err
	}
	for {
		f, err := c.Next()
		if err != nil {
			return "", err
		}
		if f.Result != nil {
			if !f.Result.OK {
				return "", fmt.Errorf("%s", f.Result.Error)
			}
			return f.Result.NewID, nil
		}
	}
}

// queryRows asks the daemon for a read model (QueryRepos, QuerySessions) and
// decodes its rows into dst. It's the read half of the CLI's daemon bridge.
func queryRows(name string, dst any) error {
	c, err := dialDaemon()
	if err != nil {
		return err
	}
	defer c.Close()
	raw, err := c.Query(name)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil // empty result set: leave dst as its zero value
	}
	return json.Unmarshal(raw, dst)
}
