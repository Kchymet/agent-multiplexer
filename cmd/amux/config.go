package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"amux/internal/amuxcfg"
	"amux/internal/core"
	"amux/internal/directsvc"
	"amux/internal/harnessbin"
	"amux/internal/keymap"
	"amux/internal/selfupdate"
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
		if args[1] == directsvc.EnabledKey {
			s, _, err := directsvc.Load()
			if err != nil {
				return err
			}
			fmt.Println(onOff(s.Enabled))
			return nil
		}
		if harnessbin.IsSettingKey(args[1]) {
			value, err := harnessbin.Get(args[1])
			if err != nil {
				return err
			}
			fmt.Println(harnessSettingLabel(args[1], value))
			return nil
		}
		if args[1] == amuxcfg.AutoUpdateKey {
			on, err := amuxcfg.ResolveAutoUpdate()
			if err != nil {
				return err
			}
			fmt.Println(onOff(on))
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
		if args[1] == directsvc.EnabledKey {
			return fmt.Errorf("%s is changed with amux direct enable|disable, which validate the listener settings first", directsvc.EnabledKey)
		}
		if harnessbin.IsSettingKey(args[1]) {
			if args[2] == "" {
				return fmt.Errorf("%s needs a value (use unset to reset)", args[1])
			}
			if err := harnessbin.Set(args[1], args[2]); err != nil {
				return err
			}
			fmt.Printf("%s = %s  (saved; %s)\n", args[1], args[2], harnessSettingHint(args[1]))
			return nil
		}
		if args[1] == amuxcfg.AutoUpdateKey {
			on, err := amuxcfg.ParseOnOff(amuxcfg.AutoUpdateKey, args[2])
			if err != nil {
				return err
			}
			if err := amuxcfg.SetAutoUpdate(&on); err != nil {
				return err
			}
			fmt.Printf("%s = %s  (saved; the daemon reads it at its next check)\n", amuxcfg.AutoUpdateKey, onOff(on))
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
		if harnessbin.IsSettingKey(args[1]) {
			if err := harnessbin.Set(args[1], ""); err != nil {
				return err
			}
			fmt.Printf("%s = %s  (default; %s)\n", args[1], harnessSettingLabel(args[1], ""), harnessSettingHint(args[1]))
			return nil
		}
		if args[1] == amuxcfg.AutoUpdateKey {
			if err := amuxcfg.SetAutoUpdate(nil); err != nil {
				return err
			}
			fmt.Printf("%s = on  (default)\n", amuxcfg.AutoUpdateKey)
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
  get|set|unset update.auto <on|off>  automatic amux self-update (on; AMUX_NO_AUTO_UPDATE=1 also disables)
  get|set|unset harnesses.update  daemon update check for amux-managed harnesses: off|notify|auto (off)
  get|set|unset harnesses.check_interval  how often that check runs (24h; minimum 1h)
  get|set|unset harnesses.<claude|codex>.channel  release channel to follow (claude: stable|latest)
  get|set|unset harnesses.<claude|codex>.bin  explicit harness binary (beats the managed install)
  get|unset claude.auth      retired: every workload follows central auth
  get|unset codex.auth       retired: every workload follows central auth
  get direct.enabled  whether the daemon runs the direct listener (off by default);
                     change it with amux direct enable|disable
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
	autoUpdate, autoSet, autoErr := amuxcfg.AutoUpdate()
	switch {
	case autoErr != nil:
		fmt.Printf("%s INVALID\n", amuxcfg.AutoUpdateKey)
	case !autoSet:
		fmt.Printf("%s on  (default; %s=1 also disables)\n", amuxcfg.AutoUpdateKey, selfupdate.DisableEnv)
	default:
		fmt.Printf("%s %s  (saved)\n", amuxcfg.AutoUpdateKey, onOff(autoUpdate))
	}
	direct, directSet, directErr := directsvc.Load()
	switch {
	case directErr != nil:
		fmt.Printf("%s INVALID\n", directsvc.EnabledKey)
	case !directSet:
		fmt.Printf("%s off  (default; amux direct enable|disable — applied by the running daemon)\n", directsvc.EnabledKey)
	default:
		fmt.Printf("%s %s  (saved; amux direct enable|disable — applied by the running daemon)\n", directsvc.EnabledKey, onOff(direct.Enabled))
	}
	var harnessErr error
	for _, key := range harnessbin.SettingKeys() {
		value, err := harnessbin.Get(key)
		switch {
		case err != nil:
			fmt.Printf("%s INVALID\n", key)
			harnessErr = err
		case value == "":
			fmt.Printf("%s %s  (default)\n", key, harnessSettingLabel(key, ""))
		default:
			fmt.Printf("%s %s  (saved)\n", key, value)
		}
	}
	return errors.Join(loadErr, authErr, controlErr, resumeErr, autoErr, directErr, harnessErr)
}

// harnessSettingLabel renders a harnesses.* value, naming the default when unset.
func harnessSettingLabel(key, value string) string {
	if value != "" {
		return value
	}
	switch {
	case key == harnessbin.KeyUpdate:
		return harnessbin.DefaultUpdateMode
	case key == harnessbin.KeyRestartSessions:
		return "on"
	case key == harnessbin.KeyInterval:
		return harnessbin.DefaultCheckInterval.String()
	case strings.HasSuffix(key, ".channel"):
		return "(release default)"
	default:
		return "(none: managed install, then PATH)"
	}
}

func harnessSettingHint(key string) string {
	if key == harnessbin.KeyUpdate || key == harnessbin.KeyInterval {
		return "the running daemon re-reads it within 15 minutes"
	}
	if key == harnessbin.KeyRestartSessions {
		return "the running daemon re-reads it within a minute"
	}
	if strings.HasSuffix(key, ".bin") {
		return "applies to newly launched sessions"
	}
	return "used by amux harness update and the daemon's update check"
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
