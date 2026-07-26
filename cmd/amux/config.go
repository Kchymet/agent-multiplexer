package main

import (
	"fmt"
	"strings"

	"amux/internal/core"
	"amux/internal/keymap"
)

// cmdConfig is amux's own settings surface (`amux config`), today covering the
// native TUI's global keybindings. It is deliberately scriptable — agents (the
// control console included) and doctor fixes drive it the same way a user does:
//
//	amux config ls
//	amux config set keys.focus-rail ctrl+g
//	amux config unset keys.focus-rail
func cmdConfig(args []string) error {
	sub := "ls"
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "ls", "list":
		return configList()
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux config get keys.<action>")
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
			return fmt.Errorf("usage: amux config set keys.<action> <chord>   (e.g. amux config set keys.focus-rail ctrl+g)")
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
			return fmt.Errorf("usage: amux config unset keys.<action>")
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
	return loadErr
}

// keyPath resolves a CLI settings path to a keymap action. Only the "keys."
// namespace exists today; the prefix keeps room for future sections.
func keyPath(path string) (string, error) {
	action, ok := strings.CutPrefix(path, "keys.")
	if !ok {
		return "", fmt.Errorf("unknown setting %q — keybindings live under keys.<action> (see `amux config ls`)", path)
	}
	return action, nil
}
