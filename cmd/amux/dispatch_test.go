package main

import (
	"errors"
	"os"
	"strings"
	"testing"

	"amux/internal/daemon"
)

// The CLI's dispatch contract: everything amux advertises in its help text is
// actually dispatched, and every advertised subcommand survives being invoked
// with no arguments and with one. Both halves are bugs we shipped — `workgroup
// open <id>` was in `amux help` but fell through to "unknown session
// subcommand", and bare `amux workgroup` panicked slicing an empty argv — and
// both are the kind that only surface when a user types the command, since
// nothing else walks the surface.

// helpBlock is one help text of the advertised surface. cmd is the command whose
// subcommands the block's entries are ("" for the top-level usage, whose entries
// are commands themselves). invoke says whether the contract test may call those
// subcommands for real.
type helpBlock struct {
	cmd    string
	usage  func()
	invoke bool
	// verbs is the dispatch table checked by name for a block that is not safe to
	// invoke — `daemon`'s verbs drive real processes, and `provide install` writes
	// a system service. Their advertised subcommands are still checked, just
	// against the table instead of by running them.
	verbs map[string]bool
}

// helpBlocks is the advertised surface: the top-level usage plus each command
// namespace's own --help. The invokable ones are pure daemon clients that fail
// fast against the stubbed dial; `daemon`'s verbs run and stop real processes, so
// its dispatch is checked by name against daemonSubcommands instead.
var helpBlocks = []helpBlock{
	{cmd: "", usage: usage},
	{cmd: "repo", usage: repoUsage, invoke: true},
	{cmd: "workgroup", usage: workgroupUsage, invoke: true},
	{cmd: "do", usage: doUsage, invoke: true},
	{cmd: "config", usage: configUsage, invoke: true},
	{cmd: "agent", usage: agentUsage, invoke: true},
	{cmd: "daemon", usage: daemonUsage, verbs: verbNames(daemonSubcommands)},
	{cmd: "provide", usage: provideUsage, verbs: verbNames(provideSubcommands)},
}

// verbNames is the set of verbs a dispatch table accepts, for the blocks the
// contract test checks by name rather than by invocation.
func verbNames[V any](table map[string]V) map[string]bool {
	out := make(map[string]bool, len(table))
	for k := range table {
		out[k] = true
	}
	return out
}

// TestAdvertisedCommandsDispatch walks every command named in the top-level
// usage() and asserts amux actually dispatches it.
func TestAdvertisedCommandsDispatch(t *testing.T) {
	for _, e := range parseHelp(t, usage) {
		if lookupCommand(e.name) == nil {
			t.Errorf("usage() advertises %q (%q) but no command dispatches it", e.name, e.line)
		}
	}
}

// TestDispatchedCommandsAreAdvertised is the same contract read backwards: a
// command the binary accepts is documented, unless it is deliberately hidden
// (internal, deprecated, or self-evident).
func TestDispatchedCommandsAreAdvertised(t *testing.T) {
	advertised := map[string]bool{}
	for _, e := range parseHelp(t, usage) {
		advertised[e.name] = true
	}
	for _, c := range commands {
		if c.hidden || advertised[c.names[0]] {
			continue
		}
		t.Errorf("amux dispatches %q but usage() never mentions it", c.names[0])
	}
}

// TestAdvertisedSubcommandsDispatch walks every subcommand amux advertises —
// those named on a top-level usage() line ("workgroup open <id>") and those in a
// namespace's own --help — and asserts the namespace accepts it, called with no
// arguments and with one, returning an error rather than panicking or falling
// through to "unknown subcommand". A nil error is fine where the verb
// legitimately no-ops (an agent self-report with nothing to report); what must
// not happen is a crash or a verb that is documented but unroutable.
func TestAdvertisedSubcommandsDispatch(t *testing.T) {
	sandboxCLI(t)
	for _, p := range advertisedSubcommands(t) {
		c := lookupCommand(p.cmd)
		if c == nil {
			t.Errorf("help advertises %q but no command dispatches %q", p.cmd+" "+p.sub, p.cmd)
			continue
		}
		b, ok := helpBlockFor(p.cmd)
		if !ok {
			t.Errorf("%q advertises subcommands but has no --help block registered in helpBlocks", p.cmd)
			continue
		}
		if !b.invoke {
			// Not safe to run — `daemon start|stop|restart` drives real processes and
			// `provide install` writes a user service — so check the dispatch table by
			// name instead.
			if !b.verbs[p.sub] {
				t.Errorf("help advertises `amux %s %s` but %s does not dispatch it", p.cmd, p.sub, p.cmd)
			}
			continue
		}
		for _, args := range [][]string{{p.sub}, {p.sub, "zzz-nonexistent"}} {
			t.Run(p.cmd+" "+strings.Join(args, " "), func(t *testing.T) {
				if err := runCommand(t, c, args); err != nil && isUnknownSubcommand(err) {
					t.Errorf("amux %s %s: %v", p.cmd, strings.Join(args, " "), err)
				}
			})
		}
	}
}

// subcommandRef is one advertised (command, subcommand) pair.
type subcommandRef struct{ cmd, sub string }

// advertisedSubcommands collects every subcommand the help texts name, from the
// top-level usage() lines and from each namespace's own help, in order and
// without duplicates.
func advertisedSubcommands(t *testing.T) []subcommandRef {
	t.Helper()
	var pairs []subcommandRef
	seen := map[subcommandRef]bool{}
	add := func(p subcommandRef) {
		if !seen[p] {
			seen[p] = true
			pairs = append(pairs, p)
		}
	}
	for _, b := range helpBlocks {
		for _, e := range parseHelp(t, b.usage) {
			if b.cmd == "" {
				// A top-level line: the entry names the command, and any literal
				// words after it are its subcommands ("repo ls | rm").
				for _, sub := range e.subs {
					add(subcommandRef{cmd: e.name, sub: sub})
				}
				continue
			}
			add(subcommandRef{cmd: b.cmd, sub: e.name})
		}
	}
	return pairs
}

