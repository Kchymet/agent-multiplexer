package main

import (
	"fmt"
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

// sendAction dials the daemon, sends one action, and waits for its result frame.
// Snapshot frames that arrive first are skipped. It returns the daemon's error
// on a failed action, so callers surface the same message the rail would show.
func sendAction(a core.Action) error {
	c, err := daemon.Dial()
	if err != nil {
		return fmt.Errorf("daemon offline: %w", err)
	}
	defer c.Close()
	if err := c.Send(a); err != nil {
		return err
	}
	for {
		f, err := c.Next()
		if err != nil {
			return err
		}
		if f.Result != nil {
			if !f.Result.OK {
				return fmt.Errorf("%s", f.Result.Error)
			}
			return nil
		}
	}
}
