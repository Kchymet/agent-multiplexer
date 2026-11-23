package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"amux/internal/core"
)

var updateGolden = flag.Bool("update", false, "rewrite the help golden files in testdata/help")

// walkTree visits every node below the root, hidden ones included.
func walkTree(fn func(n *cmdSpec)) {
	var walk func(n *cmdSpec)
	walk = func(n *cmdSpec) {
		for _, c := range n.Sub {
			fn(c)
			walk(c)
		}
	}
	walk(cliRoot())
}

// words is a node's command path without the leading "amux".
func words(n *cmdSpec) []string { return strings.Fields(n.path())[1:] }

// runHelp runs the CLI entry point and returns its exit code and output.
func runHelp(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := runCLI(args, &out, &errOut, func() bool { return false })
	return code, out.String(), errOut.String()
}

// TestEveryLevelAnswersHelp: -h and --help work at every level — namespaces,
// leaves, do actions, hidden commands — print that level's page on stdout and
// exit 0, never "unknown flag".
func TestEveryLevelAnswersHelp(t *testing.T) {
	sandboxCLI(t)
	walkTree(func(n *cmdSpec) {
		for _, h := range []string{"-h", "--help"} {
			args := append(words(n), h)
			code, out, errOut := runHelp(t, args...)
			if code != 0 || errOut != "" {
				t.Errorf("amux %s = exit %d, stderr %q", strings.Join(args, " "), code, errOut)
			}
			if !strings.HasPrefix(out, n.path()+" — ") || !strings.Contains(out, "usage: "+n.path()) {
				t.Errorf("amux %s printed %q, want %s's own page", strings.Join(args, " "), out, n.path())
			}
		}
	})
}

// TestHelpAfterOperands: --help after operands and flags still answers with the
// level the leading words name.
func TestHelpAfterOperands(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"do", "new-workgroup", "-f", "name=x", "--help"}, "amux do new-workgroup — "},
		{[]string{"workgroup", "rename", "3f7a", "-h"}, "amux workgroup rename — "},
		{[]string{"agent", "items", "--all", "--help"}, "amux agent items — "},
		{[]string{"help", "direct", "enroll"}, "amux direct enroll — "},
		{[]string{"direct", "help"}, "amux direct — "},
	} {
		code, out, _ := runHelp(t, tc.args...)
		if code != 0 || !strings.HasPrefix(out, tc.want) {
			t.Errorf("amux %s = exit %d, %q; want a page starting %q", strings.Join(tc.args, " "), code, firstOf(out), tc.want)
		}
	}
}

func firstOf(s string) string { l, _, _ := strings.Cut(s, "\n"); return l }

// TestTopLevelIsShort: the top-level page lists top-level commands only, ends
// with the exact footer, and is what -h, --help, help and a terminal-less bare
// amux all print.
func TestTopLevelIsShort(t *testing.T) {
	_, want, _ := runHelp(t, "--help")
	for _, args := range [][]string{{}, {"-h"}, {"help"}} {
		code, out, _ := runHelp(t, args...)
		if code != 0 || out != want {
			t.Errorf("amux %v = exit %d, differs from amux --help", args, code)
		}
	}
	if !strings.HasSuffix(want, "\n"+helpFooter+"\n") {
		t.Errorf("top-level help does not end with the footer:\n%s", want)
	}
	for _, sub := range []string{"enroll", "new-workgroup", "agent raise", "--permissions"} {
		if strings.Contains(want, sub) {
			t.Errorf("top-level help mentions %q; it should list top-level commands only", sub)
		}
	}
	for _, c := range cliRoot().visible() {
		if !strings.Contains(want, "\n  "+c.Name+" ") {
			t.Errorf("top-level help does not list %s", c.Name)
		}
	}
}

// TestAllHelpCoversEverything: every visible command, subcommand, flag, do
// action and do field appears in --help --all, and the three spellings print
// the same bytes.
func TestAllHelpCoversEverything(t *testing.T) {
	_, all, _ := runHelp(t, "--help", "--all")
	for _, alt := range [][]string{{"-h", "-a"}, {"--all-help"}, {"help", "--all"}} {
		if _, out, _ := runHelp(t, alt...); out != all {
			t.Errorf("amux %v differs from amux --help --all", alt)
		}
	}
	walkTree(func(n *cmdSpec) {
		if hiddenPath(n) {
			if strings.Contains(all, "==== "+n.path()+" ====") {
				t.Errorf("--all lists hidden %s", n.path())
			}
			return
		}
		if !strings.Contains(all, "\n==== "+n.path()+" ====\n") {
			t.Errorf("--all has no page for %s", n.path())
		}
		for _, f := range n.Flags {
			if !strings.Contains(all, flagLabel(f)) {
				t.Errorf("--all does not document %s %s", n.path(), flagLabel(f))
			}
		}
		for _, f := range n.Fields {
			if !strings.Contains(all, "  "+f.Name+"="+f.Arg) {
				t.Errorf("--all does not document %s field %s", n.path(), f.Name)
			}
		}
	})
	for _, a := range core.ControlActions() {
		if !strings.Contains(all, "==== amux do "+a+" ====") {
			t.Errorf("--all has no page for do action %s", a)
		}
	}
	if strings.Contains(all, "\x1b") {
		t.Error("--all contains terminal escapes")
	}
}

func hiddenPath(n *cmdSpec) bool {
	for ; n != nil; n = n.parent {
		if n.Hidden {
			return true
		}
	}
	return false
}

