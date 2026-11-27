package main

import (
	"bytes"
	"reflect"
	"slices"
	"strings"
	"testing"

	"amux/internal/core"
	"amux/internal/store"
)

// legacyCases are the old spellings users and running agents type, and the
// canonical argv each must become. Every case is routed through rewriteLegacy,
// the one place the CLI maps old to new, so an old spelling reaches exactly the
// new handler with the same operands.
var legacyCases = []struct{ old, now string }{
	{"status", "ls"},
	{"status --json", "ls --json"},
	{"workgroup", "new"},
	{"wg", "new"},
	{"workgroup ls", "ls"},
	{"wg list", "ls"},
	{"workgroup open 3f7a", "open 3f7a"},
	{"workgroup switch 3f7a", "open 3f7a"},
	{"workgroup archive 9c1b --force", "archive 9c1b --force"},
	{"workgroup done 9c1b", "archive 9c1b"},
	{"workgroup unarchive 9c1b", "unarchive 9c1b"},
	{"workgroup restore 9c1b", "unarchive 9c1b"},
	{"workgroup rename 9c1b api spike", "rename 9c1b api spike"},
	{"workgroup rm 9c1b", "rm 9c1b"},
	{"workgroup delete 9c1b", "rm 9c1b"},
	{"workgroup move 3f7a 9c1b", "move 3f7a 9c1b"},
	{"workgroup recreate 3f7a", "recreate 3f7a"},
	{"workgroup repos 3f7a api web", "repo set 3f7a api web"},
	{"workgroup grants 9c1b --all", "repo grant 9c1b --all"},
	{"queue", "inbox"},
	{"question", "inbox questions"},
	{"question list --json", "inbox questions --json"},
	{"question show q-1", "inbox show q-1"},
	{"question answer q-1 --select 2 --note ok", "inbox answer q-1 --select 2 --note ok"},
	{"question dismiss q-1 --message no", "inbox cancel q-1 --message no"},
	{"question release q-1", "inbox release q-1"},
	{"queue list --json", "inbox ls --json"},
	{"q", "inbox"},
	{"queue answer q-12 --choice Yes", "inbox answer q-12 --choice Yes"},
	{"queue reply q-12 ok", "inbox answer q-12 ok"},
	{"queue dismiss q-12", "inbox dismiss q-12"},
	{"queue ack it-1", "inbox ack it-1"},
	{"direct enroll --qr", "device connect --qr"},
	{"direct clients list", "device ls"},
	{"direct clients list --json", "device ls --json"},
	{"direct clients revoke c-1", "device revoke c-1"},
	{"direct clients update c-1 --permissions read", "device update c-1 --permissions read"},
	{"direct status", "device status"},
	{"direct enable --tailnet", "device enable --tailnet"},
	{"direct disable", "device disable"},
	{"direct logs -n 5", "device logs -n 5"},
	{"direct serve", "device serve"},
	{"direct audit --json", "device audit --json"},
	{"direct reset-identity --yes", "device reset-identity --yes"},
	{"direct cloud enable --permissions read", "cloud enroll enable --permissions read"},
	{"direct cloud status", "cloud enroll status"},
	{"direct wake enable", "cloud wake enable"},
	{"provide", "cloud run"},
	{"provide orch:7443 --token-file t", "cloud run orch:7443 --token-file t"},
	{"provide install --dry-run", "cloud connect --dry-run"},
	{"provide uninstall", "cloud disconnect"},
	{"provide revoke --yes", "cloud revoke --yes"},
	{"auth login codex", "harness login codex"},
	{"auth logout claude", "harness logout claude"},
	{"auth status", "harness auth status"},
	{"auth accounts --json", "harness auth accounts --json"},
	{"auth restart --force", "harness auth restart --force"},
	{"sandbox drift", "harness sandbox drift"},
	{"sb", "harness sandbox"},
	{"health", "doctor"},
	{"upgrade --check", "update --check"},
	{"help direct enroll", "help device connect"},
}

func TestLegacySpellingsRewrite(t *testing.T) {
	for _, c := range legacyCases {
		got, now := rewriteLegacy(strings.Fields(c.old))
		if want := strings.Fields(c.now); !slices.Equal(got, want) {
			t.Errorf("amux %s → %v, want %v", c.old, got, want)
		}
		if now == "" {
			t.Errorf("amux %s gives no now: hint", c.old)
		}
	}
	// Canonical spellings, and the old create forms that keep their own
	// handler, are left alone.
	for _, keep := range []string{"ls", "new task", "send 3f7a hi", "device connect", "harness ls",
		"workgroup create api", "workgroup add 9c1b api", "workgroup new api", "do new-workgroup -f agent=codex"} {
		if got, _ := rewriteLegacy(strings.Fields(keep)); !slices.Equal(got, strings.Fields(keep)) {
			t.Errorf("amux %s was rewritten to %v", keep, got)
		}
	}
}

