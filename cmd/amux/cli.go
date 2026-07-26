package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/mattn/go-isatty"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/gh"
	"amux/internal/store"
)

// This file is the CLI surface for repos and workgroups. It is deliberately a
// slim wrapper over the daemon: reads go through queryRows (QueryRepos /
// QuerySessions) and mutations through sendAction, so the CLI never opens the
// store or runs store/git side effects itself — the daemon (the single owner of
// the store and the agent engine) does. Only genuinely client-side interaction
// (fzf menus, the gh owner browser) lives here, since it needs a real TTY.

// cmdName sets the display name of the agent the caller is running inside. Agents
// launch with $AMUX_WORKGROUP set to their id (see wsops.AgentCommand), so this
// works from an agent's own shell or terminal tab. Exposed as "amux agent name"
// (and the "label" alias); "amux name" is kept as a deprecated top-level alias.
func cmdName(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: amux agent name <display name>")
	}
	id := strings.TrimSpace(os.Getenv("AMUX_WORKGROUP"))
	if id == "" {
		return fmt.Errorf("%s", notInsideAgent("amux agent name", "amux workgroup rename <id> <name>"))
	}
	name := strings.Join(args, " ")
	if err := sendAction(core.Action{Action: core.ActionRename, ID: id, Fields: map[string]string{"name": name}}); err != nil {
		return err
	}
	fmt.Printf("renamed agent %s to %q\n", id, name)
	return nil
}

// notInsideAgent explains a failed self-scoped command: one that acts on
// *whoever ran it* rather than on an id. Those commands know their caller only by
// the $AMUX_WORKGROUP amux exports into every pane it launches, so from a plain
// terminal there is no caller to act on. Naming the unset variable isn't enough —
// say where the command does work, and give the by-id form for everywhere else.
func notInsideAgent(cmd, byID string) string {
	return "not inside an amux agent ($AMUX_WORKGROUP unset)\n" +
		"  `" + cmd + "` acts on the agent that runs it, so it only works from that\n" +
		"  agent's own terminal tab inside amux (where amux sets $AMUX_WORKGROUP to the\n" +
		"  agent's id). From a plain shell, name the target instead: `" + byID + "`\n" +
		"  (`amux workgroup ls` lists the ids)."
}

// agentCfg is one agent's configuration gathered in the interactive create flow,
// then handed to the daemon as add-agent fields. It mirrors the daemon's
// AgentSpec but keeps the CLI free of the store/wsops layer.
type agentCfg struct {
	Repos  []string
	Agent  string // harness: claude | codex (defaults to claude)
	Mode   string
	Model  string
	Prompt string
}

// ---- repo commands -------------------------------------------------------

func cmdRepo(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: amux repo <add|ls|rm> ...")
	}
	if isHelpFlag(args[0]) {
		repoUsage()
		return nil
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			return addReposInteractive(bufio.NewReader(os.Stdin))
		}
		// Non-interactive: the daemon's add-repo resolves a gh slug, git URL, or
		// local path and clones it — the CLI just forwards the source string.
		src := args[1]
		fmt.Printf("tracking %s…\n", src)
		if err := sendAction(core.Action{Action: core.ActionAddRepo, Fields: map[string]string{"source": src}}); err != nil {
			return err
		}
		fmt.Printf("tracked %s\n", src)
		return nil
	case "ls", "list":
		repos, err := queryRepos()
		if err != nil {
			return err
		}
		if len(repos) == 0 {
			fmt.Println("(no repositories tracked — `amux repo add <url|path>`)")
			return nil
		}
		for _, r := range repos {
			fmt.Printf("%-24s %s\n", r.Name, r.Source)
		}
		return nil
	case "rm", "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux repo rm <name>")
		}
		if err := sendAction(core.Action{Action: core.ActionRmRepo, ID: args[1]}); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown repo subcommand %q\nvalid subcommands:\n"+
			"  add <url|path|OWNER/REPO>  track a repo (no argument browses GitHub)\n"+
			"  ls                         list tracked repos\n"+
			"  rm <name>                  untrack a repo", args[0])
	}
}

