package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"amux/internal/core"
	"amux/internal/provider"
	"amux/internal/providercfg"
)

// cmdProvideInstall writes the provider config file and installs the user
// service that runs `amux provide` from it. Registering a machine used to mean
// running the provider by hand in a terminal that had to stay open — nothing
// survived a reboot. This is the two-command version of that: install, then
// doctor.
//
// It merges over any existing config, so re-running it to change one setting
// (`amux provide install --name laptop`) keeps the rest, and re-running it after
// a binary upgrade re-points and restarts the service.
func cmdProvideInstall(args []string) error {
	fset := flag.NewFlagSet("provide install", flag.ContinueOnError)
	var f provideFlags
	f.register(fset)
	dryRun := fset.Bool("dry-run", false, "print the config file and service unit that would be written, then stop")
	execFlag := fset.String("exec", "", "amux binary the service runs (default: the installed binary, else this one)")
	// Install takes the same address-plus-flags shape as running the provider, so
	// it needs the same any-order parse: `amux provide install orch:7443
	// --token-file tok` must not drop the token file the way running once did.
	operands, err := parseFlagsAnyOrder(fset, args)
	if err != nil {
		return err
	}

	svc, err := providercfg.Service()
	if err != nil {
		return fmt.Errorf("provide install: %w — run `amux provide <addr>` in the foreground instead", err)
	}

	cfg, err := providercfg.Load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("provide install: %w", err)
	}
	applyProvideFlags(&cfg, &f, fset)
	// provideAddr rejects the two silent ways to get the address wrong (two
	// different addresses, a surplus operand); an address given either way
	// overrides whatever the existing config held.
	addr, err := provideAddr(f.orch, operands)
	if err != nil {
		return err
	}
	if addr != "" {
		cfg.Orchestrator = addr
	}
	// The service runs from a working directory it does not choose, so every path
	// it will later open has to be absolute in the file we write.
	if cfg.TokenFile != "" {
		if cfg.TokenFile, err = filepath.Abs(cfg.TokenFile); err != nil {
			return err
		}
	}
	if cfg.CAFile != "" {
		if cfg.CAFile, err = filepath.Abs(cfg.CAFile); err != nil {
			return err
		}
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("provide install: %w", err)
	}

	execPath, err := provideExecPath(*execFlag)
	if err != nil {
		return fmt.Errorf("provide install: %w", err)
	}
	unit := providercfg.DefaultUnit(execPath, providercfg.Path())

	if *dryRun {
		fmt.Printf("# %s\n%s\n", providercfg.Path(), cfg.Marshal())
		fmt.Printf("# %s (%s)\n%s", svc.Path(), svc.Kind(), svc.Render(unit))
		return nil
	}

	// The token is the one secret in this setup, and a service reads it
	// unattended: tighten the mode now rather than let doctor nag about it later.
	tightened, err := ensureTokenFile(cfg.TokenFile)
	if err != nil {
		return fmt.Errorf("provide install: %w", err)
	}
	if err := providercfg.Save(cfg); err != nil {
		return fmt.Errorf("provide install: write config: %w", err)
	}
	if err := svc.Install(unit); err != nil {
		return fmt.Errorf("provide install: %w", err)
	}

	fmt.Printf("wrote   %s\n", providercfg.Path())
	fmt.Printf("        orchestrator %s", cfg.Orchestrator)
	if cfg.Name != "" {
		fmt.Printf(" · name %s", cfg.Name)
	}
	fmt.Println()
	fmt.Printf("token   %s (mode 0600)\n", cfg.TokenFile)
	if tightened {
		fmt.Printf("        tightened its permissions to 0600\n")
	}
	fmt.Printf("service %s · %s\n", svc.Kind(), svc.Path())
	fmt.Printf("        runs `%s provide`, restarting on exit and at login/boot\n", execPath)
	if p := svc.Probe(); p.Active {
		fmt.Printf("        started\n")
	} else {
		fmt.Printf("        installed but not running (%s) — see `amux doctor`\n", p.Detail)
	}
	for _, line := range lingerAdvice() {
		fmt.Println(line)
	}
	fmt.Printf("\nCheck it with `amux doctor` (Provider section).\n")
	return nil
}

