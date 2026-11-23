package main

import "amux/internal/core"

func agentSpec() *cmdSpec {
	return &cmdSpec{
		Name: "agent", Group: groupMore, run: cmdAgent,
		Summary: "commands an agent runs about itself (amux help agent)",
		Detail: `Scoped to the calling agent by the fixed authenticated session context. Stdin,
environment variables, UUIDs and paths never select identity or storage.
Generated hook forms are best-effort so telemetry never disrupts the agent;
explicit report and control commands return nonzero when the daemon did not
confirm the requested operation.

Commands use the private file mailbox from the ordinary sandboxed shell. Keep
both sandboxes enabled. Discovery grants no cross-session write access. amux
ls, amux send and amux do from a session are scoped the same way: to the session
itself, or for a coordinator to its workgroup and current direct members.`,
		Sub: []*cmdSpec{
			{Name: "status", Args: "<state>", Summary: "strictly report activity: idle | ready | waiting | running"},
			{Name: "hook", Args: "<state>", Summary: "nondisruptive Claude-hook activity binding"},
			{Name: "permission", Args: "<request|allow|deny|clear> [--hook]", Summary: "report a diagnostic permission observation",
				Detail: "These reports never create answerable approval rights.",
				Flags: []flagSpec{
					{Name: "hook", Usage: "generated-hook form (reads the hook payload; never fails the agent)"},
					{Name: "request-id", Arg: "<id>", Usage: "the permission request id"},
					{Name: "tool", Arg: "<name>", Usage: "the tool asking"},
					{Name: "action", Arg: "<text>", Usage: "what the tool wants to do"},
				}},
			{Name: "capture", Args: "<event> [--hook]", Summary: "request a daemon-owned transcript snapshot",
				Detail: "Generated Claude hooks use capture --hook and supply only the event.",
				Flags:  []flagSpec{{Name: "hook", Usage: "generated-hook form"}}},
			{Name: "model", Args: "<model> | --statusline", Summary: "report the runtime's current model",
				Detail: `--statusline reads Claude's status-line JSON on stdin. Claude's installed
status-line wrapper invokes it automatically and preserves any pre-existing
status-line command.`,
				Flags: []flagSpec{
					{Name: "statusline", Usage: "read the model from Claude's status-line JSON (nondisruptive)"},
					{Name: "forward-base64", Arg: "<cmd>", Usage: "internal: the wrapped status-line command to forward to"},
				}},
			{Name: "name", Aliases: []string{"label"}, Args: "<text>", Summary: "set this agent's display name",
				Examples: []string{`amux agent name "auth refactor"`}},
			{Name: "done", Summary: "archive this member; a coordinator archives its workgroup (after user confirmation)",
				Detail: `Identity comes only from the fixed session context. Restoration requires the
host (amux unarchive <id>). Run it only once your PR is merged or
closed.`},
			goalSpec(),
			{Name: "sessions", Args: "[--json]", Summary: "discover all host agent conversations, Claude and Codex (read-only)",
				Flags: []flagSpec{{Name: "json", Usage: "print as JSON"}}},
			{Name: "events", Args: "[<session-id>] [--after <seq> | --cursor <token>] [--json]",
				Summary: "read one bounded normalized event page (yourself when no id)",
				Detail: `Through authenticated session RPC. Responses are at most 64 KiB; continue with
the returned cursor.`,
				Flags: []flagSpec{
					{Name: "after", Arg: "<sequence>", Usage: "start after this event sequence number"},
					{Name: "cursor", Arg: "<token>", Usage: "continue from a previous page's next cursor"},
					{Name: "json", Usage: "print as JSON"},
				},
				Examples: []string{"amux agent events --json", "amux agent events 3f7a --cursor <next-cursor> --json"}},
			{Name: "wait", Args: "<session-id>... [--until <states>] [--timeout <dur>] [--json]",
				Summary: "block until a session changes state or PRs, then print it (no polling)",
				Detail:  "Yours, or for a coordinator a member's.",
				Flags: []flagSpec{
					{Name: "until", Arg: "<state>[,<state>...]", Usage: "wait for one of these states (ready, idle, waiting, ...)"},
					{Name: "timeout", Arg: "<duration>", Usage: "bound the wait; exits nonzero when it expires"},
					{Name: "json", Usage: "print as JSON"},
				},
				Examples: []string{"amux agent wait 3f7a --until ready,idle --timeout 30m"}},
			{Name: "raise", Args: "--kind <kind> --title <title> [flags]",
				Summary: "raise a structured item upward (member to coordinator, else the global inbox)",
				Detail: `A member's item goes to its coordinator; a coordinator's (or any session
outside a workgroup) to the global inbox. Raising the same kind and title again
while the item is still open updates it instead of adding a duplicate.`,
				Flags: []flagSpec{
					{Name: "kind", Arg: "<kind>", Required: true, Usage: "follow_up | outstanding | decision_needed | risk | fyi"},
					{Name: "title", Arg: "<text>", Required: true, Usage: "one line"},
					{Name: "detail", Arg: "<text>", Usage: "context; - reads it from stdin"},
					{Name: "pr", Arg: "<repo#n>", Repeat: true, Usage: "related pull request"},
					{Name: "link", Arg: "<url>", Repeat: true, Usage: "related link"},
					{Name: "suggest", Arg: "<text>", Usage: "the next step you recommend"},
					{Name: "json", Usage: "print the item as JSON"},
				},
				Examples: []string{`amux agent raise --kind follow_up --title "flaky e2e test" --pr agent-multiplexer#12 --suggest "quarantine it"`}},
			{Name: "items", Args: "[--raised | --inbox] [--state <s>,... | --all] [--limit n] [--json]",
				Summary: "list the items you raised or receive; show, ack, dismiss, done, reopen",
				Detail: `Lists the open and acknowledged items you raised or receive (a coordinator
also sees its workgroup's).`,
				Flags: []flagSpec{
					{Name: "raised", Usage: "only items you raised"},
					{Name: "inbox", Usage: "only items you receive"},
					{Name: "state", Arg: "<s>[,<s>...]", Usage: "only these states"},
					{Name: "all", Usage: "include dismissed and done items"},
					{Name: "limit", Arg: "<n>", Usage: "at most n items"},
					{Name: "json", Usage: "print as JSON"},
				},
				Sub: []*cmdSpec{
					{Name: "show", Args: "<id> [--json]", Summary: "print one item"},
					{Name: "ack", Args: "<id>... [--note <text>]", Summary: "acknowledge items"},
					{Name: "dismiss", Args: "<id>... [--note <text>]", Summary: "dismiss items"},
					{Name: "done", Args: "<id>... [--note <text>]", Summary: "mark items done"},
					{Name: "reopen", Args: "<id>... [--note <text>]", Summary: "reopen items"},
				}},
			{Name: "escalate", Args: "<id>... [--note <why>]", Summary: "coordinator: hand a received item to the global inbox",
				Detail: `For decisions that need the user, or work that crosses workgroups. The global
inbox is the cross-workgroup inbox the user works from.`,
				Flags: []flagSpec{{Name: "note", Arg: "<why>", Usage: "why it is escalated"}}},
		},
	}
}

