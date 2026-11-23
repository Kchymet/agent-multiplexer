package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
)

// This file is the CLI's help engine. The command tree itself — every command,
// subcommand, flag, `amux do` action and field, with descriptions and examples —
// is data in helptree.go, and it is the single source of truth for three views:
//
//   - the top-level page (`amux`, `amux -h`, `amux --help`): top-level commands
//     with one-line summaries only;
//   - one level in detail (`amux <path...> --help`): that node's usage,
//     description, subcommands, flags, fields and examples;
//   - the whole tree (`amux --help --all`, `amux -h -a`, `amux --all-help`): every
//     node's detailed page, depth first in declaration order, as plain text — no
//     pager, no color, no daemon — so agents can read or grep it in one call.
//
// The top-level dispatch is the tree too (cmdSpec.run), and every command that
// parses flags with a flag.FlagSet registers them through parseCmdFlags, which
// refuses a FlagSet whose flags differ from the documented ones — so help cannot
// silently drift from what the parser accepts. The remaining hand-rolled parsers
// are pinned by the dispatch contract tests.

// cmdSpec is one node of the command tree.
type cmdSpec struct {
	Name    string
	Aliases []string
	// Args is the operand synopsis after the command path ("<id> <name>"). Empty
	// means "<command>" for a namespace and "[flags]" for a leaf with flags.
	Args string
	// Summary is the one-line description shown in a parent's command list.
	Summary string
	// Detail is the long description shown on this node's own page.
	Detail   string
	Flags    []flagSpec
	Fields   []fieldSpec // `amux do <action>` form fields (-f key=value)
	Examples []string
	Sub      []*cmdSpec
	// Hidden nodes dispatch and answer --help, but are left out of their
	// parent's list and the --all tree (internal or deprecated surfaces).
	Hidden bool
	// FlagSet marks a node whose flags are parsed by a flag.FlagSet registered
	// through parseCmdFlags; the contract test checks the two agree.
	FlagSet bool

	run    func(args []string) error // top-level commands only
	parent *cmdSpec
}

// flagSpec documents one flag.
type flagSpec struct {
	Name     string // long name, without dashes
	Short    string // one-letter alias, without the dash
	Arg      string // value placeholder; empty for a boolean switch
	Default  string // shown as "(default: …)"
	Required bool
	// OneOf names the other flags of a required either/or group; it renders as
	// "(required: one of …)" instead of plain "(required)".
	OneOf  []string
	Repeat bool
	Usage  string
}

// fieldSpec documents one `amux do` form field.
type fieldSpec struct {
	Name     string
	Arg      string // value placeholder
	Required bool
	Usage    string
}

// helpFooter ends the top-level page; tests pin it verbatim.
const helpFooter = "Run 'amux <command> --help' for details. 'amux --help --all' prints everything."

// treeBuilder is buildCLITree, assigned in init so the tree (whose run
// functions reach parseCmdFlags, which reads the tree) is no package-level
// initialization cycle.
var treeBuilder func() *cmdSpec

func init() { treeBuilder = buildCLITree }

var (
	cliRootOnce sync.Once
	cliRootNode *cmdSpec
)

// cliRoot returns the command tree, built once. It is built lazily rather than
// as a package-level var so the run functions it references may themselves use
// the tree without an initialization cycle.
func cliRoot() *cmdSpec {
	cliRootOnce.Do(func() {
		cliRootNode = treeBuilder()
		link(cliRootNode, nil)
	})
	return cliRootNode
}

func link(n, parent *cmdSpec) {
	n.parent = parent
	for _, c := range n.Sub {
		link(c, n)
	}
}

// child returns n's subcommand named name (or aliased to it).
func (n *cmdSpec) child(name string) *cmdSpec {
	for _, c := range n.Sub {
		if c.Name == name || slices.Contains(c.Aliases, name) {
			return c
		}
	}
	return nil
}

// path is the node's full invocation prefix ("amux direct enroll").
func (n *cmdSpec) path() string {
	if n.parent == nil {
		return n.Name
	}
	return n.parent.path() + " " + n.Name
}