// cmdProvideUninstall stops and removes the user service. The config file and
// token stay put: uninstalling a service should not quietly destroy the
// credentials and settings needed to reinstall it.
func cmdProvideUninstall(args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("usage: amux provide uninstall  (takes no arguments)")
	}
	svc, err := providercfg.Service()
	if err != nil {
		return fmt.Errorf("provide uninstall: %w", err)
	}
	before := svc.Probe()
	if err := svc.Uninstall(); err != nil {
		return fmt.Errorf("provide uninstall: %w", err)
	}
	// The status file describes a provider that no longer runs; leaving it would
	// have doctor report a connection that cannot exist.
	_ = os.Remove(provider.StatusPath())

	if before.Installed {
		fmt.Printf("removed %s (%s)\n", svc.Path(), svc.Kind())
	} else {
		fmt.Printf("no %s service was installed (%s)\n", svc.Kind(), svc.Path())
	}
	if _, err := os.Stat(providercfg.Path()); err == nil {
		fmt.Printf("kept    %s — delete it by hand to forget the orchestrator\n", providercfg.Path())
	}
	return nil
}

// applyProvideFlags overlays the flags the user actually typed onto a config
// loaded from disk. It keys off fset.Visit rather than zero values so
// `--publish-sessions=false` turns the feature off instead of reading as "not
// specified" — the difference between install being idempotent and it being a
// one-way switch.
func applyProvideFlags(cfg *providercfg.Config, f *provideFlags, fset *flag.FlagSet) {
	set := map[string]bool{}
	fset.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	assign := func(name string, apply func()) {
		if set[name] {
			apply()
		}
	}
	assign("orchestrator", func() { cfg.Orchestrator = f.orch })
	assign("token-file", func() { cfg.TokenFile = f.tokenFile })
	assign("name", func() { cfg.Name = f.name })
	assign("ca", func() { cfg.CAFile = f.caFile })
	assign("server-name", func() { cfg.ServerName = f.serverName })
	assign("max-panes", func() { cfg.MaxPanes = f.maxPanes })
	assign("publish-sessions", func() { cfg.PublishSessions = f.publishSes })
	assign("read-only-sessions", func() { cfg.ReadOnlySessions = f.readOnly })
	assign("runtime-events", func() { cfg.RuntimeEvents = f.rtEvents })
	assign("label", func() { cfg.Labels = parseLabels("", f.labels) })
	assign("feature", func() { cfg.Features = mergeFeatures("", f.features) })
}

// provideExecPath picks the binary the service runs. It prefers the stable
// install path over os.Executable() for the same reason the Claude status hooks
// do (see core.InstalledBinPath): the running binary is often a throwaway dev
// build inside a worktree that later vanishes, which would leave the service
// pointing at nothing after the next reboot.
func provideExecPath(override string) (string, error) {
	if override != "" {
		return filepath.Abs(override)
	}
	if p := core.InstalledBinPath(); isExecutableFile(p) {
		return p, nil
	}
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(self)
}

func isExecutableFile(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir() && st.Mode().Perm()&0o111 != 0
}

// ensureTokenFile checks the bearer token file exists and is readable only by
// its owner, tightening the mode if it isn't. tightened reports whether it had
// to change anything, so install can say so out loud.
func ensureTokenFile(path string) (tightened bool, err error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("token file: %w (create it holding the orchestrator's bearer token, mode 0600)", err)
	}
	if st.IsDir() {
		return false, fmt.Errorf("token file %s is a directory", path)
	}
	if st.Mode().Perm() == 0o600 {
		return false, nil
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return false, fmt.Errorf("token file %s is mode %04o and could not be tightened: %w", path, st.Mode().Perm(), err)
	}
	return true, nil
}

// lingerAdvice is the Linux/WSL2 footnote: a systemd *user* service only runs
// while the user has a session, so without lingering the provider dies the
// moment the last terminal closes — exactly the failure installing a service was
// supposed to fix.
func lingerAdvice() []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	if enabled, known := providercfg.Linger(); known && enabled {
		return []string{"linger  enabled — the service keeps running after you log out"}
	}
	lines := []string{
		"linger  not enabled — the service stops when your last session closes.",
		"        Run: " + providercfg.LingerHint(),
	}
	if providercfg.IsWSL() {
		lines = append(lines, "        On WSL2 that is every terminal into this distro closing.")
	}
	return lines
}

// mergeLabels overlays higher-precedence labels onto lower-precedence ones (the
// config file under env/flags), returning nil when there are none so the
// register message stays clean.
func mergeLabels(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}
