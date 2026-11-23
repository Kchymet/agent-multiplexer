package main

import "amux/internal/vtdemo"

// buildCLITree is the CLI's single source of truth: every command, subcommand,
// flag, `amux do` action and field, with what it means and how to call it. The
// top-level page, each level's --help and the --all reference are all rendered
// from it (help.go), and the top-level dispatch is its run functions.
//
// The top level is a hybrid: plain verbs for the frequent actions (new, ls,
// open, send, add, archive), nouns for occasional management (inbox, repo,
// device, cloud, harness, ...). Each top-level command carries a Group that
// decides where the short top-level page lists it; commands in groupMore are
// left off that page but keep their own --help and appear in --all. Old
// spellings (amux status, amux queue, amux direct ...) are not tree nodes: they
// are rewritten to the canonical argv before dispatch (legacy.go).
//
// Keep summaries to one line; put everything else in Detail, Flags, Fields and
// Examples. A command parsed by a flag.FlagSet sets FlagSet and calls
// parseCmdFlags with its path, which refuses a flag missing from here.
func buildCLITree() *cmdSpec {
	return &cmdSpec{
		Name:    "amux",
		Summary: "AI-native terminal control plane",
		Flags: []flagSpec{
			{Name: "help", Short: "h", Usage: "show help for amux or for the command it follows"},
			{Name: "all", Short: "a", Usage: "with --help (and no command): print the entire help tree"},
			{Name: "all-help", Usage: "print the entire help tree: every command, flag, action and example"},
			{Name: "version", Short: "v", Usage: "print CLI, daemon and database versions (same as amux version)"},
		},
		Detail: "Set AMUX_SKIP=1 in your shell to bypass auto-launch.",
		Sub: []*cmdSpec{
			// Everyday work: plain verbs.
			newSpec(),
			{
				Name: "ls", Group: groupWork, Args: "[--all] [--json]", run: cmdLs,
				Summary: "workgroups and agents (state, PRs)",
				Detail: `Each workgroup (its id is its coordinator's) with its agents indented below
it: id, name, harness, state, and open pull requests or review state. Reads
the daemon's canonical state. From inside an agent session the view is scoped
to the session itself (a coordinator also sees its workgroup's current direct
members).`,
				Flags: []flagSpec{
					{Name: "all", Short: "a", Usage: "also list archived workgroups and agents"},
					{Name: "json", Short: "j", Usage: "print the raw snapshot (every session field) as JSON"},
				},
				Examples: []string{"amux ls", "amux ls --json"},
			},
			{
				Name: "open", Group: groupWork, Args: "<id>", run: cmdOpen,
				Summary: "open the dashboard on a workgroup or agent",
				Detail:  "Starts it if it is stopped, as picking it in the rail does. Needs a terminal.",
			},
			sendSpec(),
			addSpec(),
			{
				Name: "archive", Group: groupWork, Args: "<id> [--force]", run: cmdArchive,
				Summary: "finish a workgroup (refuses while PRs are open)",
				Detail: `Drops a workgroup (and its agents) or a single agent off the active rail.
Refuses while an open pull request remains; --force overrides. amux unarchive
puts it back.`,
				Flags: []flagSpec{{Name: "force", Short: "f", Usage: "archive even with an open pull request"}},
			},

			// Occasional management: nouns.
			inboxSpec(),
			repoSpec(),
			deviceSpec(),
			cloudSpec(),
			harnessSpec(),
			configSpec(),
			daemonSpec(),
			{
				Name: "doctor", Group: groupManage, run: func([]string) error { return cmdDoctor() },
				Summary: "health check: versions, compatibility, dependencies and runtime",
				Detail: `Reports the CLI, daemon and database versions and their compatibility, the
dependencies amux launches, saved settings versus environment overrides, the
Harness cloud connection (amux cloud) and the device listener (amux device).`,
			},
			{
				Name: "update", Group: groupManage, Args: "[--check] [--force] [--restart-daemon]", run: cmdUpdate,
				Summary: "install the latest amux release",
				Detail: `Bare: download the latest release for this OS/arch, verify its SHA256,
atomically replace this binary, refresh the shell shim, then restart the daemon
if no agents are running.

The daemon also installs releases on its own every few hours (update.auto, on
by default; AMUX_NO_AUTO_UPDATE=1 or amux config set update.auto off disables
it). A daemon restart stops the agents it hosts, so an update that arrives
while agents run waits: amux version, doctor and ls report it as pending,
and the daemon restarts into it once no agents are running.`,
				Flags: []flagSpec{
					{Name: "check", Short: "n", Usage: "only report the installed and latest versions"},
					{Name: "force", Short: "f", Usage: "also replace a development (checkout) build, or reinstall the latest release"},
					{Name: "restart-daemon", Usage: "restart the daemon even while agents run (they are stopped and resumed,\nas with amux daemon restart)"},
				},
				Examples: []string{"amux update --check", "amux update"},
			},
			{
				Name: "version", Group: groupManage, Aliases: []string{"--version", "-v"}, Args: "[--short | --manifest]", run: cmdVersionArgs,
				Summary: "print CLI, daemon and database versions and compatibility",
				Flags: []flagSpec{
					{Name: "short", Short: "s", Usage: "print only this binary's version"},
					{Name: "manifest", Usage: "print this build's compatibility contracts as release-manifest JSON"},
				},
			},
			{
				Name: "help", Group: groupManage, Args: "[<command>...] [--all]", run: cmdHelp,
				Summary: "help for amux or a command (help agent, help api, help --all)",
				Detail: `amux help <command> is the same as amux <command> --help. Two topics cover
the commands that are not on the main page:
  amux help agent   commands an agent runs about itself (amux agent ...)
  amux help api     the scripting API: one daemon action per call (amux do ...)
amux help --all prints every command, flag, action and example in one page,
then the table of old spellings and what they are now.`,
				Examples: []string{"amux help new", "amux help api", "amux help --all"},
			},

			// Less frequent: own --help and --all only.
			{
				Name: "rename", Group: groupMore, Args: "<id> <name>", run: cmdRename,
				Summary: "set a workgroup's or agent's display name (the id is unchanged)",
			},
			{
				Name: "unarchive", Group: groupMore, Args: "<id>", run: cmdUnarchive,
				Summary: "put an archived workgroup or agent back",
			},
			{
				Name: "rm", Group: groupMore, Args: "<id> [--force]", run: cmdRm,
				Summary: "delete a workgroup or agent, its worktrees and branches",
				Detail:  "Refuses while an open pull request remains; --force overrides.",
				Flags:   []flagSpec{{Name: "force", Short: "f", Usage: "delete even with an open pull request"}},
			},
			{
				Name: "move", Group: groupMore, Args: "<agent> [<workgroup> | --new]", run: cmdMove,
				Summary: "move an agent into another workgroup, or a new one of its own",
				Detail: `The agent's repos must be within the target workgroup's. With --new or no
target it moves into a new workgroup of its own limited to its repos.`,
				Flags: []flagSpec{{Name: "new", Usage: "move into a new workgroup of its own"}},
			},
			{
				Name: "recreate", Group: groupMore, Args: "<id>", run: cmdRecreate,
				Summary: "replace one runtime with current access mounts (host only)",
				Detail:  "Stored worktree, config and transcript files are preserved.",
			},
			{
				Name: "top", Group: groupMore, run: cmdTop, FlagSet: true,
				Summary: "per-session CPU/memory, top processes and amux's own overhead",
				Flags: []flagSpec{
					{Name: "json", Usage: "print the raw resource snapshot as JSON"},
					{Name: "mem", Usage: "sort by resident memory instead of CPU"},
					{Name: "m", Usage: "shorthand for --mem"},
					{Name: "n", Arg: "<count>", Default: "3", Usage: "top processes to show per session (0 hides them)"},
				},
				Examples: []string{"amux top", "amux top --mem -n 0"},
			},
			{
				Name: "refresh", Group: groupMore, run: func([]string) error { return cmdRefresh() },
				Summary: "ask the daemon to re-poll its sources now",
			},
			{
				Name: "licenses", Group: groupMore, Aliases: []string{"notices"}, run: cmdLicenses,
				Summary: "print bundled dependency licenses and notices",
			},
			agentSpec(),
			doSpec(),

			// Internal and deprecated surfaces: dispatched, never listed.
			workgroupSpec(),
			{Name: "serve", Hidden: true, run: cmdServe, Summary: "internal: serve panes over the mux protocol"},
			{Name: "_vtdemo", Hidden: true, run: vtdemo.Run, Summary: "internal: embedded-terminal fidelity check"},
			{Name: "hook", Hidden: true, Args: "<state>", run: func(args []string) error { return cmdAgentStatus(args, true) },
				Summary: "deprecated alias of amux agent hook"},
			{Name: "name", Hidden: true, Args: "<display name>", run: cmdName, Summary: "deprecated alias of amux agent name"},
		},
	}
}