func repoUsage() {
	fmt.Fprint(os.Stderr, `amux repo — track the repositories agents get worktrees of

usage: amux repo <command>

  add <src>          track a repo: a gh "owner/name" slug, a git URL, or a local
                     path. With no <src>, browse your GitHub owners and pick
                     (needs a terminal).
  ls                 list tracked repositories  (alias: list)
  rm <name>          stop tracking a repository  (alias: remove)
`)
}

const manualEntryItem = "✎  enter a URL or local path…"

// stdinIsTerminal reports whether the CLI has a terminal on stdin. It's a
// variable so tests can drive the interactive verbs without one.
var stdinIsTerminal = func() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())
}

// requireTerminal guards the interactive pages (the fzf menus and the typed
// prompts behind them). Without a terminal — a pipe, a script, a hook — fzf has
// no screen to draw on and every prompt reads EOF, so the flow would either hang
// or silently run with blank answers. Saying so is the honest failure.
func requireTerminal(what string) error {
	if stdinIsTerminal() {
		return nil
	}
	return fmt.Errorf("%s needs an interactive terminal; run it from a shell, or use a non-interactive form (--help lists them)", what)
}

// addReposInteractive drives the gh/fzf owner→repo browser to gather one or more
// repo sources, then asks the daemon to clone and track each. The browsing needs
// a TTY so it stays client-side; the cloning is the daemon's job.
func addReposInteractive(in *bufio.Reader) error {
	if err := requireTerminal("amux repo add (interactive)"); err != nil {
		return err
	}
	sources, err := pickRepoSources(context.Background(), in)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("nothing selected")
	}
	var tracked int
	for _, s := range sources {
		fmt.Printf("tracking %s…\n", s)
		if err := sendAction(core.Action{Action: core.ActionAddRepo, Fields: map[string]string{"source": s}}); err != nil {
			fmt.Printf("  failed: %v\n", err)
			continue
		}
		fmt.Printf("tracked %s\n", s)
		tracked++
	}
	if tracked == 0 {
		return fmt.Errorf("nothing tracked")
	}
	return nil
}

// pickRepoSources returns repo source strings (gh "owner/name" slugs, or a
// manually entered URL/path) chosen through the gh owner browser. It performs no
// cloning — the daemon's add-repo does that.
func pickRepoSources(ctx context.Context, in *bufio.Reader) ([]string, error) {
	if !gh.Installed() {
		fmt.Println("GitHub CLI (gh) is not installed — see https://cli.github.com")
		return manualSource(in)
	}
	if !gh.Authed(ctx) {
		fmt.Print("Not signed in to GitHub. Authenticate now? [Y/n] ")
		line, _ := in.ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans == "n" || ans == "no" {
			return nil, fmt.Errorf("GitHub authentication required")
		}
		if err := gh.Login(ctx); err != nil {
			return nil, fmt.Errorf("gh auth login failed: %w", err)
		}
		if !gh.Authed(ctx) {
			return nil, fmt.Errorf("still not authenticated")
		}
	}
	owners, err := gh.Owners(ctx)
	if err != nil || len(owners) == 0 {
		return nil, fmt.Errorf("could not list GitHub owners: %v", err)
	}
	owner, err := fzfSelect("owner / org", append([]string{manualEntryItem}, owners...))
	if err != nil {
		return nil, err
	}
	if owner == manualEntryItem {
		return manualSource(in)
	}
	remotes, err := gh.ListReposFor(ctx, owner)
	if err != nil {
		return nil, fmt.Errorf("listing %s repos: %w", owner, err)
	}
	if len(remotes) == 0 {
		return nil, fmt.Errorf("no repositories found for %s", owner)
	}
	items := []string{manualEntryItem}
	for _, r := range remotes {
		items = append(items, r.NameWithOwner)
	}
	picks, err := fzfMultiSelect("repos in "+owner, items)
	if err != nil {
		return nil, err
	}
	var sources []string
	for _, p := range picks {
		if p == manualEntryItem {
			if extra, err := manualSource(in); err == nil {
				sources = append(sources, extra...)
			}
			continue
		}
		sources = append(sources, p)
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("nothing selected")
	}
	return sources, nil
}

