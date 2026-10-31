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
		if args[1] == amuxcfg.CoordinatorArchiveScopeKey {
			if err := refuseAgentHostSetting(args[1]); err != nil {
				return err
			}
			value, err := amuxcfg.CoordinatorArchiveScope()
			if err != nil {
				return err
			}
			fmt.Println(value)
			return nil
		}
		if args[1] == amuxcfg.GoalAutoResumeKey {
			on, err := amuxcfg.ResolveGoalAutoResume()
			if err != nil {
				return err
			}
			fmt.Println(onOff(on))
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
			if args[2] != amuxcfg.AuthCentral {
				return fmt.Errorf("%s is retired: every workload follows the central login; run `amux auth login %s`", args[1], harness)
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
		if args[1] == amuxcfg.CoordinatorArchiveScopeKey {
			if err := refuseAgentHostSetting(args[1]); err != nil {
				return err
			}
			if strings.TrimSpace(args[2]) == "" {
				return fmt.Errorf("%s must be members or host (use unset to reset)", args[1])
			}
			if err := amuxcfg.SetCoordinatorArchiveScope(args[2]); err != nil {
				return err
			}
			value, _ := amuxcfg.CoordinatorArchiveScope()
			fmt.Printf("%s = %s  (saved; applies immediately, and coordinator guides mention it from their next launch)\n", args[1], value)
			return nil
		}
		if args[1] == amuxcfg.GoalAutoResumeKey {
			on, err := amuxcfg.ParseGoalAutoResume(args[2])
			if err != nil {
				return err
			}
			if err := amuxcfg.SetGoalAutoResume(&on); err != nil {
				return err
			}
			fmt.Printf("%s = %s  (saved; run amux daemon restart to apply)\n", amuxcfg.GoalAutoResumeKey, onOff(on))
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
		if args[1] == amuxcfg.CoordinatorArchiveScopeKey {
			if err := refuseAgentHostSetting(args[1]); err != nil {
				return err
			}
			if err := amuxcfg.SetCoordinatorArchiveScope(""); err != nil {
				return err
			}
			fmt.Printf("%s = %s  (default; applies immediately)\n", args[1], amuxcfg.ArchiveScopeMembers)
			return nil
		}
		if args[1] == amuxcfg.GoalAutoResumeKey {
			if err := amuxcfg.SetGoalAutoResume(nil); err != nil {
				return err
			}
			fmt.Printf("%s = on  (default; run amux daemon restart to apply)\n", amuxcfg.GoalAutoResumeKey)
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
  get goal.auto_resume  print whether a restarted goal coordinator resumes its goal (on when unconfigured)
  set goal.auto_resume <on|off>  save the goal auto-resume switch (daemon restart required)
  unset goal.auto_resume  restore the default (on)
  get coordinator.archive_scope  print how far coordinators may archive (members when unconfigured)
  set coordinator.archive_scope <members|host>  host only: let coordinators archive idle sessions host-wide
  unset coordinator.archive_scope  restore the default (members)
  get|unset claude.auth      retired: every workload follows central auth
  get|unset codex.auth       retired: every workload follows central auth
  path               print the config file path

Keybindings live in the "keys" section of the config file (see path). A rebind
applies the next time the dashboard opens. codex.control lives in "codex" and
requires a daemon restart; changing it does not reroute running sessions.
AMUX_CODEX_CONTROL overrides the saved value when nonempty. Run amux doctor to
compare saved settings, this shell's override, and the running daemon selection.
goal.auto_resume lives in "goal": when on (the default), a goal coordinator whose
runtime is restarted (agent restart, daemon restart, amux do start) is relaunched
with a continuation so a goal that was active or waiting on a wake-up picks up
where it left off; paused, complete, blocked, input-waiting and budget-limited
goals are never resumed automatically. It too is read at daemon start.

coordinator.archive_scope lives in "coordinator" and is a host-only setting:
amux refuses to read or change it from inside an agent session. members (the
default) lets a coordinator archive only its own workgroup's direct members.
host additionally lets every coordinator see a read-only staleness view of all
sessions (amux status --json, "hostSessions") and archive any idle ordinary
agent or workgroup with amux do set-archived <id> -f archived=true. Running or
waiting (permission/input) targets, the console and repo homes are refused;
restore, delete, steer and every other effect on a foreign session stay
host-only. Each such archive is logged by the daemon and appended to
the coordinator-archive audit log (state dir audit/coordinator-archive.jsonl).
The daemon reads the value per request, so changes apply without a restart.

claude.auth and codex.auth are retired. Every workload follows the central
login (amux auth login). A saved host, shared or environment value was
migrated to central once; no credential is copied, so run amux auth login once
if Accounts reports the migration as pending. Central Codex auth always uses
App Server control.
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
	on, set, resumeErr := amuxcfg.GoalAutoResume()
	switch {
	case resumeErr != nil:
		fmt.Printf("%s INVALID\n", amuxcfg.GoalAutoResumeKey)
	case !set:
		fmt.Printf("%s on  (default; daemon restart required after changes)\n", amuxcfg.GoalAutoResumeKey)
	default:
		fmt.Printf("%s %s  (saved; daemon restart required after changes)\n", amuxcfg.GoalAutoResumeKey, onOff(on))
	}
	savedScope, scopeErr := amuxcfg.SavedCoordinatorArchiveScope()
	const scopeDesc = "how far coordinators may archive: members = own workgroup only, host = any idle session"
	switch {
	case scopeErr != nil:
		fmt.Printf("%s INVALID  (%s)\n", amuxcfg.CoordinatorArchiveScopeKey, scopeDesc)
	case savedScope == "":
		fmt.Printf("%s %s  (default; %s; host-only setting)\n", amuxcfg.CoordinatorArchiveScopeKey, amuxcfg.ArchiveScopeMembers, scopeDesc)
	default:
		fmt.Printf("%s %s  (saved; %s; host-only setting)\n", amuxcfg.CoordinatorArchiveScopeKey, savedScope, scopeDesc)
	}
	return errors.Join(loadErr, authErr, controlErr, resumeErr, scopeErr)
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
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

// refuseAgentHostSetting rejects host-only settings inside an agent session.
// An agent's config path is not the host's (its sandbox cannot reach the host
// config), so a "successful" write there would silently mislead; and widening
// coordinator authority is never an agent's decision.
func refuseAgentHostSetting(key string) error {
	if restrictedSessionClient(os.Getenv) {
		return fmt.Errorf("%s is a host-only setting; ask the user to run `amux config ... %s` on the host, outside any agent session", key, key)
	}
	return nil
}

// keyPath resolves a CLI settings path to a keymap action. The "keys."
// namespace is owned by keymap; daemon settings are handled above.
func keyPath(path string) (string, error) {
	action, ok := strings.CutPrefix(path, "keys.")
	if !ok {
		return "", fmt.Errorf("unknown setting %q — use keys.<action>, claude.auth, codex.auth, codex.control, goal.auto_resume or coordinator.archive_scope (see `amux config ls`)", path)
	}
	return action, nil
}

func bindingLabel(chord string) string {
	if chord == "" {
		return "unbound"
	}
	return chord
}
