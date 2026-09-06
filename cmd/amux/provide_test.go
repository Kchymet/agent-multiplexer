package main

import (
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"amux/internal/providercfg"
)

// parseInstall runs the install flag surface the way cmdProvideInstall does and
// returns the config the given argv would produce on top of base.
func parseInstall(t *testing.T, base providercfg.Config, argv ...string) providercfg.Config {
	t.Helper()
	fset := flag.NewFlagSet("provide install", flag.ContinueOnError)
	fset.SetOutput(os.NewFile(0, os.DevNull))
	var f provideFlags
	f.register(fset)
	if err := fset.Parse(argv); err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	applyProvideFlags(&base, &f, fset)
	return base
}

// TestInstallMergesOverTheExistingConfig is what makes `amux provide install`
// safe to re-run: changing one setting must not silently forget the rest.
func TestInstallMergesOverTheExistingConfig(t *testing.T) {
	base := providercfg.Config{
		Orchestrator: "orch:7443",
		TokenFile:    "/tmp/tok",
		Name:         "old",
		Labels:       map[string]string{"zone": "home"},
	}
	got := parseInstall(t, base, "--name", "laptop")
	if got.Name != "laptop" {
		t.Errorf("name = %q, want the new one", got.Name)
	}
	if got.Orchestrator != "orch:7443" || got.TokenFile != "/tmp/tok" {
		t.Errorf("re-install forgot settings it was not asked to change: %+v", got)
	}
	if !reflect.DeepEqual(got.Labels, map[string]string{"zone": "home"}) {
		t.Errorf("labels = %v, want the existing ones kept", got.Labels)
	}
}

// TestInstallCanTurnAFeatureOff pins the reason applyProvideFlags keys off
// fset.Visit rather than zero values: without it, --publish-sessions would be a
// one-way switch that no later install could undo.
func TestInstallCanTurnAFeatureOff(t *testing.T) {
	base := providercfg.Config{Orchestrator: "o:1", TokenFile: "/t", PublishSessions: true, RuntimeEvents: true}

	if got := parseInstall(t, base, "--name", "x"); !got.PublishSessions {
		t.Errorf("an unrelated flag turned publish-sessions off: %+v", got)
	}
	got := parseInstall(t, base, "--publish-sessions=false", "--runtime-events=false")
	if got.PublishSessions || got.RuntimeEvents {
		t.Errorf("--publish-sessions=false did not turn the feature off: %+v", got)
	}
}

func TestInstallReplacesLabelsAndFeatures(t *testing.T) {
	base := providercfg.Config{
		Orchestrator: "o:1", TokenFile: "/t",
		Labels:   map[string]string{"zone": "home", "gpu": "none"},
		Features: []string{"old"},
	}
	got := parseInstall(t, base, "--label", "zone=office", "--feature", "cuda", "--feature", "bigdisk")
	if !reflect.DeepEqual(got.Labels, map[string]string{"zone": "office"}) {
		t.Errorf("labels = %v, want the given set to replace the old one", got.Labels)
	}
	if !reflect.DeepEqual(got.Features, []string{"cuda", "bigdisk"}) {
		t.Errorf("features = %v, want the given set", got.Features)
	}
}

