package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/harnessbin"
)

// harnessManager is the install manager the CLI drives (a seam for tests).
var harnessManager = harnessbin.New

// harnessSubcommands is `amux harness`'s dispatch. Most verbs download from
// the harnesses' release servers, so the dispatch contract test checks them by
// name rather than by invocation.
var harnessSubcommands = map[string]func([]string) error{
	"ls":        harnessList,
	"list":      harnessList,
	"install":   harnessInstall,
	"update":    harnessUpdate,
	"upgrade":   harnessUpdate,
	"pin":       harnessPin,
	"unpin":     harnessUnpin,
	"rollback":  harnessRollback,
	"check":     harnessCheck,
	"gc":        harnessGC,
	"uninstall": harnessUninstall,
	"adopt":     harnessAdopt,
}

// cmdHarnessCLI manages amux-owned standalone installs of Claude Code / Codex.
func cmdHarnessCLI(args []string) error {
	sub := "ls"
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	if isHelpFlag(sub) {
		return showHelp("harness")
	}
	run, ok := harnessSubcommands[sub]
	if !ok {
		return usagef("unknown harness subcommand %q", sub)
	}
	return run(args)
}

func harnessCtx() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

func parseKind(s string) (string, error) {
	k := agent.Canonical(strings.ToLower(strings.TrimSpace(s)))
	if !harnessbin.Supported(k) {
		return "", fmt.Errorf("unknown harness %q (want %s)", s, strings.Join(harnessbin.Kinds, " or "))
	}
	return k, nil
}

// kindsArg parses an optional kind list; empty means every managed harness
// (or every supported harness when all is set).
func kindsArg(m *harnessbin.Manager, args []string, all bool) ([]string, error) {
	if len(args) == 0 {
		var out []string
		for _, k := range harnessbin.Kinds {
			if all || m.Current(k) != "" {
				out = append(out, k)
			}
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("no amux-managed harness installed (run amux harness install claude|codex)")
		}
		return out, nil
	}
	var out []string
	for _, a := range args {
		k, err := parseKind(a)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, nil
}

type harnessListEntry struct {
	Kind      string           `json:"kind"`
	Binary    agent.Binary     `json:"binary"`
	Current   string           `json:"current,omitempty"`
	Installed []string         `json:"installed,omitempty"`
	State     harnessbin.State `json:"state"`
	Channel   string           `json:"channel,omitempty"`
}

func harnessList(args []string) error {
	fs := newCmdFlagSet("harness ls")
	asJSON := fs.Bool("json", false, "")
	if err := parseCmdFlags(fs, "harness ls", args); err != nil {
		return err
	}
	m := harnessManager()
	settings, _ := harnessbin.LoadSettings()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var entries []harnessListEntry
	for _, k := range harnessbin.Kinds {
		entries = append(entries, harnessListEntry{
			Kind: k, Binary: agent.ResolveBinary(ctx, k), Current: m.Current(k),
			Installed: m.Installed(k), State: m.LoadState(k), Channel: settings.Kind(k).Channel,
		})
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"update": settings.EffectiveUpdateMode(os.Getenv), "restart_sessions": settings.RestartSessionsEnabled(), "check_interval": settings.Interval().String(), "harnesses": entries})
	}
	mode := settings.EffectiveUpdateMode(os.Getenv)
	if harnessbin.EnvDisabled(os.Getenv) {
		mode += " (" + harnessbin.DisableEnv + ")"
	}
	restart := "running sessions restart onto a new version when idle"
	if !settings.RestartSessionsEnabled() {
		restart = "running sessions keep their version until restarted"
	}
	fmt.Printf("automatic updates: %s (every %s; amux config set harnesses.update off|notify|auto); %s\n\n", mode, settings.Interval(), restart)
	for _, e := range entries {
		fmt.Printf("%s\n", e.Kind)
		fmt.Printf("  launches  %s  (%s)\n", e.Binary.Path, e.Binary.Label())
		if len(e.Installed) == 0 {
			note := "amux harness install " + e.Kind
			if e.State.Declined {
				note = "uninstalled; not adopted automatically — " + note
			}
			fmt.Printf("  managed   not installed (%s)\n", note)
		}
		if e.Current == "" {
			fmt.Printf("  adoption  %s\n", adoptionStatus(e.State))
		}
		for _, v := range e.Installed {
			var marks []string
			if v == e.Current {
				marks = append(marks, "current")
			}
			if v == e.State.Pinned {
				marks = append(marks, "pinned")
			}
			if v == e.State.Previous {
				marks = append(marks, "previous")
			}
			mark := "  "
			if v == e.Current {
				mark = "* "
			}
			fmt.Printf("  %s%-12s %s\n", mark, v, strings.Join(marks, ", "))
		}
		if e.State.Available != "" {
			fmt.Printf("  update    %s available (amux harness update %s)\n", e.State.Available, e.Kind)
		}
		if e.State.CheckError != "" {
			fmt.Printf("  check     failed: %s\n", e.State.CheckError)
		} else if !e.State.CheckedAt.IsZero() {
			fmt.Printf("  checked   %s\n", e.State.CheckedAt.Local().Format(time.RFC3339))
		}
		if e.Current != "" && e.Binary.Source != agent.BinaryFromManaged {
			fmt.Printf("  note      the managed install is shadowed by %s\n", e.Binary.Var)
		}
		fmt.Println()
	}
	return nil
}