func goalSpec() *cmdSpec {
	return &cmdSpec{
		Name: "goal", Args: "[<command>]", Summary: "Claude coordinator goal mode: show the goal or mark a transition",
		Detail: `Every user task becomes the coordinator's active goal, and amux continues the
turn until the goal is complete, the coordinator asks the user for input, it
reports itself blocked, or the user pauses it. The user pauses, resumes or
clears the goal from the host:
  amux do steer <coordinator> -f verb=goal -f status=paused|active|complete|clear`,
		Sub: []*cmdSpec{
			{Name: "show", Aliases: []string{"status"}, Args: "[--json]", Summary: "print the goal (the default)"},
			{Name: "complete", Aliases: []string{"done"}, Args: `"<summary>"`, Summary: "the objective is done and verified",
				Detail: "Archive finished members first (PR merged or closed); it lists idle unarchived members."},
			{Name: "ask", Aliases: []string{"input"}, Args: `"<question>"`, Summary: "stop and wait for the user's answer"},
			{Name: "wait", Aliases: []string{"waiting"}, Args: `"<waiting on>"`, Summary: "end this turn only; an armed background wake-up resumes the goal",
				Detail: `Never wait without an armed wake-up (Monitor / run_in_background). Name the
member/PR awaited and the wake-up; it must cover every open member PR.`},
			{Name: "blocked", Args: `"<reason>"`, Summary: "a repeating blocker only the user can clear"},
			{Name: "prompt", Hidden: true, Args: "--hook", Summary: "internal: the generated Claude prompt hook"},
			{Name: "stop", Hidden: true, Args: "--hook", Summary: "internal: the generated Claude stop hook"},
		},
	}
}

func inboxSpec() *cmdSpec {
	return &cmdSpec{
		Name: "inbox", Group: groupManage, Args: "[<command>]", run: cmdQueue,
		Summary: "questions and follow-ups waiting for you",
		Detail: `Every pending question for the user, from any session, in one place, with
answers routed back to the asking session. Reading works from a host terminal,
the admin console and the overseer; answering, dismissing and acknowledging
are the user's alone (host terminal).`,
		Sub: []*cmdSpec{
			{Name: "ls", Aliases: []string{"list"}, Args: "[--all] [--json]", Summary: "pending questions, global inbox counts and the workgroup summary (the default)",
				Detail: `Lists permission requests, AskUserQuestion prompts, coordinators' goal asks
and decision items, plus the workgroup summary (needs you / running / review /
done).`,
				Flags: []flagSpec{
					{Name: "all", Short: "a", Usage: "also list entries you dismissed"},
					{Name: "json", Short: "j", Usage: "print as JSON"},
				}},
			{Name: "answer", Aliases: []string{"reply"}, Args: "<entry> [<text>...] [--choice <label>]... [--allow | --deny]",
				Summary: "answer one entry; amux re-checks it is still open and delivers it",
				Detail: `A question takes --choice <label> (repeat per question, "a|b" for a
multi-select) or free text; a permission request takes --allow or --deny; a
goal ask or decision item takes text.`,
				Flags: []flagSpec{
					{Name: "choice", Short: "c", Arg: "<label>", Repeat: true, Usage: "the option label to pick"},
					{Name: "text", Short: "m", Arg: "<text>", Usage: "free-text answer (or give the words as operands)"},
					{Name: "allow", Usage: "allow a permission request"},
					{Name: "deny", Usage: "deny a permission request"},
				},
				Examples: []string{"amux inbox answer q-12 --choice Yes", "amux inbox answer q-13 --allow"}},
			{Name: "dismiss", Args: "<entry>", Summary: "hide an entry (a live question is hidden until it changes)"},
			{Name: "ack", Args: "<item>", Summary: "acknowledge a global-inbox item as the user (lets the overseer act on it)"},
		},
	}
}

func doSpec() *cmdSpec {
	n := &cmdSpec{
		Name: "do", Group: groupMore, Args: "<action> [id] [flags]", run: cmdDo,
		Summary: "the scripting API: one daemon control action per call (amux help api)",
		Detail: `Sends a single action to the daemon's control API (the same dispatch the
dashboard drives) and prints "ok". The positional [id] and [kind] forms still
work; flags reach the rest. Reads go through amux repo ls / amux ls. The
everyday commands (amux new, add, send, archive, rename, move) wrap these
actions.

From an agent session the request is authorized by that session's fixed
authenticated grant, never by an id, flag or environment variable: a member may
act only on itself (start, steer prompt|interject|stop, rename, archive itself),
a coordinator also on its workgroup's current direct members (plus add-agent,
steer permission, archive a member). Host-only actions are refused. See
docs/agent-cli-sandbox.md for the full scope table.`,
		Flags: []flagSpec{
			{Name: "id", Arg: "<id>", Usage: "target session id (alternative to the second positional)"},
			{Name: "target", Short: "t", Arg: "<root>", Usage: `destination workgroup id (for move; empty = a new workgroup)`},
			{Name: "field", Short: "f", Arg: "key=value", Repeat: true, Usage: "form field (see each action's fields)"},
			{Name: "kind", Arg: "<kind>", Usage: "legacy: the third positional; no current action reads it"},
			{Name: "cwd", Arg: "<dir>", Usage: "legacy: no current action reads it"},
		},
		Examples: []string{
			`amux do rename <id> -f name="api spike"`,
			"amux do move <id> --target <root>",
			`amux do add-agent <root> -f repos=api,web -f prompt="port the auth flow"`,
			`amux do new-workgroup -f name=infra -f repos=infra -f prompt="upgrade CI"`,
		},
	}
	specs := doActionSpecs()
	for _, a := range core.ControlActions() {
		if spec, ok := specs[a]; ok {
			spec.Name = a
			n.Sub = append(n.Sub, spec)
		}
	}
	return n
}