// TestLegacyRulesLandOnRealCommands: every rule's target is a dispatched node
// of the tree, so an old spelling can never route into nothing. (device update
// is the one target still to come, from the direct permissions work.)
func TestLegacyRulesLandOnRealCommands(t *testing.T) {
	for _, r := range legacyRules {
		if r.to == nil {
			if lookupCommand(r.from[0]) == nil {
				t.Errorf("hint-only %v has no handler", r.from)
			}
			continue
		}
		if lookupCommand(r.to[0]) == nil {
			t.Errorf("%v → %v: no such top-level command", r.from, r.to)
			continue
		}
		if n := leadingWords(r.to); n != len(r.to) {
			t.Errorf("%v → %v: %q is not a node of the tree", r.from, r.to, r.to[n])
		}
	}
}

// TestLegacyRoutesToTheNewHandler: an old spelling dispatches the same run
// function, with the same remaining operands, as its new spelling.
func TestLegacyRoutesToTheNewHandler(t *testing.T) {
	for _, c := range legacyCases {
		old, _ := rewriteLegacy(strings.Fields(c.old))
		now := strings.Fields(c.now)
		if old[0] == "help" {
			continue
		}
		a, b := lookupCommand(old[0]), lookupCommand(now[0])
		if a == nil || b == nil || reflect.ValueOf(a.run).Pointer() != reflect.ValueOf(b.run).Pointer() {
			t.Errorf("amux %s does not dispatch amux %s's handler", c.old, c.now)
		}
	}
}

// TestLegacyHintOnlyOnATerminal: the "now:" line goes to stderr on a terminal
// and nowhere else — an agent's tool call or a pipe sees unchanged output.
func TestLegacyHintOnlyOnATerminal(t *testing.T) {
	bak := stderrIsTerminal
	t.Cleanup(func() { stderrIsTerminal = bak })
	for _, tty := range []bool{true, false} {
		stderrIsTerminal = func() bool { return tty }
		var out, errOut bytes.Buffer
		code := runCLI([]string{"status", "--help"}, &out, &errOut, func() bool { return false })
		if code != 0 || !strings.HasPrefix(out.String(), "amux ls — ") {
			t.Fatalf("amux status --help = %d %q, want amux ls's page", code, firstOf(out.String()))
		}
		hint := errOut.String()
		switch {
		case tty && hint != "now: amux ls --help\n":
			t.Errorf("terminal hint = %q", hint)
		case !tty && hint != "":
			t.Errorf("non-terminal stderr = %q, want nothing", hint)
		}
		if strings.Contains(out.String(), "now:") {
			t.Error("the hint leaked onto stdout")
		}
	}
}

// TestLegacyWorkgroupVerbsRouteThroughTheRewrite: calling the legacy
// workgroup handler directly reaches the same new verbs.
func TestLegacyWorkgroupVerbsRouteThroughTheRewrite(t *testing.T) {
	sandboxCLI(t)
	if err := runCommand(t, lookupCommand("workgroup"), []string{"open"}); err == nil || !strings.Contains(err.Error(), "usage: amux open") {
		t.Errorf("workgroup open = %v, want amux open's usage", err)
	}
	if err := runCommand(t, lookupCommand("workgroup"), []string{"zap"}); err == nil || !isUnknownSubcommand(err) {
		t.Errorf("workgroup zap = %v, want unknown subcommand", err)
	}
}

func TestAllHelpListsOldSpellings(t *testing.T) {
	_, all, _ := runHelp(t, "help", "--all")
	if !strings.Contains(all, "==== old spellings ====") {
		t.Fatal("--all has no old spellings table")
	}
	for _, r := range legacyRules {
		if old, ok := legacyDisplay(r); ok && !strings.Contains(all, old) {
			t.Errorf("--all does not list old spelling %v", r.from)
		}
	}
	for _, short := range []string{"workgroup|wg", "queue|q", "sandbox|sb"} {
		if !strings.Contains(all, "amux "+short) {
			t.Errorf("--all does not show the short form in %s", short)
		}
	}
	for _, w := range []string{"-f agent=", "--agent <harness>"} {
		if !strings.Contains(all, w) {
			t.Errorf("--all does not mention %s", w)
		}
	}
	// The removed alias soup is gone for good.
	for _, gone := range []string{"session", "ses", "workspace", "ws"} {
		if lookupCommand(gone) != nil {
			t.Errorf("amux %s still dispatches", gone)
		}
	}
}

func TestDoHarnessKeyAliasesAgent(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"add-agent", "9c1b", "-f", "harness=codex"}, "codex"},
		{[]string{"add-agent", "9c1b", "-f", "agent=codex"}, "codex"},
		{[]string{"add-agent", "9c1b", "-f", "agent=claude", "-f", "harness=codex"}, "claude"},
	} {
		a, err := parseDoArgs(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		if a.Fields["agent"] != tc.want || a.Fields["harness"] != "" {
			t.Errorf("%v → fields %v, want agent=%s and no harness key", tc.args, a.Fields, tc.want)
		}
	}
}