// visible is n's subcommands minus hidden ones.
func (n *cmdSpec) visible() []*cmdSpec {
	var out []*cmdSpec
	for _, c := range n.Sub {
		if !c.Hidden {
			out = append(out, c)
		}
	}
	return out
}

// helpNode resolves a command path to its deepest node.
func helpNode(path ...string) *cmdSpec {
	n := cliRoot()
	for _, w := range path {
		c := n.child(w)
		if c == nil {
			break
		}
		n = c
	}
	return n
}

// mustHelpNode is helpNode for code paths that name a node that must exist.
func mustHelpNode(path string) *cmdSpec {
	n := cliRoot()
	for _, w := range strings.Fields(path) {
		c := n.child(w)
		if c == nil {
			panic(fmt.Sprintf("amux: no help node %q", path))
		}
		n = c
	}
	return n
}

// lookupCommand resolves a top-level command word (or alias), nil when amux has
// no such command.
func lookupCommand(name string) *cmdSpec {
	c := cliRoot().child(name)
	if c == nil || c.run == nil {
		return nil
	}
	return c
}

// ---- request parsing -------------------------------------------------------

// helpRequest is what an argv asks of the help engine.
type helpRequest struct {
	node *cmdSpec
	all  bool
}

// isHelpToken reports a -h/--help spelling.
func isHelpToken(a string) bool { return a == "-h" || a == "--help" || a == "-help" }

// parseHelpRequest reports whether argv (os.Args[1:]) asks for help, and for
// which node. Help is requested by -h/--help anywhere before a "--", by
// --all-help, by a leading `help` word (`amux help direct enroll`), or by the
// word `help` in a namespace's subcommand position (`amux direct help`). The
// whole tree is requested by --all-help, or by -a/--all next to -h/--help when
// no command is named (`amux --help --all`, `amux -h -a`); after a command,
// --all keeps its own meaning (`amux agent items --all --help` is items' help).
func parseHelpRequest(args []string) (helpRequest, bool) {
	if len(args) > 0 && args[0] == "help" {
		rest := args[1:]
		all := slices.Contains(rest, "--all") || slices.Contains(rest, "-a") || slices.Contains(rest, "--all-help")
		return helpRequest{node: resolveWords(rest), all: all && leadingWords(rest) == 0}, true
	}
	help, allFlag, allHelp := false, false, false
	for _, a := range args {
		if a == "--" {
			break
		}
		switch {
		case isHelpToken(a):
			help = true
		case a == "--all-help":
			allHelp = true
		case a == "--all" || a == "-a":
			allFlag = true
		}
	}
	if allHelp {
		return helpRequest{node: cliRoot(), all: true}, true
	}
	node := resolveWords(args)
	if help {
		return helpRequest{node: node, all: allFlag && node == cliRoot()}, true
	}
	// `amux <namespace> help`: the word sits where a subcommand would.
	if n := leadingWords(args); n < len(args) && args[n] == "help" && len(node.Sub) > 0 && node.child("help") == nil {
		return helpRequest{node: node}, true
	}
	return helpRequest{}, false
}

// leadingWords counts the leading args that name tree nodes.
func leadingWords(args []string) int {
	n, i := cliRoot(), 0
	for ; i < len(args); i++ {
		c := n.child(args[i])
		if c == nil {
			break
		}
		n = c
	}
	return i
}

// resolveWords resolves the leading command words of args to a node.
func resolveWords(args []string) *cmdSpec {
	return helpNode(args[:leadingWords(args)]...)
}

// ---- rendering --------------------------------------------------------------

// printHelp writes node's page: the short top-level page for the root, the
// detailed page otherwise.
func printHelp(w io.Writer, n *cmdSpec) {
	if n.parent == nil {
		renderTopHelp(w, n)
		return
	}
	renderNode(w, n, true)
}

// renderTopHelp is the short top-level page: commands with one-line summaries.
func renderTopHelp(w io.Writer, root *cmdSpec) {
	fmt.Fprintf(w, "%s — %s\n\nusage: amux [command] [flags]\n\n", root.Name, root.Summary)
	renderCommandList(w, root, false)
	if len(root.Flags) > 0 {
		fmt.Fprintln(w)
		renderFlags(w, "flags:", root.Flags)
	}
	if root.Detail != "" {
		fmt.Fprintf(w, "\n%s\n", strings.TrimRight(root.Detail, "\n"))
	}
	fmt.Fprintf(w, "\n%s\n", helpFooter)
}