// doActionSpecs documents every `amux do` action, keyed by verb. The set of
// verbs comes from core.ControlActions(); the contract test fails when one is
// missing here or documented without being accepted.
func doActionSpecs() map[string]*cmdSpec {
	force := fieldSpec{Name: "force", Arg: "true", Usage: "override the open pull request guard"}
	workgroupFields := []fieldSpec{
		{Name: "name", Arg: "<text>", Usage: "workgroup display name"},
		{Name: "prompt", Arg: "<text>", Usage: "the coordinator's task (its goal on the default runtime); never copied to a member"},
		{Name: "repos", Arg: "<a,b>", Usage: "repos the workgroup is limited to (its grant ceiling); naming any also creates\na first member agent on them (one repo = a single-repo workgroup)"},
		{Name: "harness", Arg: "<harness>", Usage: "first member's harness: claude | codex (default claude; agent= is the old key)"},
		{Name: "model", Arg: "<model>", Usage: "first member's model (default: its harness's default)"},
		{Name: "mode", Arg: "<mode>", Usage: "first member's mode: task | interactive (default task)"},
		{Name: "coordinator", Arg: "<harness>", Usage: "coordinator runtime: claude | codex (default codex, the native goal runtime;\nclaude runs in goal mode through amux's goal hooks)"},
		{Name: "coordinator_model", Arg: "<model>", Usage: "coordinator model (default: its harness's default)"},
	}
	itemFields := []fieldSpec{
		{Name: "item", Arg: "<it-...>", Required: true, Usage: "the item id"},
	}
	return map[string]*cmdSpec{
		core.ActionRefresh: {Summary: "re-poll the daemon's sources now"},
		core.ActionStart: {Args: "<id>", Summary: "start an agent's (or a whole workgroup's) process",
			Detail: "id is an agent id, or a workgroup id to start its coordinator and every non-archived member (archived members are skipped)."},
		core.ActionRestart: {Args: "<agent-id>", Summary: "restart one agent and resume its conversation (host only)",
			Detail: "Interrupts its current turn."},
		core.ActionAuthReload: {Summary: "queue Claude agents to restart with current credentials",
			Fields: []fieldSpec{{Name: "force", Arg: "true", Usage: "also interrupt busy or unknown-state agents"}}},
		core.ActionSteer: {Args: "<agent-id>", Summary: "drive an agent: prompt, interject, stop, permission, goal",
			Detail: "A prompt starts a stopped agent and answers ok only once the runtime took the prompt;\nany other verb needs a running agent.",
			Fields: []fieldSpec{
				{Name: core.SteerVerb, Arg: "<verb>", Required: true, Usage: "prompt (a new user turn) | interject (text while a turn runs) |\nstop (interrupt the turn) | permission (answer a prompt) | goal (host only)"},
				{Name: core.SteerText, Arg: "<text>", Usage: "prompt/interject: the text to deliver"},
				{Name: core.SteerDecision, Arg: "allow|deny", Usage: "permission: the answer"},
				{Name: core.SteerRequestID, Arg: "<id>", Usage: "permission: the request answered; a stale one is refused"},
				{Name: core.SteerReason, Arg: "<text>", Usage: "permission: optional rationale"},
				{Name: core.SteerGoalStatus, Arg: "<status>", Usage: "goal: active | paused | complete | clear"},
				{Name: core.SteerGoalObjective, Arg: "<text>", Usage: "goal: replacement objective"},
				{Name: core.SteerGoalBudget, Arg: "<n>", Usage: "goal: token budget (positive integer)"},
			},
			Examples: []string{
				`amux do steer 3f7a -f verb=prompt -f text="rebase on master"`,
				"amux do steer 3f7a -f verb=stop",
				"amux do steer 9c1b -f verb=goal -f status=paused",
			}},
		core.ActionRename: {Args: "<id>", Summary: "set a display name",
			Fields:   []fieldSpec{{Name: "name", Arg: "<text>", Required: true, Usage: "the new display name"}},
			Examples: []string{`amux do rename 3f7a -f name="api spike"`}},
		core.ActionMove: {Args: "<agent-id> [--target <root>]", Summary: "re-parent an agent into another workgroup (no target = a new one)",
			Examples: []string{"amux do move 3f7a --target 9c1b"}},
		core.ActionArchive: {Args: "<id>", Summary: "toggle archived / restored"},
		core.ActionSetArchived: {Args: "<id>", Summary: "archive or restore explicitly",
			Fields: []fieldSpec{
				{Name: "archived", Arg: "true|false", Required: true, Usage: "true archives, false restores"},
				force,
			}},
		core.ActionDelete: {Args: "<id>", Summary: "delete for good: worktrees and branch",
			Fields: []fieldSpec{force}},
		core.ActionKill: {Args: "<id>", Summary: "alias of delete", Fields: []fieldSpec{force}},
		core.ActionAddRepo: {Summary: "track a repo",
			Fields:   []fieldSpec{{Name: "source", Arg: "<src>", Required: true, Usage: "OWNER/REPO slug, git URL, or local path"}},
			Examples: []string{"amux do add-repo -f source=Kchymet/agent-multiplexer"}},
		core.ActionRmRepo: {Args: "<repo-name>", Summary: "untrack a repo (id is the repo name)"},
		core.ActionAgentSetRepos: {Args: "<agent-id>", Summary: "re-scope an agent to exactly these repos",
			Fields: []fieldSpec{{Name: "repos", Arg: "<a,b>", Required: true, Usage: "the agent's repos"}}},
		core.ActionCoordinatorSetRepos: {Args: "<coordinator-id>", Summary: "replace a coordinator's repo grants (host only)",
			Fields: []fieldSpec{
				{Name: "repos", Arg: "<a,b>", Usage: "the grant ceiling"},
				{Name: "all", Arg: "1", Usage: "every tracked repo, live (takes no repos)"},
			}},
		core.ActionAddAgent: {Args: "<workgroup-id>", Summary: "add an agent to a workgroup",
			Fields: []fieldSpec{
				{Name: "repos", Arg: "<a,b>", Usage: "the agent's repos (within the workgroup's); none is allowed"},
				{Name: "harness", Arg: "<harness>", Usage: "claude | codex (default claude; agent= is the old key)"},
				{Name: "model", Arg: "<model>", Usage: "the agent's model"},
				{Name: "mode", Arg: "<mode>", Usage: "task | interactive (default task)"},
				{Name: "prompt", Arg: "<text>", Usage: "the agent's first prompt"},
			},
			Examples: []string{`amux do add-agent 9c1b -f repos=api,web -f prompt="port the auth flow"`}},
		core.ActionNewWorkgroup: {Summary: "create a workgroup (and a first agent when repos are given)",
			Fields: append(append([]fieldSpec{}, workgroupFields...),
				fieldSpec{Name: "linear", Arg: "<url>", Usage: "Linear issue link, prepended to the prompt"}),
			Examples: []string{
				`amux do new-workgroup -f name=infra -f repos=infra -f prompt="upgrade CI"`,
				`amux do new-workgroup -f name=triage -f coordinator=claude -f coordinator_model=opus`,
			}},
		core.ActionCreateWorkspace: {Summary: "create a workgroup whose coordinator gets the prompt; repos only limit it (the TUI form)",
			Fields: append(append([]fieldSpec{}, workgroupFields...),
				fieldSpec{Name: "defaultAgent", Arg: "1", Usage: "create the first agent (otherwise repos only set the ceiling)"})},
		core.ActionRaiseItem: {Args: "<session-id>", Summary: "raise an item as a session (agents: amux agent raise)",
			Fields: []fieldSpec{
				{Name: core.ItemKindField, Arg: "<kind>", Required: true, Usage: "follow_up | outstanding | decision_needed | risk | fyi"},
				{Name: core.ItemTitleField, Arg: "<text>", Required: true, Usage: "one line"},
				{Name: core.ItemDetailField, Arg: "<text>", Usage: "context"},
				{Name: core.ItemPRsField, Arg: "<r#1,r#2>", Usage: "related pull requests"},
				{Name: core.ItemLinksField, Arg: "<urls>", Usage: "related links, newline-separated"},
				{Name: core.ItemSuggestField, Arg: "<text>", Usage: "the recommended next step"},
			}},
		core.ActionItemState: {Args: "<session-id>", Summary: "move an item as a session",
			Fields: append(append([]fieldSpec{}, itemFields...),
				fieldSpec{Name: core.ItemStateField, Arg: "<state>", Required: true, Usage: "ack | dismiss | done | reopen"},
				fieldSpec{Name: core.ItemNoteField, Arg: "<text>", Usage: "optional note"})},
		core.ActionEscalateItem: {Args: "<coordinator-id>", Summary: "escalate an item to the global inbox as a coordinator",
			Fields: append(append([]fieldSpec{}, itemFields...),
				fieldSpec{Name: core.ItemNoteField, Arg: "<text>", Usage: "why"})},
		core.ActionQueueAnswer: {Summary: "answer a question-queue entry (host only; amux inbox answer)",
			Fields: []fieldSpec{
				{Name: core.QueueEntryField, Arg: "<id>", Required: true, Usage: "the queue entry"},
				{Name: core.QueueTextField, Arg: "<text>", Usage: "free-text answer"},
				{Name: core.QueueChoiceField, Arg: "<label>", Usage: "option label(s), one line per question"},
				{Name: core.SteerDecision, Arg: "allow|deny", Usage: "a permission request's answer"},
			}},
		core.ActionQueueDismiss: {Summary: "hide a question-queue entry (host only; amux inbox dismiss)",
			Fields: []fieldSpec{{Name: core.QueueEntryField, Arg: "<id>", Required: true, Usage: "the queue entry"}}},
	}
}