func manualSource(in *bufio.Reader) ([]string, error) {
	fmt.Print("Git URL or local path: ")
	line, _ := in.ReadString('\n')
	src := strings.TrimSpace(line)
	if src == "" {
		return nil, fmt.Errorf("no source given")
	}
	return []string{src}, nil
}

// startCreated asks the daemon to start the agent process(es) for a freshly
// created session — a workgroup root (all its agents) or a single agent id —
// mirroring the TUI, where creating a session and switching to it starts it. It's
// best-effort: the session is already persisted, so a failure to start is a
// warning, not a failure of the create command.
func startCreated(id string) {
	if err := sendAction(core.Action{Action: core.ActionStart, ID: id}); err != nil {
		fmt.Fprintf(os.Stderr, "amux: created %s but could not start it (%v)\n", id, err)
	}
}

// ---- workgroup commands --------------------------------------------------

func cmdSession(args []string) error {
	ctx := context.Background()
	// Bare `amux workgroup` is the create page with nothing pre-attached — the
	// same as `workgroup new` with no seed repos. rest carries the subcommand's
	// own arguments, and is empty (not args[1:], which would be out of bounds)
	// when there was no subcommand word at all.
	sub, rest := "new", []string(nil)
	if len(args) > 0 {
		sub, rest = args[0], args[1:]
	}
	if isHelpFlag(sub) {
		workgroupUsage()
		return nil
	}
	switch sub {
	case "new":
		return sessionNew(ctx, rest) // optional seed repos to pre-attach
	case "open", "switch":
		if len(rest) == 0 {
			return fmt.Errorf("usage: amux workgroup open <id>")
		}
		return sessionOpen(rest[0])
	case "add":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux workgroup add <workgroup-id> [repo...]")
		}
		if len(args) == 2 {
			return sessionAdd(ctx, args[1]) // interactive
		}
		// Non-interactive: amux workgroup add <root> <repo>... [--mode m] [--model M] [--prompt t]
		repos, cfg := parseCreateFlags(args[2:])
		id, err := sendActionID(core.Action{Action: core.ActionAddAgent, ID: args[1], Fields: map[string]string{
			"repos": strings.Join(repos, ","), "agent": cfg.agent, "mode": cfg.mode, "model": cfg.model, "prompt": cfg.prompt,
		}})
		if err != nil {
			return err
		}
		fmt.Printf("added agent %s to %s (repos: %s)\n", id, args[1], orNone(strings.Join(repos, ", ")))
		startCreated(id)
		return nil
	case "create":
		// amux workgroup create <repo>... [--name n] [--prompt t] [--mode m] [--model M]
		// Creates a workgroup plus one default agent scoped to the given repos.
		repos, cfg := parseCreateFlags(args[1:])
		rootID, err := sendActionID(core.Action{Action: core.ActionCreateWorkspace, Fields: map[string]string{
			"name": cfg.name, "repos": strings.Join(repos, ","), "agent": cfg.agent,
			"mode": cfg.mode, "model": cfg.model, "prompt": cfg.prompt, "defaultAgent": "1",
		}})
		if err != nil {
			return err
		}
		fmt.Printf("created workgroup %s (agent repos: %s)\n", rootID, orNone(strings.Join(repos, ", ")))
		startCreated(rootID)
		return nil
	case "repo":
		// amux workgroup repo <repo> [--name n] [--prompt t] [--mode m] [--model M]
		// Creates a single-repo (repo-scoped) workgroup + its one agent.
		if len(args) < 2 {
			return fmt.Errorf("usage: amux workgroup repo <repo> [--prompt t] [--mode m] [--model M]")
		}
		_, cfg := parseCreateFlags(args[2:])
		id, err := sendActionID(core.Action{Action: core.ActionNewRepoAgent, ID: args[1], Fields: map[string]string{
			"agent": cfg.agent, "mode": cfg.mode, "model": cfg.model, "prompt": cfg.prompt,
		}})
		if err != nil {
			return err
		}
		fmt.Printf("created repo-scoped agent %s on %s\n", id, args[1])
		startCreated(id)
		return nil
	case "move":
		// amux workgroup move <agentID> [<targetRootID> | --new]
		if len(args) < 2 {
			return fmt.Errorf("usage: amux workgroup move <agentID> [<targetRootID>|--new]")
		}
		target := ""
		if len(args) > 2 && args[2] != "--new" {
			target = args[2]
		}
		if err := sendAction(core.Action{Action: core.ActionMove, ID: args[1], Target: target}); err != nil {
			return err
		}
		if target == "" {
			fmt.Printf("moved agent %s into a new work-scoped workgroup\n", args[1])
		} else {
			fmt.Printf("moved agent %s into workgroup %s\n", args[1], target)
		}
		return nil
	case "repos":
		// amux workgroup repos <agent-id> <repo>... — re-scope an agent to exactly
		// these tracked repos (adds/removes worktrees to match).
		if len(args) < 2 {
			return fmt.Errorf("usage: amux workgroup repos <agent-id> <repo>...")
		}
		repos := args[2:]
		if err := sendAction(core.Action{Action: core.ActionAgentSetRepos, ID: args[1], Fields: map[string]string{"repos": strings.Join(repos, ",")}}); err != nil {
			return err
		}
		fmt.Printf("agent %s repos: %s\n", args[1], orNone(strings.Join(repos, ", ")))
		return nil
	case "rm", "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux workgroup rm <id>")
		}
		if err := sendAction(core.Action{Action: core.ActionDelete, ID: args[1]}); err != nil {
			return err
		}
		fmt.Printf("removed %s\n", args[1])
		return nil
	case "rename":
		if len(args) < 3 {
			return fmt.Errorf("usage: amux workgroup rename <id> <name>")
		}
		return sendAction(core.Action{Action: core.ActionRename, ID: args[1], Fields: map[string]string{"name": strings.Join(args[2:], " ")}})
	case "archive", "done":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux workgroup archive <id>")
		}
		if err := sendAction(core.Action{Action: core.ActionSetArchived, ID: args[1], Fields: map[string]string{"archived": "true"}}); err != nil {
			return err
		}
		fmt.Printf("archived %s\n", args[1])
		return nil
	case "unarchive", "restore":
		if len(args) < 2 {
			return fmt.Errorf("usage: amux workgroup unarchive <id>")
		}
		if err := sendAction(core.Action{Action: core.ActionSetArchived, ID: args[1], Fields: map[string]string{"archived": "false"}}); err != nil {
			return err
		}
		fmt.Printf("restored %s\n", args[1])
		return nil
	case "ls", "list":
		return sessionList()
	default:
		return fmt.Errorf("unknown workgroup subcommand %q\nvalid subcommands:\n"+
			"  new                          create a workgroup on a config page (default)\n"+
			"  create <repo>...             create one non-interactively, with a first agent\n"+
			"  repo <repo>                  start a repo-scoped agent on a tracked repo\n"+
			"  add <id> [repo...]           add another agent to a workgroup\n"+
			"  move <agent> [<id>|--new]    re-parent an agent into another workgroup\n"+
			"  repos <agent> <repo>...      re-scope an agent to exactly these repos\n"+
			"  rename <id> <name>           set a display name (the id is unchanged)\n"+
			"  archive | unarchive <id>     mark done / bring back (reversible)\n"+
			"  rm <id>                      delete for good — worktrees + branch\n"+
			"  ls                           list workgroups and their agents", sub)
	}
}