// Top-level groups: where the short top-level page lists a command.
const (
	groupWork   = "work"   // everyday verbs, listed first
	groupManage = "manage" // occasional management nouns
	groupMore   = "more"   // named on one line only; full pages in --all
)

func newSpec() *cmdSpec {
	return &cmdSpec{
		Name: "new", Group: groupWork, Args: `["<task>"] [flags]`, run: cmdNew,
		Summary: "start a workgroup (no args: the create form)",
		Detail: `Creates a workgroup and hands <task> to its coordinator, which starts the
member agents the task needs. With no repo the coordinator picks one or asks;
--repo limits the workgroup to those repos and seeds a first member on them
(idle until the coordinator steers it). With no arguments at all, the
interactive create form opens (needs a terminal).

Coordinators are interactive and members run as tasks; --interactive is the
advanced override for that first member.`,
		Flags: []flagSpec{
			{Name: "repo", Arg: "<name>", Repeat: true, Usage: "limit the workgroup to this tracked repo and seed a first member on it\n(repeat, or give a comma list)"},
			{Name: "harness", Arg: "<harness>", Default: "claude", Usage: "the first member's harness: claude | codex"},
			{Name: "model", Arg: "<model>", Usage: "the first member's model (default: the harness's default)"},
			{Name: "coordinator", Arg: "<harness>", Default: "codex", Usage: "the coordinator's harness: claude | codex"},
			{Name: "coordinator-model", Arg: "<model>", Usage: "the coordinator's model"},
			{Name: "name", Arg: "<name>", Usage: "workgroup display name"},
			{Name: "interactive", Usage: "advanced: the first member is interactive instead of a task"},
		},
		Examples: []string{
			"amux new",
			`amux new "upgrade CI to Go 1.26"`,
			`amux new "fix the flaky auth test" --repo api --harness codex`,
		},
	}
}