func configSpec() *cmdSpec {
	return &cmdSpec{
		Name: "config", Group: groupManage, Aliases: []string{"conf", "cfg"}, Args: "[<command>]", run: cmdConfig,
		Summary: "show or change amux settings (keybindings, Codex control, updates, ...)",
		Detail: `Settings:
  keys.<action> <chord>                 dashboard keybinding (chords like ctrl+g, alt+p, f5);
                                        applies the next time the dashboard opens
  codex.control app-server|pty          the daemon's Codex rollout selection (default pty;
                                        daemon restart required; AMUX_CODEX_CONTROL overrides)
  goal.auto_resume on|off               a restarted goal coordinator resumes an active or
                                        waiting goal (default on; read at daemon start)
  update.auto on|off                    automatic amux self-update (default on;
                                        AMUX_NO_AUTO_UPDATE=1 also disables)
  harnesses.update off|notify|auto      daemon update check for managed harnesses
  harnesses.check_interval <dur>        how often that check runs (24h; minimum 1h)
  harnesses.<claude|codex>.channel <c>  release channel to follow (claude: stable|latest)
  harnesses.<claude|codex>.bin <path>   explicit harness binary (beats the managed install)
  claude.auth, codex.auth               retired: every workload follows central auth
                                        (get and unset only)
  direct.enabled                        whether the daemon runs the direct listener (get
                                        only; change it with amux device enable|disable)

Run amux doctor to compare saved settings, this shell's overrides, and the
running daemon's selection.`,
		Sub: []*cmdSpec{
			{Name: "ls", Aliases: []string{"list"}, Summary: "saved settings and defaults (ignores environment overrides; the default)"},
			{Name: "get", Args: "<setting>", Summary: "print one setting's effective value"},
			{Name: "set", Args: "<setting> <value>", Summary: "save a setting",
				Examples: []string{"amux config set keys.focus-rail ctrl+g", "amux config set codex.control app-server"}},
			{Name: "unset", Args: "<setting>", Summary: "restore a setting's default"},
			{Name: "path", Summary: "print the config file path"},
		},
	}
}

