package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"amux/internal/amuxcfg"
	"amux/internal/core"
	"amux/internal/keymap"
)

// cmdConfig is amux's own scriptable settings surface for native TUI
// keybindings and daemon rollout selection:
//
//	amux config ls
//	amux config set keys.focus-rail ctrl+g
//	amux config unset keys.focus-rail
func cmdConfig(args []string) error {
	sub := "ls"
	if len(args) > 0 {
		sub = args[0]
	}
	if isHelpFlag(sub) {
		configUsage()
		return nil
	}
	switch sub {
	case "ls", "list":
		return configList()
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux config get <setting>")
		}
		if args[1] == "codex.control" {
			value, err := amuxcfg.CodexControl()
			if err != nil {
				return err
			}
			if value == "" {
				value = amuxcfg.PTY
			}
			fmt.Println(value)
			return nil
		}
		action, err := keyPath(args[1])
		if err != nil {
			return err
		}
		km, err := keymap.Load()
		if err != nil {
			return err
		}
		chord := km.Chord(action)
		if chord == "" {
			return fmt.Errorf("unknown action %q (see `amux config ls`)", action)
		}
		fmt.Println(chord)
		return nil
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: amux config set <setting> <value>")
		}
		if args[1] == "codex.control" {
			if args[2] == "" {
				return fmt.Errorf("codex.control must be app-server or pty (use unset to reset)")
			}
			if err := amuxcfg.SetCodexControl(args[2]); err != nil {
				return err
			}
			fmt.Printf("codex.control = %s  (saved; run amux daemon restart to apply; environment overrides still take precedence)\n", args[2])
			return nil
		}
		action, err := keyPath(args[1])
		if err != nil {
			return err
		}
		chord, err := keymap.Set(action, args[2])
		if err != nil {
			return err
		}
		fmt.Printf("keys.%s = %s  (reopen the dashboard to apply)\n", action, chord)
		return nil
	case "unset", "reset":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux config unset <setting>")
		}
		if args[1] == "codex.control" {
			if err := amuxcfg.SetCodexControl(""); err != nil {
				return err
			}
			fmt.Println("codex.control = pty  (default; run amux daemon restart to apply; environment overrides still take precedence)")
			return nil
		}
		action, err := keyPath(args[1])
		if err != nil {
			return err
		}
		if err := keymap.Unset(action); err != nil {
			return err
		}
		km, _ := keymap.Load()
		fmt.Printf("keys.%s = %s  (default; reopen the dashboard to apply)\n", action, km.Chord(action))
		return nil
	case "path":
		fmt.Println(core.ConfigPath())
		return nil
	default:
		return fmt.Errorf("unknown config subcommand %q (want ls|get|set|unset|path)", sub)
	}
}

func configUsage() {
	fmt.Fprint(os.Stderr, `amux config — show or change amux settings

usage: amux config <subcommand>

  ls                 saved settings/defaults (ignores environment overrides)
  get keys.<action>  print one binding's effective chord
  set keys.<action> <chord>  rebind (chords like ctrl+g, alt+p, f5)
  unset keys.<action>  restore a binding's built-in default
  get codex.control  print saved control mode (pty when unconfigured)
  set codex.control <app-server|pty>  save the daemon's Codex rollout selection
  unset codex.control  restore the default (pty)
  path               print the config file path

Keybindings live in the "keys" section of the config file (see path). A rebind
applies the next time the dashboard opens. codex.control lives in "codex" and
requires a daemon restart; changing it does not reroute running sessions.
AMUX_CODEX_CONTROL overrides the saved value when nonempty. Run amux doctor to
compare saved settings, this shell's override, and the running daemon selection.
`)
}

// configList prints every binding; a load error (bad entries in config.json)
// is reported after the table, which shows the effective fallbacks.
func configList() error {
	km, loadErr := keymap.Load()
	for _, b := range km.List() {
		mark := ""
		if !b.Default {
			mark = "  (custom)"
		}
		fmt.Printf("keys.%-14s %-12s %s%s\n", b.Action, b.Chord, b.Desc, mark)
	}
	value, controlErr := amuxcfg.CodexControl()
	if controlErr != nil {
		fmt.Println("codex.control INVALID")
	} else {
		mark := "saved"
		if value == "" {
			value, mark = amuxcfg.PTY, "default"
		}
		fmt.Printf("codex.control %s  (%s; daemon restart required after changes)\n", value, mark)
	}
	return errors.Join(loadErr, controlErr)
}

// keyPath resolves a CLI settings path to a keymap action. The "keys."
// namespace is owned by keymap; daemon settings are handled above.
func keyPath(path string) (string, error) {
	action, ok := strings.CutPrefix(path, "keys.")
	if !ok {
		return "", fmt.Errorf("unknown setting %q — use keys.<action> or codex.control (see `amux config ls`)", path)
	}
	return action, nil
}
