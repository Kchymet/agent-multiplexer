package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/daemon"
)

// The CLI's dispatch contract: everything the help tree (helptree.go) advertises
// is actually dispatched, and every advertised subcommand survives being invoked
// with no arguments and with one. Both halves are bugs we shipped — `workgroup
// open <id>` was in `amux help` but fell through to "unknown session
// subcommand", and bare `amux workgroup` panicked slicing an empty argv — and
// both are the kind that only surface when a user types the command, since
// nothing else walks the surface. The top-level dispatch is the tree itself, so
// only the namespaces' own subcommand dispatch can drift from it.

// verbTables are the namespaces whose subcommands are not safe to invoke in a
// test — daemon's verbs drive real processes, provide install writes a user
// service, harness downloads releases — so their advertised subcommands are
// checked by name against the dispatch table, in both directions.
func verbTables() map[string]map[string]bool {
	return map[string]map[string]bool{
		"harness auth": verbNames(authSubcommands),
		"daemon":       verbNames(daemonSubcommands),
		"cloud":        verbNames(cloudSubcommands),
		"device":       verbNames(deviceSubcommands),
		"harness":      verbNames(harnessSubcommands),
	}
}

// verbNames is the set of verbs a dispatch table accepts.
func verbNames[V any](table map[string]V) map[string]bool {
	out := make(map[string]bool, len(table))
	for k := range table {
		if k != "" {
			out[k] = true
		}
	}
	return out
}

// TestVerbTablesMatchTree: a table-dispatched namespace documents every verb it
// accepts, and accepts every verb it documents (names and aliases alike).
func TestVerbTablesMatchTree(t *testing.T) {
	for cmd, verbs := range verbTables() {
		n := mustHelpNode(cmd)
		documented := map[string]bool{}
		for _, c := range n.Sub {
			for _, name := range append([]string{c.Name}, c.Aliases...) {
				documented[name] = true
				if !verbs[name] {
					t.Errorf("help advertises `amux %s %s` but %s does not dispatch it", cmd, name, cmd)
				}
			}
		}
		for v := range verbs {
			if !documented[v] {
				t.Errorf("amux %s dispatches %q but its help never mentions it", cmd, v)
			}
		}
	}
}

// TestAdvertisedSubcommandsDispatch invokes every subcommand of the
// switch-dispatched namespaces, with no arguments and with one, and asserts the
// namespace accepts it: returning an error is fine (the daemon is stubbed
// offline), panicking or "unknown subcommand" is not.
func TestAdvertisedSubcommandsDispatch(t *testing.T) {
	sandboxCLI(t)
	tables := verbTables()
	for _, c := range cliRoot().Sub {
		if len(c.Sub) == 0 || tables[c.Name] != nil {
			continue
		}
		for _, sub := range c.Sub {
			for _, name := range append([]string{sub.Name}, sub.Aliases...) {
				for _, args := range [][]string{{name}, {name, "zzz-nonexistent"}} {
					t.Run(c.Name+" "+strings.Join(args, " "), func(t *testing.T) {
						if err := runCommand(t, c, args); err != nil && isUnknownSubcommand(err) {
							t.Errorf("amux %s %s: %v", c.Name, strings.Join(args, " "), err)
						}
					})
				}
			}
		}
	}
}

// TestBareWorkgroupIsTheCreatePage pins the fix for the bare `amux workgroup`
// crash: the subcommand defaulted to "new" and then sliced args[1:] on the empty
// argv, panicking before it ever reached the create page.
func TestBareWorkgroupIsTheCreatePage(t *testing.T) {
	sandboxCLI(t)
	c := lookupCommand("workgroup")
	for _, args := range [][]string{nil, {}} {
		err := runCommand(t, c, args)
		// Without a terminal the create page can't be drawn, so it says so —
		// the point is that it got there instead of panicking.
		if err == nil || isUnknownSubcommand(err) {
			t.Errorf("bare `amux workgroup` = %v, want the create page's terminal error", err)
		}
	}
}

// TestWorkgroupOpenNeedsAnID pins that the `open` verb reports its own usage
// when given no id, rather than opening the dashboard on nothing.
func TestWorkgroupOpenNeedsAnID(t *testing.T) {
	sandboxCLI(t)
	err := runCommand(t, lookupCommand("workgroup"), []string{"open"})
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Errorf("`amux open` = %v, want a usage error", err)
	}
}