func harnessSpec() *cmdSpec {
	return &cmdSpec{
		Name: "harness", Group: groupManage, Aliases: []string{"harnesses"}, Args: "[<command>]", run: cmdHarnessCLI,
		Summary: "Claude Code / Codex installs and logins",
		Detail: `<kind> is claude or codex. amux downloads the official native builds (Claude
Code's release bucket, Codex's GitHub release package), verifies their SHA-256,
and keeps each version in its own directory under
~/.local/share/amux/harnesses/<kind>/<version>, with a "current" link that
update/pin/rollback swap atomically. New sessions get the current version;
running sessions are restarted onto it when idle between turns, resuming their
conversations (harnesses.restart_sessions off keeps them on their version).

Binary resolution: AMUX_<KIND>_BIN, then config harnesses.<kind>.bin, then the
amux-managed install, then your own install on PATH. Settings (amux config):
  harnesses.update off|notify|auto   daemon-scheduled adoption + updates (default auto;
                                     AMUX_NO_HARNESS_AUTO_UPDATE=1 turns it off)
  harnesses.restart_sessions on|off  restart idle running sessions onto a new version (default on)
  harnesses.check_interval <dur>     how often it runs (default 24h, min 1h)
  harnesses.<kind>.channel <ch>      release channel to follow (claude: stable|latest)
  harnesses.<kind>.bin <path>        explicit binary override
amux disables the harnesses' own self-updaters in sessions it launches.`,
		Sub: []*cmdSpec{
			{Name: "ls", Aliases: []string{"list"}, FlagSet: true, Summary: "which binary each harness launches, installed versions, updates (the default)",
				Flags: []flagSpec{{Name: "json", Usage: "print JSON"}}},
			{Name: "install", Args: "<kind> [<version>|stable|latest] [--no-activate]", FlagSet: true, Summary: "download and activate a version",
				Flags:    []flagSpec{{Name: "no-activate", Usage: "install without making it current"}},
				Examples: []string{"amux harness install claude", "amux harness install codex 0.157.1"}},
			{Name: "update", Aliases: []string{"upgrade"}, Args: "[<kind>...]", Summary: "install and activate the newest release (skips pinned harnesses)"},
			{Name: "pin", Args: "<kind> [<version>]", Summary: "activate and freeze a version (default: the current one)"},
			{Name: "unpin", Args: "<kind>", Summary: "allow updates again"},
			{Name: "rollback", Args: "<kind>", Summary: "re-activate the previously active version"},
			{Name: "check", Args: "[<kind>...]", Summary: "look for a newer release without installing it"},
			{Name: "gc", Args: "[<kind>...]", Summary: "delete versions not current, previous, pinned or running"},
			{Name: "uninstall", Args: "<kind>", Summary: "remove amux's install; launches fall back to your PATH",
				Detail: "Also stops automatic adoption of it until the next install."},
			{Name: "adopt", Args: "[--dry-run]", FlagSet: true, Summary: "install a managed copy of each harness you already use (what the daemon does)",
				Detail: "A harness counts as used when its binary is on PATH or it is logged in.",
				Flags:  []flagSpec{{Name: "dry-run", Usage: "report what would be adopted without installing"}}},
			{Name: "login", Args: "[claude|codex] [--api-key] [--token-stdin]", Summary: "create or replace a harness's central login",
				Detail: `Claude: runs claude setup-token (browser sign-in), then asks you to paste the
token it prints, with input hidden. Codex: signs the central Codex home in to
ChatGPT (prints a browser URL; the daemon owns that login). --api-key replaces
either with an API key. Run it in a host terminal or the amux console terminal;
see amux harness auth --help for how logins reach sessions.`,
				Flags: []flagSpec{
					{Name: "api-key", Usage: "configure an API key instead of a subscription login"},
					{Name: "token-stdin", Usage: "read the token or key from stdin instead of a hidden prompt"},
				},
				Examples: []string{"amux harness login", "amux harness login codex", "printf %s \"$KEY\" | amux harness login claude --api-key --token-stdin"}},
			{Name: "logout", Args: "<claude|codex>", Summary: "fence delivery and clear a harness's central login"},
			harnessAuthSpec(),
			sandboxSpec(),
		},
	}
}

func sandboxSpec() *cmdSpec {
	return &cmdSpec{
		Name: "sandbox", Args: "[<command>]",
		Summary: "each agent's private copy of your harness config: drift, promote, reset",
		Detail: `Every agent runs Claude Code / Codex against a private copy of your own config
dir (~/.claude, $CODEX_HOME), seeded from it as a template and pointed at via
CLAUDE_CONFIG_DIR / CODEX_HOME. The agent may edit its copy. amux compares each
copy with the template and reports what diverged, so you decide whether an
agent's change should propagate to your config (and so to every new agent) or
be discarded. Nothing propagates on its own. Credentials are shared, never
copied. <id> is the agent's store id (from amux ls), or "console".`,
		Sub: []*cmdSpec{
			{Name: "drift", Aliases: []string{"changes", "ls", "list"}, Args: "[<id>] [--json]",
				Summary: "list config paths that diverged from the template, per agent (the default)",
				Detail: `Statuses:
  agent-changed | agent-added | agent-removed  the agent's edit; promote or reset it
  template-changed  you changed the template since the copy was seeded; reset
                    pulls the new version in
  conflict          both sides changed; pick with promote/reset
  shared-detached   the agent's auth file became a private copy; reset re-links it`,
				Flags: []flagSpec{{Name: "json", Usage: "print as JSON"}}},
			{Name: "promote", Args: "<id> <path>", Summary: "propagate the agent's version of <path> into your config"},
			{Name: "reset", Args: "<id> <path>", Summary: "discard the agent's version; re-copy the template's"},
			{Name: "path", Args: "<id>", Summary: "print the agent's private config dir(s) and env"},
		},
	}
}

func harnessAuthSpec() *cmdSpec {
	return &cmdSpec{
		Name: "auth", Args: "<command>",
		Summary: "central logins: status, accounts, default, rebind, restart",
		Detail: `Run logins in a host terminal or the trusted amux console terminal. Other agent
sessions cannot configure accounts. Sessions never hold a refresh token: Claude
receives the setup-token through CLAUDE_CODE_OAUTH_TOKEN, and Codex receives
short-lived ChatGPT tokens from the daemon. A setup-token is valid for one year
and is not refreshed; run login again to reissue it before then. Reissuing
does not revoke the previous token. Status never prints a secret.

One amux-owned configuration is available for each harness. Every workload,
including existing and archived ones, follows its harness's central
configuration. Each new harness process uses the newest successfully
configured login; running processes keep what they received and are never
stopped by login.`,
		Sub: []*cmdSpec{
			{Name: "status", Args: "[claude|codex]", Summary: "show the amux-owned configuration and limits"},
			{Name: "accounts", Args: "[--json]", Summary: "view Claude/Codex configuration and limits",
				Flags: []flagSpec{{Name: "json", Usage: "print as JSON"}}},
			{Name: "default", Args: "<claude|codex>", Summary: "confirm the central configuration (always the default)"},
			{Name: "rebind", Args: "<claude|codex> <id>", Summary: "stop one workload so it relaunches on the current login now"},
			{Name: "login", Hidden: true, Args: "[claude|codex] [--api-key] [--token-stdin]", Summary: "same as amux harness login"},
			{Name: "logout", Hidden: true, Args: "<claude|codex>", Summary: "same as amux harness logout"},
			{Name: "restart", Args: "[--force]", Summary: "restart idle Claude agents with current credentials (conversations resume)",
				Flags: []flagSpec{{Name: "force", Usage: "also interrupt busy or unknown-state agents"}}},
		},
	}
}