func workgroupUsage() {
	fmt.Fprint(os.Stderr, `amux workgroup — create, open, and manage workgroups and their agents
(aliases: wg, session, ses, workspace, ws)

usage: amux workgroup [command]

  (bare)             the interactive create page (same as "new")
  new [repo...]      create a workgroup through the config page, optionally
                     pre-attaching an agent scoped to the given repos
  create <repo>...   non-interactive: workgroup + one agent on those repos
                     [--name n] [--prompt t] [--agent a] [--mode m] [--model M]
  repo <repo>        create a single-repo (repo-scoped) agent on a tracked repo
  add <root> [repo...]  add another agent to an existing workgroup
  open <id>          open the dashboard on a workgroup or agent  (alias: switch)
  ls                 list workgroups and their agents  (alias: list)
  move <agent> [<root>|--new]  re-parent an agent into another workgroup
  repos <agent> <repo>...  re-scope an agent to exactly these tracked repos
  rename <id> <name>  set a display name (the id is unchanged)
  archive <id>       drop a session off the active rail  (alias: done)
  unarchive <id>     put an archived session back  (alias: restore)
  rm <id>            delete a session, its worktrees and branches  (alias: delete)
`)
}

// sessionOpen opens the dashboard on one workgroup or agent — the CLI form of
// picking it in the rail. The id is resolved against the daemon's session model
// first, so a typo is a plain error here instead of a dashboard that opens on
// nothing; the TUI then attaches to it (starting it, as opening a session does).
func sessionOpen(id string) error {
	roots, err := querySessions()
	if err != nil {
		return err
	}
	if !knownSessionID(roots, id) {
		return fmt.Errorf("no workgroup or agent %q (see `amux workgroup ls`)", id)
	}
	return cmdNative(id)
}