func harnessInstall(args []string) error {
	fs := newCmdFlagSet("harness install")
	noActivate := fs.Bool("no-activate", false, "")
	if err := parseCmdFlags(fs, "harness install", reorderFlags(args)); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return fmt.Errorf("usage: amux harness install <claude|codex> [<version>|stable|latest] [--no-activate]")
	}
	kind, err := parseKind(fs.Arg(0))
	if err != nil {
		return err
	}
	request := fs.Arg(1)
	if request == "" {
		settings, _ := harnessbin.LoadSettings()
		request = settings.Kind(kind).Channel
	}
	m := harnessManager()
	ctx, cancel := harnessCtx()
	defer cancel()
	fmt.Fprintf(os.Stderr, "installing %s %s…\n", kind, orElse(request, "(default channel)"))
	v, err := m.Install(ctx, kind, request, !*noActivate)
	if err != nil {
		return err
	}
	if *noActivate {
		fmt.Printf("%s %s installed (not active; current is %s)\n", kind, v, orElse(m.Current(kind), "none"))
		return nil
	}
	fmt.Printf("%s %s installed and active at %s\n", kind, v, m.BinPath(kind, v))
	fmt.Println("new sessions launch it; running sessions keep their version until restarted")
	reportGC(m, kind)
	return nil
}

func harnessUpdate(args []string) error {
	m := harnessManager()
	kinds, err := kindsArg(m, args, false)
	if err != nil {
		return err
	}
	settings, _ := harnessbin.LoadSettings()
	ctx, cancel := harnessCtx()
	defer cancel()
	var errs []error
	for _, k := range kinds {
		from, to, err := m.Update(ctx, k, settings.Kind(k).Channel)
		switch {
		case errors.Is(err, harnessbin.ErrPinned):
			fmt.Printf("%s: %v — skipped\n", k, err)
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", k, err))
		case from == to:
			fmt.Printf("%s: %s is already the newest\n", k, to)
		default:
			fmt.Printf("%s: %s → %s (running sessions keep %s until restarted)\n", k, orElse(from, "none"), to, orElse(from, "their version"))
			reportGC(m, k)
		}
	}
	return errors.Join(errs...)
}

func harnessPin(args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return fmt.Errorf("usage: amux harness pin <claude|codex> [<version>]")
	}
	kind, err := parseKind(args[0])
	if err != nil {
		return err
	}
	m := harnessManager()
	request := ""
	if len(args) == 2 {
		request = args[1]
	} else if cur := m.Current(kind); cur != "" {
		request = cur
	}
	ctx, cancel := harnessCtx()
	defer cancel()
	v, err := m.Pin(ctx, kind, request)
	if err != nil {
		return err
	}
	fmt.Printf("%s pinned to %s (updates skip it until amux harness unpin %s)\n", kind, v, kind)
	return nil
}

func harnessUnpin(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: amux harness unpin <claude|codex>")
	}
	kind, err := parseKind(args[0])
	if err != nil {
		return err
	}
	if err := harnessManager().Unpin(kind); err != nil {
		return err
	}
	fmt.Printf("%s unpinned\n", kind)
	return nil
}

func harnessRollback(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: amux harness rollback <claude|codex>")
	}
	kind, err := parseKind(args[0])
	if err != nil {
		return err
	}
	from, to, err := harnessManager().Rollback(kind)
	if err != nil {
		return err
	}
	fmt.Printf("%s: rolled back %s → %s (new sessions launch %s)\n", kind, from, to, to)
	return nil
}

func harnessCheck(args []string) error {
	m := harnessManager()
	kinds, err := kindsArg(m, args, true)
	if err != nil {
		return err
	}
	settings, _ := harnessbin.LoadSettings()
	ctx, cancel := harnessCtx()
	defer cancel()
	var errs []error
	for _, k := range kinds {
		v, err := m.Resolve(ctx, k, settings.Kind(k).Channel)
		if m.Current(k) != "" {
			_ = m.RecordCheck(k, v, err)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", k, err))
			continue
		}
		cur := m.Current(k)
		switch {
		case cur == "":
			fmt.Printf("%s: newest %s (not installed by amux)\n", k, v)
		case harnessbin.CompareVersions(v, cur) > 0:
			fmt.Printf("%s: %s available (current %s)\n", k, v, cur)
		default:
			fmt.Printf("%s: %s is up to date\n", k, cur)
		}
	}
	return errors.Join(errs...)
}