func sendSpec() *cmdSpec {
	return &cmdSpec{
		Name: "send", Group: groupWork, Args: `<id> "<text>" [--now | --stop]`, run: cmdSend,
		Summary: "message an agent or workgroup (queued if busy; --now / --stop)",
		Detail: `<id> is an agent id, or a workgroup id to reach its coordinator. By default the
text is a new prompt: delivered when the agent is ready, queued while it is
busy, and a stopped agent is started first. --now interjects it into the turn
that is running; --stop interrupts the turn (no text needed). Text "-" reads it
from stdin. Delivery is typed into the agent's composer (docs/steering.md).

From inside an agent session the same scope applies as for amux do steer: a
member may message only itself, a coordinator also its members.`,
		Flags: []flagSpec{
			{Name: "now", Usage: "interject into the running turn instead of queueing a new prompt"},
			{Name: "stop", Usage: "interrupt the running turn"},
		},
		Examples: []string{
			`amux send 3f7a "rebase on master"`,
			`amux send 9c1b "status report, please"`,
			`amux send 3f7a --now "use the v2 endpoint"`,
			"amux send 3f7a --stop",
		},
	}
}

func addSpec() *cmdSpec {
	return &cmdSpec{
		Name: "add", Group: groupWork, Args: `<workgroup> ["<task>"] [flags]`, run: cmdAdd,
		Summary: "add an agent to a workgroup",
		Detail: `Adds a member agent within the workgroup's repos and gives it <task> as its
first prompt. With only <workgroup>, pick the agent's settings interactively
(needs a terminal). Members run as tasks; --interactive makes this one
interactive (driven turn by turn, closed by you).`,
		Flags: []flagSpec{
			{Name: "repo", Arg: "<name>", Repeat: true, Usage: "the agent's repos, within the workgroup's (repeat, or give a comma list)"},
			{Name: "harness", Arg: "<harness>", Default: "claude", Usage: "the agent's harness: claude | codex"},
			{Name: "model", Arg: "<model>", Usage: "the agent's model (default: the harness's default)"},
			{Name: "interactive", Usage: "advanced: an interactive agent instead of a task"},
		},
		Examples: []string{
			`amux add 9c1b "write the migration" --repo api`,
			"amux add 9c1b",
		},
	}
}