// provideFlagSpecs are the settings shared by running the provider and
// installing it (provideFlags.register).
var provideFlagSpecs = []flagSpec{
	{Name: "orchestrator", Arg: "<host:port>", Usage: "orchestrator to dial (or give it as the positional arg)"},
	{Name: "token-file", Arg: "<path>", Usage: "file holding the bearer token (mode 0600); preferred over $AMUX_PROVIDER_TOKEN"},
	{Name: "name", Arg: "<text>", Usage: "provider display name (default $AMUX_PROVIDER_NAME or hostname)"},
	{Name: "ca", Arg: "<pem>", Usage: "private CA to trust on top of the system roots (default $AMUX_TLS_CA)"},
	{Name: "server-name", Arg: "<name>", Usage: "TLS server name for SNI/verification (default $AMUX_TLS_SERVERNAME)"},
	{Name: "max-panes", Arg: "<n>", Usage: "capability: max concurrent panes (default $AMUX_PROVIDER_MAX_PANES)"},
	{Name: "allow-compute", Usage: "allow remote spawn/input/resize/kill (off by default; $AMUX_PROVIDER_ALLOW_COMPUTE)"},
	{Name: "harness", Arg: "<name>", Repeat: true, Usage: "verify and advertise only this harness (auto restores discovery)"},
	{Name: "identity-mode", Arg: "<mode>", Usage: "credential source (machine only: this host's own harness config)"},
	{Name: "label", Arg: "k=v", Repeat: true, Usage: "scheduling label; merged over $AMUX_PROVIDER_LABELS"},
	{Name: "feature", Arg: "<s>", Repeat: true, Usage: "opaque capability feature string; merged with $AMUX_PROVIDER_FEATURES"},
	{Name: "publish-sessions", Usage: "publish this daemon's session inventory and accept lifecycle verbs"},
	{Name: "read-only-sessions", Usage: "publish inventory but reject every lifecycle verb"},
	{Name: "runtime-events", Usage: "stream read-only transcript events (needs --publish-sessions)"},
	{Name: "direct-wake", Usage: "forward content-free direct wake-ups while amux cloud wake enable is in effect"},
	{Name: "direct-enroll", Usage: "offer cloud-assisted direct enrollment while amux cloud enroll enable is in effect"},
	{Name: "account-status", Usage: "allow safe machine login and subscription-limit status (off by default)"},
	{Name: "account-manage", Usage: "retired; configure accounts through amux CLI/TUI"},
	{Name: "account-use", Usage: "allow machine-account use (defaults to writable session publishing)"},
}

func cloudSpec() *cmdSpec {
	return &cmdSpec{
		Name: "cloud", Group: groupManage, Args: "<command>", run: cmdCloud,
		Summary: "reach this machine through Harness cloud",
		Detail: `The cloud connection (provider mode) dials out to Harness over TLS, registers
this machine and serves agent panes over that one connection, so the Harness
web dashboard and phone app reach it from anywhere. A registration owned by
your account is your personal compute pool of kind "machine"; amux cloud
status and amux doctor print its provider id. See docs/remote-provider.md.`,
		Examples: []string{
			"amux cloud connect --orchestrator orch.example.com:7443 --token-file ~/.config/amux/provider.token",
			"amux cloud status",
		},
		Sub: []*cmdSpec{
			{Name: "connect", Args: "[flags]", FlagSet: true, Summary: "connect this machine to Harness cloud: config file plus a user service (survives reboot)",
				Detail: `Writes ~/.config/amux/provider.toml and a user service (a systemd user unit on
Linux/WSL2, a launchd agent on macOS). The token stays in the 0600 file named
by --token-file, so rotating it is one write and no reinstall.`,
				Flags: append(append([]flagSpec{}, provideFlagSpecs...),
					flagSpec{Name: "dry-run", Usage: "print the config file and service unit that would be written, then stop"},
					flagSpec{Name: "exec", Arg: "<path>", Usage: "amux binary the service runs (default: the installed binary, else this one)"}),
				Examples: []string{"amux cloud connect --orchestrator orch.example.com:7443 --token-file ~/.config/amux/provider.token"}},
			{Name: "status", Summary: "the cloud connection: config, token, service, last registration and heartbeat"},
			{Name: "disconnect", Summary: "stop and remove the user service (the config file is kept)"},
			{
				Name: "run", Args: "[<orchestrator-addr>] [flags]", FlagSet: true,
				Summary: "run the cloud connection in the foreground (provider mode)",
				Detail: `Provider mode dials out to a remote orchestrator over TLS, registers this
machine, and serves agent panes over that one connection. With Harness as the
orchestrator, a registration owned by your account is your personal compute
pool of kind "machine", and its pool id is the providerId the orchestrator
assigns (amux doctor prints it). See docs/remote-provider.md.

amux cloud run runs in the foreground. Settings resolve flags first, then
the AMUX_PROVIDER_* / AMUX_TLS_* env vars, then the config file amux cloud
connect wrote. The bearer token is never taken from argv: use --token-file or
AMUX_PROVIDER_TOKEN. Flags may come before or after the address.`,
				Flags: provideFlagSpecs,
				Examples: []string{
					"amux cloud connect --orchestrator orch.example.com:7443 --token-file ~/.config/amux/provider.token",
					"amux doctor",
				},
			},
			{Name: "revoke", Args: "--yes [flags]", FlagSet: true, Summary: "permanently revoke this provider's broker registration and relay access",
				Detail: `Host-only and requires broker support. Uses the saved transport settings unless
flags override them, and waits at most 15 seconds. An unconfirmed result can
be retried with the same token. Direct-client grants remain valid (revoke them
with amux device revoke); config, token file and service are kept.`,
				Flags: []flagSpec{
					{Name: "yes", Required: true, Usage: "confirm the revocation"},
					{Name: "orchestrator", Arg: "<host:port>", Usage: "orchestrator address"},
					{Name: "token-file", Arg: "<path>", Usage: "file holding the provider bearer token"},
					{Name: "ca", Arg: "<pem>", Usage: "PEM CA file"},
					{Name: "server-name", Arg: "<name>", Usage: "TLS server name"},
				}},
			{Name: "enroll", Args: "<command>", Summary: "let Harness cloud connect devices, up to a ceiling (off by default)",
				Detail: `Enabling cloud enrollment trusts the pinned Harness owner, through Harness, to
mint enrollment codes up to the ceiling. Codes are bound to the requesting
client's PKCE verifier and last 5 minutes. Every enable, disable or re-pin
invalidates outstanding cloud codes; grants already redeemed stay until revoked
(amux device revoke).`,
				Sub: []*cmdSpec{
					{Name: "enable", Args: "[--permissions <list>] [--all-sessions | --workgroup <id>...] [flags]", FlagSet: true,
						Summary: "allow Harness to mint enrollment codes up to a ceiling",
						Detail: `The ceiling defaults to read,steer,approve (read, steer and the execute set)
over all ordinary sessions, never the amux console; manage, create and accounts
are opt-in, and --workgroup narrows the scope. A request naming no permissions
gets read plus whatever of steer and approve the ceiling includes. --owner and
--provider default to the owner and provider Harness
reported at this host's latest Harness cloud registration (shown by amux
doctor); a flag that disagrees with it is refused unless both are given.`,
						Flags: []flagSpec{
							{Name: "permissions", Arg: "<list>", Default: "read,steer,approve", Usage: "ceiling (see amux device connect --help); codes never exceed it"},
							{Name: "all-sessions", Usage: "ceiling: any ordinary session (the default)"},
							{Name: "workgroup", Arg: "<root-id>", Repeat: true, Usage: "ceiling: only these workgroups (not with --all-sessions)"},
							{Name: "owner", Arg: "<user-id>", Usage: "advanced: the Harness user id allowed to request codes"},
							{Name: "provider", Arg: "<id>", Usage: "advanced: this host's Harness provider id"},
							{Name: "max-outstanding", Arg: "<n>", Usage: "unredeemed cloud codes at once (default 3)"},
							{Name: "max-per-hour", Arg: "<n>", Usage: "cloud codes per hour (default 10)"},
						},
						Examples: []string{"amux cloud enroll enable --permissions read,steer --all-sessions"}},
					{Name: "disable", Summary: "turn cloud enrollment off (invalidates outstanding codes)"},
					{Name: "status", Args: "[--json]", Summary: "show the cloud enrollment policy",
						Flags: []flagSpec{{Name: "json", Usage: "print as JSON"}}},
				}},
			{Name: "wake", Args: "<command>", Summary: "content-free cloud wake-ups for connected devices (opt-in)",
				Detail: "See docs/direct-wake-contract.md. Pins default from this host's live provider registration.",
				Sub: []*cmdSpec{
					{Name: "enable", Args: "[--owner <id> --provider <id>]", FlagSet: true, Summary: "allow wake-ups, then restart amux cloud run with --direct-wake",
						Flags: []flagSpec{
							{Name: "owner", Arg: "<harness-user-id>", Usage: "advanced: pin the Harness owner"},
							{Name: "provider", Arg: "<provider-id>", Usage: "advanced: pin this host's provider id"},
						}},
					{Name: "disable", Summary: "turn wake-ups off"},
					{Name: "status", Args: "[--json]", Summary: "show the wake-up policy",
						Flags: []flagSpec{{Name: "json", Usage: "print as JSON"}}},
				}},
		},
	}
}