// harnessAdopt applies the daemon's adoption rules once, now: a managed copy
// for each harness the user already uses, never one they don't, never over
// an explicit AMUX_<KIND>_BIN / harnesses.<kind>.bin, never a kind already
// managed or uninstalled. scripts/install.sh runs it.
func harnessAdopt(args []string) error {
	fs := newCmdFlagSet("harness adopt")
	dry := fs.Bool("dry-run", false, "")
	if err := parseCmdFlags(fs, "harness adopt", args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("usage: amux harness adopt [--dry-run]")
	}
	settings, err := harnessbin.LoadSettings()
	if err != nil {
		return err
	}
	if mode := settings.EffectiveUpdateMode(os.Getenv); mode != harnessbin.UpdateAuto {
		why := "harnesses.update is " + mode
		if harnessbin.EnvDisabled(os.Getenv) {
			why = harnessbin.DisableEnv + " is set"
		}
		fmt.Printf("not adopting: %s (amux harness install <kind> installs one explicitly)\n", why)
		return nil
	}
	m := harnessManager()
	ctx, cancel := harnessCtx()
	defer cancel()
	input := func(kind string) harnessbin.AdoptInput { return agent.AdoptInput(ctx, kind) }
	if *dry {
		for _, k := range harnessbin.Kinds {
			ok, why := m.AdoptDecision(k, input(k))
			verb := "skip"
			if ok {
				verb = "adopt"
			}
			fmt.Printf("%s: %s — %s\n", k, verb, why)
		}
		return nil
	}
	var errs []error
	for _, r := range m.Adopt(ctx, settings, input) {
		switch {
		case r.Err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", r.Kind, r.Err))
		case r.Adopted:
			fmt.Printf("%s: installed amux-managed %s (%s); new sessions launch it and amux keeps it updated\n", r.Kind, r.Version, r.Reason)
		default:
			fmt.Printf("%s: skipped — %s\n", r.Kind, r.Reason)
		}
	}
	return errors.Join(errs...)
}

func harnessGC(args []string) error {
	m := harnessManager()
	kinds, err := kindsArg(m, args, true)
	if err != nil {
		return err
	}
	var errs []error
	for _, k := range kinds {
		if err := reportGC(m, k); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func harnessUninstall(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: amux harness uninstall <claude|codex>")
	}
	kind, err := parseKind(args[0])
	if err != nil {
		return err
	}
	removed, kept, err := harnessManager().Uninstall(kind)
	if err != nil {
		return err
	}
	fmt.Printf("%s: removed %s; new sessions use your own install (PATH)\n", kind, orElse(strings.Join(removed, ", "), "nothing"))
	if len(kept) > 0 {
		fmt.Printf("%s: kept %s (still running; amux harness gc %s removes it later)\n", kind, strings.Join(kept, ", "), kind)
	}
	return nil
}

func reportGC(m *harnessbin.Manager, kind string) error {
	removed, _, err := m.GC(kind)
	if len(removed) > 0 {
		fmt.Printf("%s: removed unused %s\n", kind, strings.Join(removed, ", "))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: gc: %v\n", kind, err)
	}
	return err
}

// reorderFlags moves flags after positionals to the front so `install claude
// 1.2.3 --no-activate` parses.
func reorderFlags(args []string) []string {
	var flags, pos []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

// adoptionStatus describes a harness's last automatic-adoption decision and
// its outcome, as recorded by the daemon (or amux harness adopt), for
// `amux harness ls` and `amux doctor`.
func adoptionStatus(st harnessbin.State) string {
	at := func(t time.Time) string { return t.Local().Format(time.RFC3339) }
	switch {
	case !st.AdoptStartedAt.IsZero():
		return fmt.Sprintf("installing since %s (%s); not finished — if this persists, see `harness updates` in %s", at(st.AdoptStartedAt), st.AdoptReason, core.LogPath())
	case st.AdoptDecision == harnessbin.AdoptDecisionSkip:
		return fmt.Sprintf("skipped at %s: %s", at(st.AdoptCheckedAt), st.AdoptReason)
	case st.AdoptError != "" && !st.AdoptAttemptAt.IsZero():
		return fmt.Sprintf("failed at %s: %s (retried after %s)", at(st.AdoptAttemptAt), st.AdoptError, harnessbin.AdoptRetry)
	case st.AdoptDecision == harnessbin.AdoptDecisionAdopt:
		return fmt.Sprintf("adopted at %s (%s)", at(st.AdoptAttemptAt), st.AdoptReason)
	}
	return "no decision recorded yet (the daemon decides about 2 minutes after it starts, then every 15 minutes; amux harness adopt --dry-run previews it)"
}

func orElse(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