// helpBlockFor returns the help block a command namespace publishes.
func helpBlockFor(cmd string) (helpBlock, bool) {
	for _, b := range helpBlocks {
		if b.cmd == cmd {
			return b, true
		}
	}
	return helpBlock{}, false
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

// TestWorkgroupOpenNeedsAnID pins that the newly implemented `open` verb reports
// its own usage when given no id, rather than opening the dashboard on nothing.
func TestWorkgroupOpenNeedsAnID(t *testing.T) {
	sandboxCLI(t)
	err := runCommand(t, lookupCommand("workgroup"), []string{"open"})
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Errorf("`amux workgroup open` = %v, want a usage error", err)
	}
}

// TestNamespaceHelp asserts every command namespace answers --help/-h/help by
// printing its own help and exiting cleanly.
func TestNamespaceHelp(t *testing.T) {
	sandboxCLI(t)
	for _, b := range helpBlocks {
		if b.cmd == "" {
			continue
		}
		c := lookupCommand(b.cmd)
		for _, flag := range []string{"--help", "-h", "help"} {
			out, err := captureOutput(t, func() error { return c.run([]string{flag}) })
			if err != nil {
				t.Errorf("amux %s %s = %v, want no error", b.cmd, flag, err)
			}
			if !strings.Contains(out, "usage: amux "+b.cmd) {
				t.Errorf("amux %s %s printed %q, want its own help", b.cmd, flag, out)
			}
		}
	}
}

// ---- helpers -------------------------------------------------------------

// helpEntry is one line of a help block: the word it names (a command in the
// top-level block, a subcommand in a namespace block), any further literal words
// on the line — the subcommands of a top-level entry, as in "repo ls | rm" or
// "daemon [stop|start|restart]" — and the raw line for failure messages.
type helpEntry struct {
	name string
	subs []string
	line string
}

// parseHelp captures a help text and reads the command words out of its entry
// block — the indented lines between "usage: amux …" and the prose that follows.
// Each entry is "  <spec>  <description>": two or more spaces separate the
// invocation from its prose, which is how the first word of the description is
// told apart from a subcommand. Continuation lines (indented further) and
// argument placeholders (<id>, [--json], …) are skipped.
func parseHelp(t *testing.T, print func()) []helpEntry {
	t.Helper()
	text, _ := captureOutput(t, func() error { print(); return nil })

	var entries []helpEntry
	started := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "usage: amux") {
			started = true
			continue
		}
		if !started {
			continue
		}
		if strings.TrimSpace(line) == "" {
			if len(entries) > 0 {
				break // the entry block ended
			}
			continue // the blank line right after "usage:"
		}
		if !strings.HasPrefix(line, "  ") {
			break // prose after the entries
		}
		if strings.HasPrefix(line, "   ") {
			continue // a description continued onto its own line
		}
		spec, desc, ok := strings.Cut(strings.TrimLeft(line, " "), "  ")
		if !ok || strings.TrimSpace(desc) == "" {
			t.Fatalf("help line %q is not %q — the contract test needs two or more "+
				"spaces between an entry and its description", line, "  <spec>  <description>")
		}
		if e, ok := parseSpec(spec); ok {
			e.line = strings.TrimSpace(line)
			entries = append(entries, e)
		}
	}
	return entries
}

// parseSpec reads one entry's invocation spec. The first word names the entry
// ("(bare)" names nothing, so the line is dropped); every later literal word is
// a subcommand of it — including the members of an alternation, so
// "daemon [stop|start|restart]" names three and "repo ls | rm" two. Argument
// placeholders (<id>, [--json], --new, "...") name nothing.
func parseSpec(spec string) (helpEntry, bool) {
	fields := strings.Fields(spec)
	if len(fields) == 0 || !isCommandWord(fields[0]) {
		return helpEntry{}, false
	}
	e := helpEntry{name: fields[0]}
	for _, tok := range fields[1:] {
		for _, alt := range strings.Split(strings.Trim(tok, "[]"), "|") {
			if isCommandWord(alt) {
				e.subs = append(e.subs, alt)
			}
		}
	}
	return e, true
}

// isCommandWord reports whether a spec token is a literal command word rather
// than a placeholder, a flag, or punctuation.
func isCommandWord(tok string) bool {
	if tok == "" {
		return false
	}
	for _, r := range tok {
		if (r < 'a' || r > 'z') && r != '-' {
			return false
		}
	}
	return !strings.HasPrefix(tok, "-")
}

// runCommand invokes a command with args, silencing its output and turning a
// panic into a test failure (a panicking CLI is the bug this file guards).
func runCommand(t *testing.T, c *command, args []string) (err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("amux %s %s panicked: %v", c.names[0], strings.Join(args, " "), r)
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
// opening an fzf screen.
func sandboxCLI(t *testing.T) {
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

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	stdinBak, dialBak, ttyBak := os.Stdin, dial, stdinIsTerminal
	os.Stdin = devNull
	dial = func() (*daemon.Client, error) { return nil, errors.New("daemon offline (test)") }
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() {
		os.Stdin, dial, stdinIsTerminal = stdinBak, dialBak, ttyBak
		_ = devNull.Close()
	})
}