// knownSessionID reports whether id names a workgroup root or one of its agents.
func knownSessionID(roots []core.WorkgroupRow, id string) bool {
	for _, r := range roots {
		if r.ID == id {
			return true
		}
		for _, a := range r.Agents {
			if a.ID == id {
				return true
			}
		}
	}
	return false
}

func sessionList() error {
	roots, err := querySessions()
	if err != nil {
		return err
	}
	for _, r := range roots {
		fmt.Printf("%-8s %-6s %s\n", r.ID, defaultStr(r.Scope, "work"), orID(r.Display, r.ID))
		for _, s := range r.Agents {
			tag := ""
			if s.Archived {
				tag = " [archived]"
			}
			fmt.Printf("  %-8s %-8s %-6s %s%s\n", s.ID, agent.Canonical(s.Agent), s.Mode,
				strings.ReplaceAll(s.Repos, ",", "+"), tag)
		}
	}
	return nil
}

// sessionNew is the interactive create page: name the workgroup, optionally
// configure agents (each scoped to fuzzy-picked tracked repos, otherwise a
// repo-less default agent), then Create. Repos are an attribute of an agent, not
// the workgroup, so there's no workgroup-level repo list here.
func sessionNew(ctx context.Context, seedRepos []string) error {
	if err := requireTerminal("amux workgroup new"); err != nil {
		return err
	}
	in := bufio.NewReader(os.Stdin)
	name := ""
	var agents []agentCfg
	// A seed repo (e.g. from the rail) pre-configures a first agent scoped to it.
	if len(seedRepos) > 0 {
		agents = append(agents, agentCfg{Repos: seedRepos, Agent: agent.DefaultKind(), Mode: "task", Model: defaultModelFor(agent.DefaultKind())})
	}

	for {
		menu := []string{
			fmt.Sprintf("Name   › %s", orOptional(name)),
			"+ add agent",
		}
		for i, a := range agents {
			menu = append(menu, fmt.Sprintf("  agent %d: %s", i+1, describeAgent(a)))
		}
		menu = append(menu, "────────────────", "✓ Create workgroup", "✗ Cancel")

		choice, err := fzfMenu("new workgroup", menu)
		if err != nil {
			return nil // Esc
		}
		switch {
		case strings.HasPrefix(choice, "Name"):
			name = promptLine(in, "Workgroup name (optional)")
		case strings.HasPrefix(choice, "+ add agent"):
			if a, ok := configureAgent(ctx, in); ok {
				agents = append(agents, a)
			}
		case strings.HasPrefix(strings.TrimSpace(choice), "agent "):
			if i := agentIndex(choice); i >= 0 && i < len(agents) {
				agents = append(agents[:i], agents[i+1:]...)
			}
		case strings.HasPrefix(choice, "✓"):
			return createWorkspace(name, agents)
		case strings.HasPrefix(choice, "✗"):
			return nil
		}
	}
}