// directServeFlags are the listener settings serve takes and enable saves.
var directServeFlags = []flagSpec{
	{Name: "listen", Arg: "<ip:port>", Usage: "address to bind (a private or loopback IP literal)"},
	{Name: "endpoint", Arg: "<origin>", Repeat: true, Usage: "https origin clients dial, e.g. https://amux.home.example:8443"},
	{Name: "allow-origin", Arg: "<origin>", Repeat: true, Usage: "exact browser origin allowed to call"},
	{Name: "tls-cert", Arg: "<pem>", Usage: "certificate chain (browser use needs a browser-trusted one)"},
	{Name: "tls-key", Arg: "<pem>", Usage: "private key"},
	{Name: "allow-public-bind", Usage: "permit a non-private listen address (deliberate exposure)"},
	{Name: "access-ttl", Arg: "<dur>", Usage: "access token lifetime (default and maximum 10m)"},
	{Name: "idle-ttl", Arg: "<dur>", Usage: "refresh idle lifetime (default and maximum 720h)"},
	{Name: "absolute-ttl", Arg: "<dur>", Usage: "grant absolute lifetime (default and maximum 2160h)"},
}

const directPermissionsHelp = `comma list; read is always required:
  read     inventory, history, live stream
  steer    prompt, interject, stop
  respond  goal control
  approve  answer permission requests (allow/deny)
  manage   start, rename, archive, unarchive
  create   add-agent; new workgroups (--all-sessions only)
  accounts read machine account status (--all-sessions only)
  execute  alias for approve (stored and shown as approve)
e.g. read,steer or read,steer,execute,manage
Nothing implies anything else; there is no "all".`