// renderCommandList prints the "commands:" block of n's visible subcommands.
// withArgs adds each subcommand's operand synopsis.
func renderCommandList(w io.Writer, n *cmdSpec, withArgs bool) {
	subs := n.visible()
	if len(subs) == 0 {
		return
	}
	var rows [][2]string
	if n.parent == nil {
		rows = append(rows, [2]string{"(bare)", bareSummary})
	}
	for _, c := range subs {
		left := c.Name
		if withArgs && c.Args != "" {
			left += " " + c.Args
		}
		right := c.Summary
		if len(c.Aliases) > 0 {
			right += "  (alias: " + strings.Join(c.Aliases, ", ") + ")"
		}
		rows = append(rows, [2]string{left, right})
	}
	label := "commands:"
	if n.Name == "do" && n.parent != nil && n.parent.parent == nil {
		label = "actions:"
	}
	fmt.Fprintln(w, label)
	renderRows(w, rows)
}

// bareSummary is the top-level page's "(bare)" row.
const bareSummary = "open the workgroup dashboard (native TUI); without a terminal, print this help"

// renderNode is one node's detailed page. footer adds the "Run … --help" hint
// for a namespace (left out of the --all tree, where every page follows).
func renderNode(w io.Writer, n *cmdSpec, footer bool) {
	fmt.Fprintf(w, "%s — %s\n\n", n.path(), n.Summary)
	fmt.Fprintf(w, "usage: %s\n", synopsis(n))
	if len(n.Aliases) > 0 {
		fmt.Fprintf(w, "aliases: %s\n", strings.Join(n.Aliases, ", "))
	}
	if n.Detail != "" {
		fmt.Fprintf(w, "\n%s\n", strings.TrimRight(n.Detail, "\n"))
	}
	if len(n.visible()) > 0 {
		fmt.Fprintln(w)
		renderCommandList(w, n, true)
	}
	if len(n.Flags) > 0 {
		fmt.Fprintln(w)
		renderFlags(w, "flags:", n.Flags)
	}
	if len(n.Fields) > 0 {
		fmt.Fprintln(w)
		renderFields(w, n.Fields)
	}
	if len(n.Examples) > 0 {
		fmt.Fprintln(w, "\nexamples:")
		for _, e := range n.Examples {
			fmt.Fprintf(w, "  %s\n", e)
		}
	}
	if footer && len(n.visible()) > 0 {
		fmt.Fprintf(w, "\nRun '%s <command> --help' for details.\n", n.path())
	}
}

// synopsis is the node's usage line.
func synopsis(n *cmdSpec) string {
	args := n.Args
	if args == "" {
		switch {
		case len(n.Sub) > 0:
			args = "<command>"
		case len(n.Flags) > 0:
			args = "[flags]"
		case len(n.Fields) > 0:
			args = "[-f key=value]..."
		}
	}
	return strings.TrimSpace(n.path() + " " + args)
}

// renderAll writes the entire tree: the root page, then every visible node's
// detailed page, depth first in declaration order.
func renderAll(w io.Writer) {
	root := cliRoot()
	fmt.Fprintln(w, "amux — complete command reference (amux --help --all)")
	fmt.Fprintln(w, "Every command, subcommand, flag and `amux do` action with its fields.")
	fmt.Fprintln(w)
	renderTopHelp(w, root)
	var walk func(n *cmdSpec)
	walk = func(n *cmdSpec) {
		for _, c := range n.visible() {
			fmt.Fprintf(w, "\n==== %s ====\n\n", c.path())
			renderNode(w, c, false)
			walk(c)
		}
	}
	walk(root)
}

// flagLabel is "-t, --target <root>".
func flagLabel(f flagSpec) string {
	var b strings.Builder
	if f.Short != "" {
		b.WriteString("-" + f.Short + ", ")
	}
	b.WriteString("--" + f.Name)
	if f.Arg != "" {
		b.WriteString(" " + f.Arg)
	}
	return b.String()
}