// createWorkspace creates the workgroup over the daemon: when the user configured
// no explicit agents we seed one default (repo-less) agent; otherwise we create
// the bare workgroup and add each configured agent, scoped to its picked repos.
func createWorkspace(name string, agents []agentCfg) error {
	fields := map[string]string{"name": name}
	if len(agents) == 0 {
		fields["defaultAgent"] = "1"
		fields["agent"] = agent.DefaultKind()
		fields["mode"] = "task"
		fields["model"] = defaultModelFor(agent.DefaultKind())
	}
	rootID, err := sendActionID(core.Action{Action: core.ActionCreateWorkspace, Fields: fields})
	if err != nil {
		return err
	}
	for _, a := range agents {
		if _, err := sendActionID(core.Action{Action: core.ActionAddAgent, ID: rootID, Fields: map[string]string{
			"repos": strings.Join(a.Repos, ","), "agent": a.Agent, "mode": a.Mode, "model": a.Model, "prompt": a.Prompt,
		}}); err != nil {
			return err
		}
	}
	startCreated(rootID)
	fmt.Printf("created workgroup %s — it's starting; open it in the dashboard (`amux`)\n", rootID)
	return nil
}

func sessionAdd(ctx context.Context, rootID string) error {
	if err := requireTerminal("amux workgroup add (interactive)"); err != nil {
		return err
	}
	in := bufio.NewReader(os.Stdin)
	a, ok := configureAgent(ctx, in)
	if !ok {
		return nil
	}
	id, err := sendActionID(core.Action{Action: core.ActionAddAgent, ID: rootID, Fields: map[string]string{
		"repos": strings.Join(a.Repos, ","), "agent": a.Agent, "mode": a.Mode, "model": a.Model, "prompt": a.Prompt,
	}})
	if err != nil {
		return err
	}
	startCreated(id)
	fmt.Printf("added agent %s — it's starting; open it in the dashboard (`amux`)\n", id)
	return nil
}