func deviceSpec() *cmdSpec {
	serveFlags := append(append([]flagSpec{}, directServeFlags...),
		flagSpec{Name: "managed", Usage: "internal: the daemon's child; runs the saved settings and takes no other flag"})
	enableFlags := append(append([]flagSpec{}, directServeFlags...),
		flagSpec{Name: "tailnet", Usage: "derive listen, endpoint and certificate from this machine's tailnet\n(--tailnet=false leaves tailnet mode)"},
		flagSpec{Name: "port", Arg: "<n>", Usage: "tailnet: port for the derived listen address and endpoint (default 8443)"},
		flagSpec{Name: "tailscale-socket", Arg: "<path>", Usage: "tailnet: tailscaled socket (default: the platform's)"},
		flagSpec{Name: "no-default-origin", Usage: "tailnet: do not allow the Harness web origin by default (registered.webOrigin;\nwithout it, pass --allow-origin <Harness web origin>)"},
		flagSpec{Name: "no-wait", Usage: "do not wait for the listener to become ready"},
	)
	return &cmdSpec{
		Name: "device", Group: groupManage, run: cmdDevice,
		Summary: "phones and browsers that reach this machine directly (host only)",
		Detail: `The device listener (the direct server) is an opt-in HTTPS listener on a
private address. A phone or browser connects with a one-time code you paste or
scan (connect --qr), then reaches this host's sessions with destination-issued
credentials. No cloud service is in
the path. See docs/direct-access.md.

Disabling the managed listener is not revocation: clients stay enrolled and
the cloud policy stays as it is. Use amux device revoke and amux cloud
enroll disable for those.`,
		Sub: []*cmdSpec{
			{Name: "connect", Args: "[flags]", FlagSet: true, Summary: "connect a phone or browser: a one-time code to paste, or a QR code to scan",
				Detail: `Needs the listener (amux device enable). The running server publishes its endpoints into the bundle. Before minting,
enroll prints the effective label, permissions and scope on stderr. Without QR
flags it prints a v1 bundle (amux-direct:1:...) alone on stdout, to paste into
a client.

The scope defaults to every ordinary session (--all-sessions); --workgroup
narrows it. The amux console is never in any scope.

QR enrollment (--qr, --qr-png): the code is a device ticket (amux-direct:2:...),
minted locally with no cloud involved: the phone verifies the server identity,
then claims the ticket with its own secret before redeeming it, so a photo of
the code is useless once the phone has claimed it. It is single use and
expires like a bundle. The same ticket is printed alone on stdout, where it can
be piped or pasted into the app's ticket box. The QR code and instructions go
to stderr when stderr is a terminal, otherwise to the controlling terminal
(/dev/tty); with no terminal at all, --qr fails before minting (use --qr-png).`,
				Flags: []flagSpec{
					{Name: "label", Arg: "<text>", Default: "<hostname>-<YYYYMMDD-HHMM>", Usage: "name for the device, shown by amux device ls"},
					{Name: "permissions", Arg: "<list>", Default: "read,steer,approve", Usage: directPermissionsHelp},
					{Name: "all-sessions", Usage: "grant every ordinary session on this host (the default)"},
					{Name: "workgroup", Arg: "<root-id>", Repeat: true, Usage: "grant only this workgroup and its members (not with --all-sessions)"},
					{Name: "ttl", Arg: "<dur>", Default: "10m", Usage: "code lifetime (maximum 1h)"},
					{Name: "json", Usage: "print the full response as JSON on stdout"},
					{Name: "qr", Usage: "also draw the enrollment as a QR code for the Harness Android app\n(Backends > Connect a machine from a QR code)"},
					{Name: "invert", Usage: "swap the QR's filled and empty cells (terminals that ignore colors\nwith a dark background); applies to --qr"},
					{Name: "qr-png", Arg: "<path>", Usage: "also write the QR code as a PNG to a NEW file (mode 0600; an existing\nfile is refused); delete it after enrolling"},
					{Name: "replace", Usage: "once redeemed, revoke earlier clients with the same label from the same\ndevice (re-enrolling a phone without piling up duplicates)"},
				},
				Examples: []string{
					directEnrollExample,
					"amux device connect --qr",
					"amux device connect --label pixel --permissions read,steer --workgroup 771951 --qr",
				}},
			{Name: "ls", Aliases: []string{"list"}, Args: "[--stale [--idle-days N]] [--json]", FlagSet: true,
				Summary: "list connected devices (enrolled clients), with any in-place grant changes",
				Detail: `--stale lists only active clients that look unused: superseded by a newer
enrollment from the same device (same client kind and name, or device id), or
not seen (no token refresh) for --idle-days days (default 14).`,
				Flags: []flagSpec{
					{Name: "stale", Usage: "only clients that look superseded or unused"},
					{Name: "idle-days", Arg: "<n>", Default: "14", Usage: "with --stale: days without a token refresh"},
					{Name: "json", Usage: "print as JSON"},
				}},
			{Name: "update", Args: "<client-id> [--permissions <list>] [--all-sessions | --workgroup <id>...]", FlagSet: true,
				Summary: "change a device's permissions or scope in place (no reconnect needed)",
				Detail: `The client keeps its identity and credentials. Narrowing applies at once:
open streams and calls that depended on a dropped right are ended (the client
reconnects under the new grant). Widening applies to the client's next request.
Omitted parts are kept. create and accounts need --all-sessions: narrowing to
workgroups drops them when inherited and refuses them when listed. Every change
is recorded on the client (amux device ls) and in amux device audit.`,
				Flags: []flagSpec{
					{Name: "permissions", Arg: "<list>", Usage: "the complete new permission list (read is always required)"},
					{Name: "all-sessions", OneOf: []string{"workgroup"}, Usage: "widen the scope to every session on this host"},
					{Name: "workgroup", Arg: "<root-id>", OneOf: []string{"all-sessions"}, Repeat: true, Usage: "set the scope to these workgroups"},
					{Name: "json", Usage: "print the result as JSON"},
				},
				Examples: []string{
					"amux device update c_AbC... --permissions read,steer,approve",
					"amux device update c_AbC... --workgroup 771951",
				}},
			{Name: "revoke", Args: "<client-id>", Summary: "disconnect a device now (closes its streams first)"},
			{Name: "status", Args: "[--json]", Summary: "setting, managed listener, server and provider offer",
				Flags: []flagSpec{{Name: "json", Usage: "print as JSON"}}},
			{Name: "enable", Args: "[flags]", FlagSet: true, Summary: "save the listener settings and turn it on (the daemon runs it from now on)",
				Detail: `Off by default. Starts the daemon if it is stopped. Typed flags merge over the
saved settings; a repeatable flag replaces its saved list. With --tailnet the
listen address, endpoint and certificate come from tailscaled's LocalAPI; typed
serve flags override what it derives, and an empty value returns that setting
to derivation. The tailnet certificate is reused from amux's store or
~/.config/amux/tls when it has over 30 days left, else obtained from
tailscaled, and renewed in place while serving.`,
				Flags: enableFlags,
				Examples: []string{
					"amux device enable --tailnet",
					"amux device enable --listen 192.168.1.10:8443 --endpoint https://amux.home.example:8443 --tls-cert cert.pem --tls-key key.pem",
				}},
			{Name: "disable", Args: "[--no-wait]", FlagSet: true, Summary: "turn the managed listener off (settings, clients and cloud policy are kept)",
				Flags: []flagSpec{{Name: "no-wait", Usage: "do not wait for the listener to stop"}}},
			{Name: "logs", Args: "[-n lines]", FlagSet: true, Summary: "the managed listener's log",
				Flags: []flagSpec{{Name: "n", Arg: "<lines>", Default: "50", Usage: "lines to show"}}},
			{Name: "audit", Args: "[--client <id>] [--limit N] [--json]",
				Summary: "answers, item triage and overseer steers made by direct clients (newest last)",
				Flags: []flagSpec{
					{Name: "client", Arg: "<id>", Usage: "only this client's actions"},
					{Name: "limit", Arg: "<n>", Usage: "at most n records"},
					{Name: "json", Usage: "print as JSON"},
				}},
			{Name: "serve", Args: "[flags]", FlagSet: true, Summary: "run the device listener in the foreground (manual)",
				Detail: `Plain amux device serve runs the saved tailnet configuration in the foreground
only while the managed listener is disabled. --tls-cert/--tls-key default to
$AMUX_TLS_CERT/$AMUX_TLS_KEY.`,
				Flags: serveFlags},
			{Name: "reset-identity", Args: "--yes", Summary: "replace the server identity; revokes every client and disables cloud enrollment",
				Flags: []flagSpec{{Name: "yes", Required: true, Usage: "confirm"}}},
		},
	}
}

func daemonSpec() *cmdSpec {
	return &cmdSpec{
		Name: "daemon", Group: groupManage, Args: "[<command>]", run: cmdDaemon,
		Summary: "run or control the local daemon (restart loads a new binary)",
		Detail: `The daemon owns the store and the agent engine: it hosts every running agent,
so stopping or restarting it stops the agents it hosts. Bare amux daemon runs
one in the foreground until signalled (what auto-launch spawns); the commands
below control a detached one.`,
		Sub: []*cmdSpec{
			{Name: "stop", Aliases: []string{"down"}, Summary: "stop the running daemon cleanly"},
			{Name: "start", Aliases: []string{"up"}, Summary: "start a detached daemon if none is answering"},
			{Name: "restart", Aliases: []string{"reload"}, Summary: "stop, then start: how a newly installed binary is loaded"},
		},
	}
}