func renderFlags(w io.Writer, title string, flags []flagSpec) {
	fmt.Fprintln(w, title)
	var rows [][2]string
	for _, f := range flags {
		var notes []string
		switch {
		case len(f.OneOf) > 0:
			names := []string{"--" + f.Name}
			for _, o := range f.OneOf {
				names = append(names, "--"+o)
			}
			notes = append(notes, "required: one of "+strings.Join(names, " | "))
		case f.Required:
			notes = append(notes, "required")
		}
		if f.Repeat {
			notes = append(notes, "repeatable")
		}
		usage := f.Usage
		if len(notes) > 0 {
			usage = "(" + strings.Join(notes, "; ") + ") " + usage
		}
		if f.Default != "" {
			sep := " "
			if strings.Contains(usage, "\n") {
				sep = "\n"
			}
			usage += sep + "(default: " + f.Default + ")"
		}
		rows = append(rows, [2]string{flagLabel(f), usage})
	}
	renderRows(w, rows)
}

func renderFields(w io.Writer, fields []fieldSpec) {
	fmt.Fprintln(w, "fields (-f key=value, repeatable):")
	var rows [][2]string
	for _, f := range fields {
		usage := f.Usage
		if f.Required {
			usage = "(required) " + usage
		}
		rows = append(rows, [2]string{f.Name + "=" + f.Arg, usage})
	}
	renderRows(w, rows)
}

// helpColumn caps the left column; a longer label puts its text on the next line.
const helpColumn = 28

// renderRows prints two-column rows: "  left  right", continuation lines of a
// multi-line right side indented under it.
func renderRows(w io.Writer, rows [][2]string) {
	width := 0
	for _, r := range rows {
		if l := len([]rune(r[0])); l > width && l <= helpColumn {
			width = l
		}
	}
	indent := strings.Repeat(" ", 2+width+2)
	for _, r := range rows {
		lines := strings.Split(strings.TrimRight(r[1], "\n"), "\n")
		left := r[0]
		if len([]rune(left)) > width {
			fmt.Fprintf(w, "  %s\n", left)
			for _, l := range lines {
				fmt.Fprintf(w, "%s%s\n", indent, l)
			}
			continue
		}
		fmt.Fprintf(w, "  %-*s  %s\n", width, left, lines[0])
		for _, l := range lines[1:] {
			fmt.Fprintf(w, "%s%s\n", indent, l)
		}
	}
}

// ---- usage errors -------------------------------------------------------------

// usageError marks an error caused by how the command was invoked (a missing
// operand, an unknown subcommand or flag); main prints the relevant level's
// help after it and exits 2.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return usageError{fmt.Sprintf(format, a...)} }

// usageErr is the "usage: <synopsis>" error for the command at path.
func usageErr(path string) error {
	return usageError{"usage: " + synopsis(mustHelpNode(path))}
}

// showHelp prints the help page of the command at path to stdout: what a
// namespace's own help word does.
func showHelp(path string) error {
	printHelp(os.Stdout, mustHelpNode(path))
	return nil
}