// configureAgent presents one review screen for a new agent, pre-filled with
// rational defaults — "task" mode and the user's preferred Claude model — so the
// common case is a single ENTER on "Add agent". Its repos are fuzzy-picked from
// the tracked set (none by default). The menu loops until the user confirms or
// cancels.
func configureAgent(ctx context.Context, in *bufio.Reader) (agentCfg, bool) {
	a := agentCfg{
		Agent: agent.DefaultKind(),                  // default harness (Claude Code)
		Mode:  "task",                               // default: task-style work
		Model: defaultModelFor(agent.DefaultKind()), // default: your usual model
	}
	for {
		menu := []string{
			fmt.Sprintf("Repos   › %s", orNone(strings.Join(a.Repos, ", "))),
			fmt.Sprintf("Harness › %s", a.Agent),
			fmt.Sprintf("Mode    › %s", a.Mode),
			fmt.Sprintf("Model   › %s", orDefault(a.Model)),
			fmt.Sprintf("Prompt  › %s", orOptional(a.Prompt)),
			"────────────────", "✓ Add agent", "✗ Cancel",
		}
		choice, err := fzfMenu("configure agent", menu)
		if err != nil {
			return agentCfg{}, false // Esc
		}
		switch {
		case strings.HasPrefix(choice, "Repos"):
			a.Repos = pickRepos(ctx, in, "repos for this agent (TAB)")
		case strings.HasPrefix(choice, "Harness"):
			// Switching harness re-derives the model default, since each harness draws
			// from its own model set (see defaultModelFor / agent.HarnessFor().Models).
			a.Agent = cycleHarness(a.Agent)
			a.Model = defaultModelFor(a.Agent)
		case strings.HasPrefix(choice, "Mode"):
			a.Mode = pickMode()
		case strings.HasPrefix(choice, "Model"):
			a.Model = pickModel(a.Agent, a.Model)
		case strings.HasPrefix(choice, "Prompt"):
			a.Prompt = promptLine(in, "Initial prompt (optional)")
		case strings.HasPrefix(choice, "✓"):
			return a, true
		case strings.HasPrefix(choice, "✗"):
			return agentCfg{}, false
		}
	}
}

// cycleHarness advances to the next registered harness, wrapping around, so the
// menu entry toggles through the offered set (agent.Kinds, default first).
func cycleHarness(cur string) string {
	kinds := agent.Kinds()
	for i, k := range kinds {
		if k == cur {
			return kinds[(i+1)%len(kinds)]
		}
	}
	return kinds[0]
}

// pickModel offers the harness's selectable models (agent.HarnessFor().Models,
// default first). Esc/blank keeps the current pick.
func pickModel(agentKind, cur string) string {
	choice, err := fzfMenu("model", agent.HarnessFor(agentKind).Models())
	if err != nil || strings.TrimSpace(choice) == "" {
		return cur
	}
	return choice
}

// pickRepos multi-selects tracked repos, offering to clone/add new ones.
func pickRepos(ctx context.Context, in *bufio.Reader, prompt string) []string {
	const clone = "➕  add / clone a repo…"
	for {
		tracked, err := queryRepos()
		if err != nil {
			return nil
		}
		items := []string{clone}
		for _, r := range tracked {
			items = append(items, r.Name)
		}
		picks, err := fzfMultiSelect(prompt, items)
		if err != nil {
			return nil
		}
		var sel []string
		doClone := false
		for _, p := range picks {
			if p == clone {
				doClone = true
			} else {
				sel = append(sel, p)
			}
		}
		if doClone {
			_ = addReposInteractive(in)
			if len(sel) > 0 {
				return sel
			}
			continue // reopen with the freshly-cloned repo available
		}
		return sel
	}
}

func pickMode() string {
	choice, err := fzfMenu("mode", []string{
		"task         — autonomous; runs a specific job to completion, then self-reports done",
		"interactive  — human-driven; you drive it turn by turn and close it yourself",
	})
	if err != nil || strings.HasPrefix(choice, "task") {
		return store.ModeTask
	}
	return store.ModeInteractive
}