func repoSpec() *cmdSpec {
	return &cmdSpec{
		Name: "repo", Group: groupManage, run: cmdRepo,
		Summary: "track the repositories agents get worktrees of",
		Sub: []*cmdSpec{
			{Name: "add", Args: "[<src>]", Summary: "track a repo: a gh owner/name slug, a git URL, or a local path",
				Detail:   "With no <src>, browse your GitHub owners and pick (needs a terminal).",
				Examples: []string{"amux repo add Kchymet/agent-multiplexer", "amux repo add ~/src/api"}},
			{Name: "ls", Aliases: []string{"list"}, Summary: "list tracked repositories"},
			{Name: "rm", Aliases: []string{"remove"}, Args: "<name>", Summary: "stop tracking a repository"},
			{Name: "set", Args: "<agent> <repo>...", Summary: "re-scope an agent to exactly these tracked repos"},
			{Name: "grant", Args: "<coordinator> [--all | --none | repo...]", Summary: "set a workgroup's repo ceiling (host only)",
				Flags: []flagSpec{
					{Name: "all", Usage: "every tracked repo, live (repos tracked later are included)"},
					{Name: "none", Usage: "no repos (also the meaning of no arguments)"},
				}},
		},
	}
}

// createFlags are the legacy amux workgroup create/repo flags.
var createFlags = []flagSpec{
	{Name: "name", Arg: "<name>", Usage: "workgroup display name"},
	{Name: "prompt", Arg: "<text>", Usage: "the coordinator's task; the first agent starts idle for it"},
	{Name: "harness", Arg: "<harness>", Default: "claude", Usage: "first agent's harness (claude | codex; --agent is the old spelling)"},
	{Name: "mode", Arg: "<mode>", Default: "task", Usage: "first agent's mode (task | interactive)"},
	{Name: "model", Arg: "<model>", Usage: "first agent's model (default: the harness's default)"},
	{Name: "coordinator", Arg: "<harness>", Default: "codex", Usage: "coordinator harness (claude | codex)"},
	{Name: "coordinator-model", Arg: "<model>", Usage: "coordinator model"},
}

// workgroupSpec is the legacy `amux workgroup` namespace, hidden. Its renamed
// verbs (ls, open, archive, rename, ...) are rewritten to the new commands
// before dispatch (legacy.go); what is left here are the create forms whose
// operands differ from amux new / amux add, kept so old scripts still run.
func workgroupSpec() *cmdSpec {
	return &cmdSpec{
		Name: "workgroup", Aliases: []string{"wg"}, Hidden: true, Args: "[<command>]", run: cmdSession,
		Summary: "legacy: amux new, amux add, amux ls and friends replace it",
		Detail:  "Bare amux workgroup is the interactive create page (now: amux new).",
		Sub: []*cmdSpec{
			{Name: "new", Args: "[repo...]", Summary: "interactive create page (now: amux new --repo <r>)"},
			{Name: "create", Args: "<repo>... [flags]", Summary: "a workgroup limited to those repos, with one agent on them (now: amux new)",
				Flags: createFlags},
			{Name: "repo", Args: "<repo> [flags]", Summary: "a workgroup limited to one repo, with one agent on it (now: amux new --repo)",
				Flags: createFlags},
			{Name: "add", Args: "<root> [repo...] [--harness h] [--mode m] [--model M] [--prompt t]",
				Summary: "add an agent to a workgroup (now: amux add)",
				Flags: []flagSpec{
					{Name: "harness", Arg: "<harness>", Default: "claude", Usage: "the agent's harness (claude | codex; --agent is the old spelling)"},
					{Name: "mode", Arg: "<mode>", Default: "task", Usage: "task | interactive"},
					{Name: "model", Arg: "<model>", Usage: "the agent's model"},
					{Name: "prompt", Arg: "<text>", Usage: "the agent's first prompt"},
				}},
		},
	}
}