// isUsageError recognizes usage errors: the typed ones, flag-package parse
// errors, and the conventional wordings the hand-rolled parsers return.
func isUsageError(err error) bool {
	var ue usageError
	if errors.As(err, &ue) {
		return true
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "usage:") {
		return true
	}
	for _, s := range []string{
		"flag provided but not defined", "flag needs an argument", "invalid value \"",
		"invalid boolean flag", "unexpected argument", "too many arguments",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	if strings.HasPrefix(msg, "unknown ") {
		for _, s := range []string{"subcommand", "command", "flag", "option", "action", "argument"} {
			if strings.Contains(msg, s) {
				return true
			}
		}
	}
	return false
}

// errFirstLine is an error's first line: the short error main prints before the
// level help (the help replaces any usage text the error carried).
func errFirstLine(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	return line
}

// ---- flag sets bound to the tree ------------------------------------------------

// flagSetProbe lets the contract test inspect a command's FlagSet without
// running the command: when set, parseCmdFlags hands it the FlagSet and returns
// errFlagProbe instead of parsing.
var flagSetProbe func(path string, fs *flag.FlagSet)

var errFlagProbe = errors.New("flag probe")

// newCmdFlagSet is a FlagSet for the command at path, silenced: help and
// errors come from the tree, never from the flag package.
func newCmdFlagSet(path string) *flag.FlagSet {
	fs := flag.NewFlagSet("amux "+path, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// bindCmdFlags checks fs against the tree's flags for path (both directions:
// every defined flag is documented, every documented flag is defined) and
// copies the documented usage onto the FlagSet.
func bindCmdFlags(fs *flag.FlagSet, path string) error {
	if flagSetProbe != nil {
		flagSetProbe(path, fs)
		return errFlagProbe
	}
	return checkCmdFlags(fs, mustHelpNode(path))
}

func checkCmdFlags(fs *flag.FlagSet, n *cmdSpec) error {
	documented := map[string]flagSpec{}
	for _, f := range n.Flags {
		documented[f.Name] = f
		if f.Short != "" {
			documented[f.Short] = f
		}
	}
	var problems []string
	fs.VisitAll(func(fl *flag.Flag) {
		spec, ok := documented[fl.Name]
		if !ok {
			problems = append(problems, "-"+fl.Name+" is parsed but not documented")
			return
		}
		fl.Usage = spec.Usage
	})
	for _, f := range n.Flags {
		if fs.Lookup(f.Name) == nil {
			problems = append(problems, "--"+f.Name+" is documented but not parsed")
		}
		if f.Short != "" && fs.Lookup(f.Short) == nil {
			problems = append(problems, "-"+f.Short+" is documented but not parsed")
		}
	}
	if len(problems) > 0 {
		sort := slices.Clone(problems)
		slices.Sort(sort)
		return fmt.Errorf("internal: %s flags drifted from help: %s", n.path(), strings.Join(sort, "; "))
	}
	return nil
}

// parseCmdFlags checks fs against the tree and parses args.
func parseCmdFlags(fs *flag.FlagSet, path string, args []string) error {
	if err := bindCmdFlags(fs, path); err != nil {
		return err
	}
	return fs.Parse(args)
}

// parseCmdFlagsAnyOrder is parseFlagsAnyOrder after the same check.
func parseCmdFlagsAnyOrder(fs *flag.FlagSet, path string, args []string) ([]string, error) {
	if err := bindCmdFlags(fs, path); err != nil {
		return nil, err
	}
	return parseFlagsAnyOrder(fs, args)
}

// ---- entry point ------------------------------------------------------------------

// runCLI is main's body, returning the process exit code.
func runCLI(args []string, stdout, stderr io.Writer, isTTY func() bool) int {
	if req, ok := parseHelpRequest(args); ok {
		if req.all {
			renderAll(stdout)
		} else {
			printHelp(stdout, req.node)
		}
		return 0
	}
	if len(args) == 0 {
		if !isTTY() {
			// No terminal for the dashboard (an agent, a script, a pipe): the
			// useful answer is the command list.
			printHelp(stdout, cliRoot())
			return 0
		}
		if err := cmdNative(""); err != nil {
			fmt.Fprintln(stderr, "amux:", err)
			return 1
		}
		return 0
	}
	c := lookupCommand(args[0])
	if c == nil {
		fmt.Fprintf(stderr, "amux: unknown command %q\n\n", args[0])
		printHelp(stderr, cliRoot())
		return 2
	}
	if err := c.run(args[1:]); err != nil {
		if isUsageError(err) {
			fmt.Fprintln(stderr, "amux:", errFirstLine(err))
			fmt.Fprintln(stderr)
			renderNode(stderr, resolveWords(args), true)
			return 2
		}
		fmt.Fprintln(stderr, "amux:", err)
		var coded interface{ ExitCode() int }
		if errors.As(err, &coded) {
			return coded.ExitCode()
		}
		return 1
	}
	return 0
}

// stdioIsTerminal reports whether both stdin and stdout are terminals — what
// the bare dashboard needs.
func stdioIsTerminal() bool { return isTerminal(os.Stdin) && isTerminal(os.Stdout) }