func describeAgent(a agentCfg) string {
	repos := "(plain agent)"
	if len(a.Repos) > 0 {
		repos = strings.Join(a.Repos, "+")
	}
	parts := []string{repos, agent.Canonical(a.Agent), a.Mode}
	if a.Model != "" {
		parts = append(parts, a.Model)
	}
	return strings.Join(parts, " · ")
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

func orID(display, id string) string {
	if strings.TrimSpace(display) == "" {
		return id
	}
	return display
}

func agentIndex(line string) int {
	line = strings.TrimSpace(line)
	var n int
	if _, err := fmt.Sscanf(line, "agent %d:", &n); err != nil {
		return -1
	}
	return n - 1
}

// ---- query helpers -------------------------------------------------------

func queryRepos() ([]core.RepoRow, error) {
	var rows []core.RepoRow
	return rows, queryRows(core.QueryRepos, &rows)
}

func querySessions() ([]core.WorkgroupRow, error) {
	var rows []core.WorkgroupRow
	return rows, queryRows(core.QuerySessions, &rows)
}

// ---- shared helpers ------------------------------------------------------

type createCfg struct{ name, prompt, mode, model, agent string }

func parseCreateFlags(args []string) ([]string, createCfg) {
	// Same rational defaults as the interactive flow: the claude harness and task
	// mode. --agent/--mode/--model below override these; the model default is
	// derived from the chosen harness after the scan (see below), so --agent codex
	// gets a Codex model rather than a Claude one.
	cfg := createCfg{mode: "task", agent: agent.DefaultKind()}
	modelSet := false
	var repos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--name" && i+1 < len(args):
			cfg.name = args[i+1]
			i++
		case strings.HasPrefix(a, "--name="):
			cfg.name = strings.TrimPrefix(a, "--name=")
		case a == "--prompt" && i+1 < len(args):
			cfg.prompt = args[i+1]
			i++
		case strings.HasPrefix(a, "--prompt="):
			cfg.prompt = strings.TrimPrefix(a, "--prompt=")
		case a == "--mode" && i+1 < len(args):
			cfg.mode = args[i+1]
			i++
		case strings.HasPrefix(a, "--mode="):
			cfg.mode = strings.TrimPrefix(a, "--mode=")
		case a == "--model" && i+1 < len(args):
			cfg.model = args[i+1]
			modelSet = true
			i++
		case strings.HasPrefix(a, "--model="):
			cfg.model = strings.TrimPrefix(a, "--model=")
			modelSet = true
		case a == "--agent" && i+1 < len(args):
			cfg.agent = args[i+1]
			i++
		case strings.HasPrefix(a, "--agent="):
			cfg.agent = strings.TrimPrefix(a, "--agent=")
		default:
			repos = append(repos, a)
		}
	}
	if !modelSet {
		cfg.model = defaultModelFor(cfg.agent)
	}
	return repos, cfg
}

// defaultModelFor is the model amux pre-fills for a harness: the user's configured
// preference (read from the CLI's own config via the harness), falling back to the
// harness's built-in default when they haven't set one. The offered choices come
// from the same harness's Models().
func defaultModelFor(agentKind string) string {
	h := agent.HarnessFor(agentKind)
	if pref := strings.TrimSpace(h.PreferredModel()); pref != "" {
		return pref
	}
	return h.DefaultModel()
}

func fzfMultiSelect(prompt string, items []string) ([]string, error) {
	out, err := runFzf(items, "--multi", "--prompt", prompt+"> ", "--height", "100%", "--border",
		"--header", "TAB to multi-select, ENTER to confirm, ESC to cancel")
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return lines, nil
}

func fzfSelect(prompt string, items []string) (string, error) {
	out, err := runFzf(items, "--prompt", prompt+"> ", "--height", "100%", "--border",
		"--header", "type to filter, ENTER to select, ESC to cancel")
	return strings.TrimSpace(out), err
}

func fzfMenu(prompt string, items []string) (string, error) {
	out, err := runFzf(items, "--no-sort", "--prompt", prompt+"> ", "--height", "100%", "--border",
		"--header", "↑/↓ then ENTER, ESC to cancel")
	return strings.TrimSpace(out), err
}

func runFzf(items []string, args ...string) (string, error) {
	cmd := exec.Command("fzf", args...)
	cmd.Stdin = strings.NewReader(strings.Join(items, "\n"))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("cancelled")
		}
		return "", fmt.Errorf("fzf not available: %w", err)
	}
	return string(out), nil
}

func promptLine(in *bufio.Reader, label string) string {
	fmt.Printf("%s: ", label)
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

func orOptional(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(optional)"
	}
	return s
}

func orDefault(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(claude default)"
	}
	return s
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}
