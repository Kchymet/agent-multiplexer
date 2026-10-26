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
		if harness, ok := authSetting(args[1]); ok {
			value, err := amuxcfg.AuthMode(harness)
			if err != nil {
				return err
			}
			fmt.Println(value)
			return nil
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
		if chord == "" && (keymap.Keymap{}).Chord(action) == "" {
			return fmt.Errorf("unknown action %q (see `amux config ls`)", action)
		}
		fmt.Println(bindingLabel(chord))
		return nil
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: amux config set <setting> <value>")
		}
		if harness, ok := authSetting(args[1]); ok {
			if args[2] == "" {
				return fmt.Errorf("%s must be one of %q (use unset to reset)", args[1], amuxcfg.AuthModes(harness))
			}
			if err := amuxcfg.SetAuthMode(harness, args[2]); err != nil {
				return err
			}
			fmt.Printf("%s = %s  (saved; %s)\n", args[1], args[2], authApplyHint(harness))
			return nil
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
		if harness, ok := authSetting(args[1]); ok {
			if err := amuxcfg.SetAuthMode(harness, ""); err != nil {
				return err
			}
			fmt.Printf("%s = %s  (default; %s)\n", args[1], amuxcfg.DefaultAuthMode(harness), authApplyHint(harness))
			return nil
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
		fmt.Printf("keys.%s = %s  (default; reopen the dashboard to apply)\n", action, bindingLabel(km.Chord(action)))
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
  get|set|unset claude.auth  central (default) | host | shared | environment
  get|set|unset codex.auth   central (default) | host | environment
  path               print the config file path

Keybindings live in the "keys" section of the config file (see path). A rebind
applies the next time the dashboard opens. codex.control lives in "codex" and
requires a daemon restart; changing it does not reroute running sessions.
AMUX_CODEX_CONTROL overrides the saved value when nonempty. Run amux doctor to
compare saved settings, this shell's override, and the running daemon selection.

claude.auth and codex.auth choose how sessions authenticate. central delivers
one host login (amux auth login) to every session; host shares the host
harness login as before; shared (Claude) uses an older amux shared login;
environment provides nothing, leaving API keys, apiKeyHelper, cloud providers
and gateways to the environment and harness settings. Central Codex auth
always uses App Server control.
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
		if b.Chord == "" {
			mark = "  (default displaced by configured binding)"
		}
		fmt.Printf("keys.%-14s %-12s %s%s\n", b.Action, bindingLabel(b.Chord), b.Desc, mark)
	}
	var authErr error
	for _, harness := range []string{"claude", "codex"} {
		saved, err := amuxcfg.SavedAuthMode(harness)
		switch {
		case err != nil:
			fmt.Printf("%s.auth INVALID\n", harness)
			authErr = errors.Join(authErr, err)
		case saved == "":
			fmt.Printf("%s.auth %s  (default)\n", harness, amuxcfg.DefaultAuthMode(harness))
		default:
			fmt.Printf("%s.auth %s  (saved)\n", harness, saved)
		}
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
	return errors.Join(loadErr, authErr, controlErr)
}

// authSetting maps claude.auth / codex.auth to their harness.
func authSetting(path string) (string, bool) {
	switch path {
	case "claude.auth":
		return "claude", true
	case "codex.auth":
		return "codex", true
	}
	return "", false
}

func authApplyHint(harness string) string {
	if harness == "codex" {
		return "run amux daemon restart to apply"
	}
	return "applies to Claude panes started from now on; amux auth restart restarts idle ones"
}

// keyPath resolves a CLI settings path to a keymap action. The "keys."
// namespace is owned by keymap; daemon settings are handled above.
func keyPath(path string) (string, error) {
	action, ok := strings.CutPrefix(path, "keys.")
	if !ok {
		return "", fmt.Errorf("unknown setting %q — use keys.<action>, claude.auth, codex.auth or codex.control (see `amux config ls`)", path)
	}
	return action, nil
}

func bindingLabel(chord string) string {
	if chord == "" {
		return "unbound"
	}
	return chord
}