func TestSendBuildsSteer(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		verb  string
		text  string
		stdin string
	}{
		{[]string{"3f7a", "rebase", "on", "master"}, core.SteerPrompt, "rebase on master", ""},
		{[]string{"9c1b", "status?"}, core.SteerPrompt, "status?", ""},
		{[]string{"3f7a", "--now", "use v2"}, core.SteerInterject, "use v2", ""},
		{[]string{"3f7a", "use v2", "--now"}, core.SteerInterject, "use v2", ""},
		{[]string{"3f7a", "--stop"}, core.SteerStop, "", ""},
		{[]string{"3f7a", "-"}, core.SteerPrompt, "from stdin", "from stdin\n"},
	} {
		a, err := steerForSend(tc.args, strings.NewReader(tc.stdin))
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if a.Action != core.ActionSteer || a.ID != tc.args[0] || a.Fields[core.SteerVerb] != tc.verb || a.Fields[core.SteerText] != tc.text {
			t.Errorf("%v → %+v", tc.args, a)
		}
	}
	for _, bad := range [][]string{{}, {"3f7a"}, {"3f7a", "--now"}, {"3f7a", "--stop", "x"}, {"3f7a", "--now", "--stop", "x"}, {"3f7a", "--zap", "x"}} {
		if _, err := steerForSend(bad, strings.NewReader("")); err == nil || !isUsageError(err) {
			t.Errorf("send %v = %v, want a usage error", bad, err)
		}
	}
}

func TestNewAndAddActions(t *testing.T) {
	f, err := parseVerbFlags([]string{"fix", "it", "--repo", "api,web", "--agent", "codex", "--coordinator-model", "opus"},
		[]string{"repo", "harness", "model", "coordinator", "coordinator-model", "name"}, []string{"interactive"}, harnessFlagAlias)
	if err != nil {
		t.Fatal(err)
	}
	a := newWorkgroupAction(strings.Join(f.operands, " "), f.list("repo"), f)
	want := map[string]string{"prompt": "fix it", "repos": "api,web", "agent": "codex", "mode": store.ModeTask, "coordinator_model": "opus"}
	if a.Action != core.ActionNewWorkgroup || !reflect.DeepEqual(a.Fields, want) {
		t.Errorf("new action = %+v, want fields %v", a, want)
	}
	// No repo: the coordinator alone, which picks a repo or asks.
	f, _ = parseVerbFlags([]string{"triage"}, []string{"repo"}, []string{"interactive"}, nil)
	if a := newWorkgroupAction("triage", nil, f); !reflect.DeepEqual(a.Fields, map[string]string{"prompt": "triage"}) {
		t.Errorf("repo-less new = %v, want only the prompt", a.Fields)
	}

	f, err = parseVerbFlags([]string{"9c1b", "write", "the", "migration", "--repo", "api", "--harness=codex", "--model", "m", "--interactive"},
		[]string{"repo", "harness", "model"}, []string{"interactive"}, harnessFlagAlias)
	if err != nil {
		t.Fatal(err)
	}
	a = addAgentAction(f.operands[0], strings.Join(f.operands[1:], " "), f)
	want = map[string]string{"repos": "api", "agent": "codex", "model": "m", "mode": store.ModeInteractive, "prompt": "write the migration"}
	if a.Action != core.ActionAddAgent || a.ID != "9c1b" || !reflect.DeepEqual(a.Fields, want) {
		t.Errorf("add action = %+v, want fields %v", a, want)
	}
}

func TestRenderLsGroupsAgentsUnderWorkgroups(t *testing.T) {
	sessions := []core.Session{
		{ID: "9c1b", Title: "auth", Kind: "codex", IsRoot: true, State: core.StateRunning},
		{ID: "3f7a", Title: "api", Kind: "claude", RootID: "9c1b", State: core.StateIdle},
		{ID: "0ld0", Title: "old", Kind: "claude", RootID: "9c1b", Archived: true},
	}
	var b bytes.Buffer
	renderLs(&b, sessions, false, map[string]string{"9c1b": " · repos: api"})
	out := b.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "9c1b ") || !strings.HasPrefix(lines[1], "  3f7a ") {
		t.Fatalf("ls =\n%s", out)
	}
	if !strings.Contains(lines[0], "running") || !strings.Contains(lines[0], "· repos: api") || !strings.Contains(lines[1], "stopped") {
		t.Errorf("ls lacks state or repos:\n%s", out)
	}
	b.Reset()
	renderLs(&b, sessions, true, nil)
	if !strings.Contains(b.String(), "0ld0") || !strings.Contains(b.String(), "[archived]") {
		t.Errorf("ls --all omits archived:\n%s", b.String())
	}
}