// TestNamespaceHelpWord keeps `amux <cmd> help` (and -h/--help as the first
// argument) answered by the namespace itself, as it was before help moved to
// the tree: a direct call of the run function, not only through main.
func TestNamespaceHelpWord(t *testing.T) {
	sandboxCLI(t)
	for _, name := range []string{"repo", "workgroup", "do", "config", "agent", "inbox", "daemon", "cloud", "device", "harness"} {
		c := lookupCommand(name)
		for _, flag := range []string{"--help", "-h", "help"} {
			out, err := captureOutput(t, func() error { return c.run([]string{flag}) })
			if err != nil {
				t.Errorf("amux %s %s = %v, want no error", name, flag, err)
			}
			if !strings.Contains(out, "usage: amux "+name) {
				t.Errorf("amux %s %s printed %q, want its own help", name, flag, out)
			}
		}
	}
}

// ---- helpers -------------------------------------------------------------

// runCommand invokes a command with args, silencing its output and turning a
// panic into a test failure (a panicking CLI is the bug this file guards).
func runCommand(t *testing.T, c *cmdSpec, args []string) (err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("amux %s %s panicked: %v", c.Name, strings.Join(args, " "), r)
		}
	}()
	_, err = captureOutput(t, func() error { return c.run(args) })
	return err
}

// isUnknownSubcommand reports whether an error is the dispatch rejecting a verb
// it doesn't know — the failure mode `workgroup open` had.
func isUnknownSubcommand(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "unknown") && strings.Contains(msg, "command")
}

// captureOutput runs fn with stdout and stderr redirected to a temp file and
// returns what it wrote. A file (not a pipe) so a chatty command can't fill a
// buffer and deadlock.
func captureOutput(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	f, ferr := os.CreateTemp(t.TempDir(), "out")
	if ferr != nil {
		t.Fatal(ferr)
	}
	defer os.Remove(f.Name())
	outBak, errBak := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = f, f
	err := func() error {
		defer func() { os.Stdout, os.Stderr = outBak, errBak }()
		return fn()
	}()
	_ = f.Close()
	b, rerr := os.ReadFile(f.Name())
	if rerr != nil {
		t.Fatal(rerr)
	}
	return string(b), err
}

// sandboxCLI isolates a test that drives real commands: every path amux touches
// moves to a temp home, the daemon connection is cut (so no command reaches — or
// spawns — the developer's own daemon), stdin becomes /dev/null, and the CLI is
// told it has no terminal so the interactive create pages report that instead of
// opening an fzf screen. The fixed session authority — which HOME/XDG cannot
// redirect, and which a suite run inside an amux worker inherits — is replaced
// by the fail-closed fake from session_isolation_test.go, so a real `agent
// done`/`agent name`/`do` verb can never reach the worker's own mailbox.
func sandboxCLI(t *testing.T) *isolatedSessionAuthority {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", home+"/data")
	t.Setenv("XDG_CONFIG_HOME", home+"/config")
	t.Setenv("XDG_RUNTIME_DIR", home+"/run")
	t.Setenv("AMUX_SOCK", home+"/run/absent.sock")
	t.Setenv("AMUX_WORKGROUP", "")
	t.Setenv("AMUX_WORKSPACE", "")
	t.Setenv("AMUX_SESSION_ID", "")
	t.Setenv(core.SessionAccessEnv, "")
	authority := isolateSessionAuthority(t)

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	stdinBak, dialBak, ttyBak := os.Stdin, dial, stdinIsTerminal
	startupAuthBak := protectedHostStartup
	os.Stdin = devNull
	dial = func() (*daemon.Client, error) { return nil, errors.New("daemon offline (test)") }
	stdinIsTerminal = func() bool { return false }
	protectedHostStartup = func() error { return nil }
	t.Cleanup(func() {
		os.Stdin, dial, stdinIsTerminal = stdinBak, dialBak, ttyBak
		protectedHostStartup = startupAuthBak
		_ = devNull.Close()
	})
	return authority
}
