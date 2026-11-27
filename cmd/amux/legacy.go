package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
)

// Old spellings. The CLI's surface was redesigned around plain verbs (new, ls,
// open, send, add, archive) and a few nouns (inbox, repo, device, cloud,
// harness); every spelling it replaced keeps working. A pure rename is not a
// tree node: rewriteLegacy maps the old leading words to the new ones before
// help and dispatch, so an old spelling reaches exactly the new handler and
// --help on it shows the new page. The few old forms whose operands differ
// (amux workgroup create/repo/add/new) still run their own handler under the
// hidden workgroup node and only get a hint.
//
// On a terminal the rewrite prints one "now: amux ..." line on stderr. Pipes,
// scripts and agents' tool calls have no terminal on stderr, so their output
// is unchanged.

// legacyRule maps old leading words to new ones. A nil to keeps the argv and
// only hints (the form still runs its legacy handler).
type legacyRule struct {
	from []string
	to   []string
	// exact matches only an argv that is exactly from (bare amux workgroup).
	exact bool
	// hint replaces the derived "now:" text.
	hint string
}

// legacyRules, most specific first (rewriteLegacy takes the first match).
var legacyRules = func() []legacyRule {
	var rules []legacyRule
	add := func(from, to string) {
		rules = append(rules, legacyRule{from: strings.Fields(from), to: strings.Fields(to)})
	}
	for _, wg := range []string{"workgroup", "wg"} {
		rules = append(rules, legacyRule{from: []string{wg}, to: []string{"new"}, exact: true})
		for _, r := range [][2]string{
			{"ls", "ls"}, {"list", "ls"},
			{"open", "open"}, {"switch", "open"},
			{"archive", "archive"}, {"done", "archive"},
			{"unarchive", "unarchive"}, {"restore", "unarchive"},
			{"rename", "rename"},
			{"rm", "rm"}, {"delete", "rm"},
			{"move", "move"},
			{"recreate", "recreate"},
			{"repos", "repo set"},
			{"grants", "repo grant"},
		} {
			add(wg+" "+r[0], r[1])
		}
		for _, r := range [][2]string{
			{"new", `amux new  (or amux new --repo <repo> "<task>")`},
			{"create", `amux new --repo <repo> "<task>"`},
			{"repo", `amux new --repo <repo> "<task>"`},
			{"add", `amux add <workgroup> "<task>" [--repo <repo>]`},
		} {
			rules = append(rules, legacyRule{from: []string{wg, r[0]}, hint: r[1]})
		}
	}
	add("status", "ls")
	for _, q := range []string{"queue", "q"} {
		add(q+" list", "inbox ls")
		add(q+" reply", "inbox answer")
		add(q, "inbox")
	}
	// amux question (workgroup 7bead5) moved under amux inbox before release.
	for _, r := range [][2]string{
		{"list", "inbox questions"}, {"ls", "inbox questions"},
		{"show", "inbox show"}, {"answer", "inbox answer"},
		{"cancel", "inbox cancel"}, {"dismiss", "inbox cancel"},
		{"release", "inbox release"},
	} {
		add("question "+r[0], r[1])
	}
	add("question", "inbox questions")
	add("direct enroll", "device connect")
	add("direct clients list", "device ls")
	add("direct clients ls", "device ls")
	add("direct clients", "device") // revoke, and update (workgroup 8406c5)
	add("direct cloud", "cloud enroll")
	add("direct wake", "cloud wake")
	add("direct", "device")
	add("provide install", "cloud connect")
	add("provide uninstall", "cloud disconnect")
	add("provide revoke", "cloud revoke")
	add("provide", "cloud run")
	add("auth login", "harness login")
	add("auth logout", "harness logout")
	add("auth", "harness auth")
	add("sandbox", "harness sandbox")
	add("sb", "harness sandbox")
	add("health", "doctor")
	add("check", "doctor")
	add("upgrade", "update")
	return rules
}()

// rewriteLegacy maps an old spelling in argv to the canonical one. now is the
// "amux ..." text to show a person (empty when argv was already canonical).
// A leading `help` word is kept: amux help direct enroll is amux help device
// connect.
func rewriteLegacy(args []string) (out []string, now string) {
	prefix := 0
	if len(args) > 0 && args[0] == "help" {
		prefix = 1
	}
	words := args[prefix:]
	for _, r := range legacyRules {
		if len(words) < len(r.from) || !slices.Equal(words[:len(r.from)], r.from) {
			continue
		}
		if r.exact && len(words) != len(r.from) {
			continue
		}
		if r.to == nil {
			return args, r.hint
		}
		out = append(append(append([]string{}, args[:prefix]...), r.to...), words[len(r.from):]...)
		if r.hint != "" {
			return out, r.hint
		}
		return out, "amux " + shellJoin(out)
	}
	return args, ""
}

// shellJoin quotes the words that need it, for a hint a person can re-type.
func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\"'$`\\|&;<>()*?[]#~") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		q[i] = a
	}
	return strings.Join(q, " ")
}

// stderrIsTerminal gates the legacy hint; tests replace it.
var stderrIsTerminal = func() bool { return isTerminal(os.Stderr) }

// legacyShortForms are old short spellings shown on their long form's row.
var legacyShortForms = map[string]string{"wg": "workgroup", "q": "queue", "sb": "sandbox"}

// legacyDisplay is a rule's old spelling as the table shows it ("amux
// workgroup|wg ls"); ok is false for a short-form rule its long form covers.
func legacyDisplay(r legacyRule) (string, bool) {
	if _, short := legacyShortForms[r.from[0]]; short {
		return "", false
	}
	words := slices.Clone(r.from)
	for s, long := range legacyShortForms {
		if words[0] == long {
			words[0] += "|" + s
		}
	}
	return "amux " + strings.Join(words, " "), true
}

// renderLegacyTable lists every old spelling and what it is now (amux help --all).
func renderLegacyTable(w io.Writer) {
	fmt.Fprintln(w, "Old spellings keep working; on a terminal they print the new one on stderr.")
	fmt.Fprintln(w)
	var rows [][2]string
	for _, r := range legacyRules {
		old, ok := legacyDisplay(r)
		if !ok {
			continue
		}
		now := r.hint
		if now == "" {
			now = "amux " + strings.Join(r.to, " ")
		}
		if len(r.from) == 2 && r.from[0] == "direct" && r.from[1] == "clients" {
			old, now = old+" <command>", now+" <command>"
		}
		rows = append(rows, [2]string{old, now})
	}
	rows = append(rows,
		[2]string{"amux do ... -f agent=<harness>", "amux do ... -f harness=<harness>"},
		[2]string{"--agent <harness> (create flags)", "--harness <harness>"},
	)
	renderRowsWidth(w, rows, 32)
}