// TestDoActionsMatchVocabulary: core.ControlActions() is the source of which
// verbs exist; every one is documented, and nothing else is.
func TestDoActionsMatchVocabulary(t *testing.T) {
	do := mustHelpNode("do")
	for _, a := range core.ControlActions() {
		if do.child(a) == nil {
			t.Errorf("do action %q has no entry in doActionSpecs", a)
		}
	}
	for a := range doActionSpecs() {
		if !core.KnownAction(a) {
			t.Errorf("doActionSpecs documents %q, which is not in core.ControlActions()", a)
		}
	}
}

// TestTreeIsWellFormed: one-line summaries, no duplicate names or aliases among
// siblings, no duplicate flags on a node.
func TestTreeIsWellFormed(t *testing.T) {
	walkTree(func(n *cmdSpec) {
		if strings.TrimSpace(n.Summary) == "" || strings.Contains(n.Summary, "\n") {
			t.Errorf("%s: summary must be one non-empty line", n.path())
		}
		seen := map[string]bool{}
		for _, c := range n.Sub {
			for _, name := range append([]string{c.Name}, c.Aliases...) {
				if seen[name] {
					t.Errorf("%s: %q is used twice among its subcommands", n.path(), name)
				}
				seen[name] = true
			}
		}
		flags := map[string]bool{}
		for _, f := range n.Flags {
			for _, name := range []string{f.Name, f.Short} {
				if name != "" && flags[name] {
					t.Errorf("%s: flag %q documented twice", n.path(), name)
				}
				flags[name] = true
			}
		}
	})
}

// TestFlagSetsMatchHelp drives every FlagSet-parsed command up to its parse and
// checks the flags it defines against the tree, both directions, plus any
// non-zero default. Help therefore cannot drift from what the parser accepts.
func TestFlagSetsMatchHelp(t *testing.T) {
	sandboxCLI(t)
	restrictedBak := sessionContextRestricted
	sessionContextRestricted = func() bool { return false }
	t.Cleanup(func() { sessionContextRestricted = restrictedBak; flagSetProbe = nil })
	walkTree(func(n *cmdSpec) {
		if !n.FlagSet {
			return
		}
		path := strings.Join(words(n), " ")
		var got *flag.FlagSet
		flagSetProbe = func(p string, fs *flag.FlagSet) {
			if p == path {
				got = fs
			}
		}
		args := words(n)
		if path == "harness install" {
			args = append(args, "claude")
		}
		c := lookupCommand(args[0])
		_, err := captureOutput(t, func() error { return c.run(args[1:]) })
		flagSetProbe = nil
		if got == nil {
			t.Errorf("%s is marked FlagSet but never reached parseCmdFlags(%q) (err %v)", n.path(), path, err)
			return
		}
		if !errors.Is(err, errFlagProbe) {
			t.Errorf("%s: want the probe to stop the command, got %v", n.path(), err)
		}
		if err := checkCmdFlags(got, n); err != nil {
			t.Error(err)
		}
		for _, f := range n.Flags {
			fl := got.Lookup(f.Name)
			if fl == nil {
				continue
			}
			switch fl.DefValue {
			case "", "0", "0s", "false", "[]":
			default:
				if fl.DefValue != f.Default {
					t.Errorf("%s --%s: parser default %q, help says %q", n.path(), f.Name, fl.DefValue, f.Default)
				}
			}
		}
	})
}

// TestUsageErrorsShowTheirLevel: a wrong command or missing operand prints a
// short error and that level's help on stderr, exits 2, and never dumps the
// whole tree.
func TestUsageErrorsShowTheirLevel(t *testing.T) {
	sandboxCLI(t)
	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"bogus"}, []string{`amux: unknown command "bogus"`, helpFooter}},
		{[]string{"repo", "rm"}, []string{"amux: usage: amux repo rm <name>", "amux repo rm — "}},
		{[]string{"repo", "zap"}, []string{`unknown repo subcommand "zap"`, "amux repo — ", "Run 'amux repo <command> --help'"}},
		{[]string{"agent", "wait"}, []string{"usage: amux agent wait", "--timeout"}},
		{[]string{"do", "zap"}, []string{`unknown action "zap"`, "amux do — ", "new-workgroup"}},
		{[]string{"top", "--zap"}, []string{"flag provided but not defined", "amux top — "}},
	} {
		code, out, errOut := runHelp(t, tc.args...)
		if code != 2 || out != "" {
			t.Errorf("amux %s = exit %d stdout %q, want exit 2 and help on stderr", strings.Join(tc.args, " "), code, out)
		}
		for _, w := range tc.want {
			if !strings.Contains(errOut, w) {
				t.Errorf("amux %s stderr lacks %q:\n%s", strings.Join(tc.args, " "), w, errOut)
			}
		}
		if strings.Contains(errOut, "==== ") {
			t.Errorf("amux %s printed the whole tree", strings.Join(tc.args, " "))
		}
	}
}

// TestHelpGolden pins the rendered pages most often read: the top level, a
// FlagSet leaf and a do action. Regenerate with go test ./cmd/amux -run
// TestHelpGolden -update.
func TestHelpGolden(t *testing.T) {
	for name, args := range map[string][]string{
		"top":              {"--help"},
		"direct-enroll":    {"direct", "enroll", "--help"},
		"do-new-workgroup": {"do", "new-workgroup", "--help"},
		"workgroup":        {"workgroup", "--help"},
		"agent-raise":      {"agent", "raise", "-h"},
		"direct":           {"direct", "-h"},
	} {
		_, out, _ := runHelp(t, args...)
		path := filepath.Join("testdata", "help", name+".golden")
		if *updateGolden {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update to create it)", err)
		}
		if out != string(want) {
			t.Errorf("amux %s drifted from %s (run with -update if intended)\n--- got\n%s", strings.Join(args, " "), path, out)
		}
	}
}