// TestExecutionConfigPrecedence keeps the execution selector aligned with the
// rest of provider configuration: flags override service environment, which
// overrides the installed file. `auto` is an ordered reset in a repeated flag
// list, so a later harness begins a new explicit allowlist.
func TestExecutionConfigPrecedence(t *testing.T) {
	file := providercfg.Config{Harnesses: []string{"claude"}, IdentityMode: "machine"}
	env := map[string]string{
		"AMUX_PROVIDER_HARNESSES":     "codex, hermes",
		"AMUX_PROVIDER_IDENTITY_MODE": "api-key",
	}
	getenv := func(key string) string { return env[key] }

	got, err := executionConfig(provideFlags{
		harnesses: multiFlag{"claude", "auto", "codex"}, identityMode: "machine",
	}, file, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"codex"}; !reflect.DeepEqual(got.Harnesses, want) {
		t.Errorf("harnesses = %v, want %v", got.Harnesses, want)
	}
	if got.IdentityMode != "machine" {
		t.Errorf("identity mode = %q, want flag value machine", got.IdentityMode)
	}

	got, err = executionConfig(provideFlags{}, file, func(key string) string {
		if key == "AMUX_PROVIDER_HARNESSES" {
			return "codex"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"codex"}; !reflect.DeepEqual(got.Harnesses, want) {
		t.Errorf("environment did not override file: %v", got.Harnesses)
	}
	if got.IdentityMode != "machine" {
		t.Errorf("file identity mode = %q, want machine", got.IdentityMode)
	}
}

func TestInstallExecutionConfigRoundTrip(t *testing.T) {
	base := providercfg.Config{Orchestrator: "o:1", TokenFile: "/t"}
	cfg := parseInstall(t, base,
		"--harness", "claude", "--harness", "auto", "--harness", "codex",
		"--identity-mode", "machine")
	if want := []string{"codex"}; !reflect.DeepEqual(cfg.Harnesses, want) {
		t.Fatalf("installed harnesses = %v, want %v", cfg.Harnesses, want)
	}
	got, err := providercfg.Parse(cfg.Marshal())
	if err != nil {
		t.Fatalf("Parse installed config: %v", err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Errorf("installed config round trip changed it:\n got %+v\nwant %+v", got, cfg)
	}
}

// TestInstallTakesTheAddressEitherWay: install has the same address-plus-flags
// shape running the provider does, so it needs the same any-order parse — a
// --token-file dropped here would write a config with no credential in it.
func TestInstallAddressAndFlagsInAnyOrder(t *testing.T) {
	for name, argv := range map[string][]string{
		"address first": {"orch:7443", "--token-file", "/tmp/tok"},
		"address last":  {"--token-file", "/tmp/tok", "orch:7443"},
		"flag form":     {"--orchestrator", "orch:7443", "--token-file", "/tmp/tok"},
	} {
		t.Run(name, func(t *testing.T) {
			fset := flag.NewFlagSet("provide install", flag.ContinueOnError)
			fset.SetOutput(os.NewFile(0, os.DevNull))
			var f provideFlags
			f.register(fset)
			operands, err := parseFlagsAnyOrder(fset, argv)
			if err != nil {
				t.Fatalf("parse %v: %v", argv, err)
			}
			addr, err := provideAddr(f.orch, operands)
			if err != nil {
				t.Fatalf("provideAddr: %v", err)
			}
			if addr != "orch:7443" {
				t.Errorf("address = %q, want orch:7443", addr)
			}
			if f.tokenFile != "/tmp/tok" {
				t.Errorf("token file = %q, want it read wherever it sits", f.tokenFile)
			}
		})
	}
}

// TestEnsureTokenFile: the service reads the credential unattended, so install
// tightens a loose mode rather than leaving it for doctor to complain about.
func TestEnsureTokenFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing", func(t *testing.T) {
		_, err := ensureTokenFile(filepath.Join(dir, "absent"))
		if err == nil || !strings.Contains(err.Error(), "0600") {
			t.Errorf("ensureTokenFile = %v, want an error that says how to create it", err)
		}
	})

	t.Run("already 0600", func(t *testing.T) {
		path := filepath.Join(dir, "tight")
		if err := os.WriteFile(path, []byte("tok"), 0o600); err != nil {
			t.Fatal(err)
		}
		tightened, err := ensureTokenFile(path)
		if err != nil || tightened {
			t.Errorf("ensureTokenFile = (%v, %v), want (false, nil)", tightened, err)
		}
	})

	t.Run("world readable", func(t *testing.T) {
		path := filepath.Join(dir, "loose")
		if err := os.WriteFile(path, []byte("tok"), 0o644); err != nil {
			t.Fatal(err)
		}
		tightened, err := ensureTokenFile(path)
		if err != nil || !tightened {
			t.Fatalf("ensureTokenFile = (%v, %v), want (true, nil)", tightened, err)
		}
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("mode = %04o, want 0600", st.Mode().Perm())
		}
	})
}

// TestProvideRunNeedsAnOrchestrator: with nothing configured, bare `amux
// provide` must point at the command that configures it rather than dialing
// nothing.
func TestProvideRunNeedsAnOrchestrator(t *testing.T) {
	sandboxCLI(t)
	err := runCommand(t, lookupCommand("provide"), nil)
	if err == nil || !strings.Contains(err.Error(), "amux provide install") {
		t.Errorf("bare `amux provide` = %v, want it to name the install command", err)
	}
}

// TestProvideRunReadsTheConfigFile is the property the user service depends on:
// the unit runs a bare `amux provide`, so everything must come from the file.
func TestProvideRunReadsTheConfigFile(t *testing.T) {
	sandboxCLI(t)
	if err := providercfg.Save(providercfg.Config{
		Orchestrator: "orch:7443", TokenFile: filepath.Join(t.TempDir(), "absent"),
	}); err != nil {
		t.Fatal(err)
	}
	// The address resolves from the file (no "need an orchestrator address"), and
	// the run stops at the unreadable token rather than dialing without one.
	err := runCommand(t, lookupCommand("provide"), nil)
	if err == nil || !strings.Contains(err.Error(), "read token file") {
		t.Errorf("`amux provide` = %v, want it to get past the address and fail on the token", err)
	}
}

// TestProvideUninstallTakesNoArguments keeps a typo'd flag from being read as a
// silent no-op on a command that removes things.
func TestProvideUninstallTakesNoArguments(t *testing.T) {
	sandboxCLI(t)
	if err := cmdProvideUninstall([]string{"--all"}); err == nil {
		t.Error("uninstall accepted an unknown argument")
	}
}
