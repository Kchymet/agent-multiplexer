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
	"amux/internal/wsops"
)

// This file is the CLI surface for repos and workgroups. It is deliberately a
// slim wrapper over the daemon: reads go through queryRows (QueryRepos /
// QuerySessions) and mutations through sendAction, so the CLI never opens the
// store or runs store/git side effects itself — the daemon (the single owner of
// the store and the agent engine) does. Only genuinely client-side interaction
// (fzf menus, the gh owner browser) lives here, since it needs a real TTY.

// cmdName sets the launched session's display name using its fixed context.
// Environment hints cannot redirect a self-rename, including for coordinators.
// The deprecated top-level name alias shares this implementation.
func cmdName(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: amux agent name <display name>")
	}
	session, err := loadAgentSessionContext()
	if err != nil || strings.TrimSpace(session.SubjectID) == "" {
		return fmt.Errorf("%s", notInsideAgent("amux agent name", "amux rename <id> <name>"))
	}
	id := session.SubjectID
	name := strings.Join(args, " ")
	if _, err := restrictedAction(core.Action{Action: core.ActionRename, ID: id, Fields: map[string]string{"name": name}}); err != nil {
		return err
	}
	fmt.Printf("renamed agent %s to %q\n", id, name)
	return nil
}

// notInsideAgent describes the fixed identity required by self-scoped commands.
func notInsideAgent(cmd, byID string) string {
	return "not inside an amux agent (fixed session context unavailable)\n" +
		"  `" + cmd + "` acts on the launched session from its own terminal tab.\n" +
		"  From a plain shell, use `" + byID + "` (`amux ls` lists ids)."
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
		return usageErr("repo")
	}
	if isHelpFlag(args[0]) {
		return showHelp("repo")
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
			fmt.Println(repoLine(r))
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
	case "set":
		// amux repo set <agent-id> <repo>... — re-scope an agent to exactly
		// these tracked repos (adds/removes worktrees to match).
		if len(args) < 2 {
			return usageErr("repo set")
		}
		repos := args[2:]
		if err := sendAction(core.Action{Action: core.ActionAgentSetRepos, ID: args[1], Fields: map[string]string{"repos": strings.Join(repos, ",")}}); err != nil {
			return err
		}
		fmt.Printf("agent %s repos: %s\n", args[1], orNone(strings.Join(repos, ", ")))
		return nil
	case "grant":
		// Host-only grant administration. --all (the default for a workgroup
		// created without repos) follows every tracked repository live; a repo
		// list fixes the ceiling to exactly those; --none (or an empty list)
		// deliberately seals the workgroup to no repositories.
		if len(args) < 2 {
			return usageErr("repo grant")
		}
		action, label, err := grantsAction(args[1], args[2:])
		if err != nil {
			return err
		}
		if err := sendAction(action); err != nil {
			return err
		}
		fmt.Printf("coordinator %s grants: %s\n", args[1], label)
		return nil
	default:
		return usagef("unknown repo subcommand %q", args[0])
	}
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

// cmdSession is the legacy `amux workgroup` namespace. Its renamed verbs reach
// the new commands' handlers through the same rewrite as the command line
// (legacy.go); the create forms whose operands differ from amux new / amux add
// keep their own parsing here so old scripts still run.
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
		return showHelp("workgroup")
	}
	switch sub {
	case "new":
		return sessionNew(ctx, rest) // optional seed repos to pre-attach
	case "add":
		if len(rest) == 0 {
			return fmt.Errorf("usage: amux add <workgroup> \"<task>\" (was: amux workgroup add <workgroup> [repo...])")
		}
		if len(rest) == 1 {
			return sessionAdd(ctx, rest[0]) // interactive
		}
		// Non-interactive: amux workgroup add <root> <repo>... [--mode m] [--model M] [--prompt t]
		repos, cfg := parseCreateFlags(rest[1:])
		id, err := sendActionID(core.Action{Action: core.ActionAddAgent, ID: rest[0], Fields: map[string]string{
			"repos": strings.Join(repos, ","), "agent": cfg.agent, "mode": cfg.mode, "model": cfg.model, "prompt": cfg.prompt,
		}})
		if err != nil {
			return err
		}
		fmt.Printf("added agent %s to %s (repos: %s)\n", id, rest[0], orNone(strings.Join(repos, ", ")))
		startCreated(id)
		return nil
	case "create":
		// amux workgroup create <repo>... [--name n] [--prompt t] [--mode m] [--model M]
		// Creates a workgroup plus one default agent scoped to the given repos.
		repos, cfg := parseCreateFlags(rest)
		rootID, err := sendActionID(core.Action{Action: core.ActionCreateWorkspace, Fields: createWorkspaceFields(repos, cfg)})
		if err != nil {
			return err
		}
		fmt.Printf("created workgroup %s (agent repos: %s)\n", rootID, orNone(strings.Join(repos, ", ")))
		startCreated(rootID)
		return nil
	case "repo":
		if len(rest) == 0 {
			return fmt.Errorf("usage: amux new --repo <repo> \"<task>\" (was: amux workgroup repo <repo>)")
		}
		_, cfg := parseCreateFlags(rest[1:])
		id, err := sendActionID(core.Action{Action: core.ActionCreateWorkspace, Fields: createWorkspaceFields([]string{rest[0]}, cfg)})
		if err != nil {
			return err
		}
		fmt.Printf("created workgroup %s limited to repo %s (with one agent on it)\n", id, rest[0])
		startCreated(id)
		return nil
	}
	// The renamed verbs (ls, open, archive, rename, rm, move, ...): exactly the
	// handlers their new spellings run.
	if out, _ := rewriteLegacy(append([]string{"workgroup"}, args...)); out[0] != "workgroup" {
		if c := lookupCommand(out[0]); c != nil {
			return c.run(out[1:])
		}
	}
	return usagef("unknown workgroup subcommand %q", sub)
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
		return fmt.Errorf("no workgroup or agent %q (see `amux ls`)", id)
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

// grantsAction maps `amux repo grant <id> [--all | --none | repo...]` to
// the host-only coordinator-set-repos action, plus the confirmation label.
func grantsAction(id string, args []string) (core.Action, string, error) {
	fields := map[string]string{"repos": ""}
	label := "none"
	switch {
	case len(args) == 1 && args[0] == "--all":
		fields["all"], label = "1", "all tracked repos"
	case len(args) == 1 && args[0] == "--none", len(args) == 0:
	default:
		for _, arg := range args {
			if strings.HasPrefix(arg, "-") {
				return core.Action{}, "", fmt.Errorf("usage: amux repo grant <coordinator-id> [--all | --none | repo...]")
			}
		}
		fields["repos"], label = strings.Join(args, ","), strings.Join(args, ", ")
	}
	return core.Action{Action: core.ActionCoordinatorSetRepos, ID: id, Fields: fields}, label, nil
}

// repoLine renders one `amux repo ls` row. A coordinator's listing carries no
// sources but marks whether each tracked repo is granted to its workgroup.
func repoLine(r core.RepoRow) string {
	if r.Granted == nil {
		return fmt.Sprintf("%-24s %s", r.Name, r.Source)
	}
	mark := "granted"
	if !*r.Granted {
		mark = "not granted (host: amux repo grant)"
	}
	return fmt.Sprintf("%-24s %s", r.Name, mark)
}

// grantsTag renders a workgroup's repository ceiling for `workgroup ls`, so a
// workgroup limited to one repo reads as such ("· repos: api") next to one
// granted several or none. A legacy coordinator with no ceiling yet shows none.
func grantsTag(r core.WorkgroupRow) string {
	if !r.Granted {
		return ""
	}
	if r.GrantsAll {
		return " · repos: all tracked"
	}
	if len(r.Grants) == 0 {
		return " · repos: (none)"
	}
	return " · repos: " + strings.Join(r.Grants, ", ")
}

// workgroupForm is the interactive creation page. Coordinator settings are
// separate from member defaults; repositories optionally seed a first member.
type workgroupForm struct {
	name, task  string
	coordinator wsops.Coordinator
	repos       []string
}

func (f workgroupForm) menu() []string {
	return []string{
		fmt.Sprintf("Name › %s", orOptional(f.name)),
		fmt.Sprintf("Task › %s", orOptional(f.task)),
		fmt.Sprintf("Coordinator harness › %s", f.coordinator.Kind),
		fmt.Sprintf("Coordinator model › %s", orDefault(f.coordinator.Model)),
		fmt.Sprintf("Repos (optional) › %s", orNone(strings.Join(f.repos, ", "))),
		"────────────────", "✓ Create workgroup", "✗ Cancel",
	}
}

func (f workgroupForm) action() core.Action {
	fields := map[string]string{
		"name": f.name, "prompt": f.task,
		wsops.FieldCoordinator:      f.coordinator.Kind,
		wsops.FieldCoordinatorModel: f.coordinator.Model,
	}
	// Omission preserves the host's default grants and creates only the
	// coordinator. Selected repos request a first member on its default harness.
	if len(f.repos) > 0 {
		fields["repos"] = strings.Join(f.repos, ",")
	}
	return core.Action{Action: core.ActionNewWorkgroup, Fields: fields}
}

func (f *workgroupForm) cycleCoordinator() {
	f.coordinator.Kind = pickCoordinatorKind(f.coordinator.Kind)
	f.coordinator.Model = defaultModelFor(f.coordinator.Kind)
}

// sessionNew names the workgroup, configures its coordinator, and optionally
// selects repositories. Further members can be configured after creation.
func sessionNew(ctx context.Context, seedRepos []string) error {
	if err := requireTerminal("amux new"); err != nil {
		return err
	}
	in := bufio.NewReader(os.Stdin)
	f := workgroupForm{
		coordinator: wsops.Coordinator{Kind: agent.GoalRuntime, Model: defaultModelFor(agent.GoalRuntime)},
		repos:       seedRepos,
	}
	for {
		choice, err := fzfMenu("new workgroup", f.menu())
		if err != nil {
			return nil // Esc
		}
		switch {
		case strings.HasPrefix(choice, "Name"):
			f.name = promptLine(in, "Workgroup name (optional)")
		case strings.HasPrefix(choice, "Task"):
			f.task = promptLine(in, "Task for the coordinator (optional)")
		case strings.HasPrefix(choice, "Coordinator harness"):
			f.cycleCoordinator()
		case strings.HasPrefix(choice, "Coordinator model"):
			f.coordinator.Model = pickCoordinatorModel(in, f.coordinator.Kind, f.coordinator.Model)
		case strings.HasPrefix(choice, "Repos"):
			f.repos = pickRepos(ctx, in, "repos for the first agent (optional, TAB)")
		case strings.HasPrefix(choice, "✓"):
			rootID, err := sendActionID(f.action())
			if err != nil {
				return err
			}
			startCreated(rootID)
			fmt.Printf("created workgroup %s — it's starting; open it in the dashboard (`amux`)\n", rootID)
			return nil
		case strings.HasPrefix(choice, "✗"):
			return nil
		}
	}
}

func pickCoordinatorModel(in *bufio.Reader, kind, current string) string {
	if len(agent.HarnessFor(kind).Models()) == 0 {
		return promptLine(in, "Coordinator model (optional)")
	}
	return pickModel(kind, current)
}

func sessionAdd(ctx context.Context, rootID string) error {
	if err := requireTerminal("amux add (interactive)"); err != nil {
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

type createCfg struct{ name, prompt, mode, model, agent, coordinator, coordinatorModel string }

func createWorkspaceFields(repos []string, cfg createCfg) map[string]string {
	fields := map[string]string{
		"name": cfg.name, "agent": cfg.agent, "mode": cfg.mode,
		"model": cfg.model, "prompt": cfg.prompt, "defaultAgent": "1",
	}
	// The coordinator runtime is chosen only when asked for: absent, the daemon
	// picks the goal runtime with its own default model (never the worker's).
	if cfg.coordinator != "" {
		fields[wsops.FieldCoordinator] = cfg.coordinator
	}
	if cfg.coordinatorModel != "" {
		fields[wsops.FieldCoordinatorModel] = cfg.coordinatorModel
	}
	if len(repos) > 0 {
		fields["repos"] = strings.Join(repos, ",")
	}
	return fields
}

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
		case (a == "--harness" || a == "--agent") && i+1 < len(args):
			cfg.agent = args[i+1]
			i++
		case strings.HasPrefix(a, "--harness="):
			cfg.agent = strings.TrimPrefix(a, "--harness=")
		case strings.HasPrefix(a, "--agent="):
			cfg.agent = strings.TrimPrefix(a, "--agent=")
		case a == "--coordinator" && i+1 < len(args):
			cfg.coordinator = args[i+1]
			i++
		case strings.HasPrefix(a, "--coordinator="):
			cfg.coordinator = strings.TrimPrefix(a, "--coordinator=")
		case a == "--coordinator-model" && i+1 < len(args):
			cfg.coordinatorModel = args[i+1]
			i++
		case strings.HasPrefix(a, "--coordinator-model="):
			cfg.coordinatorModel = strings.TrimPrefix(a, "--coordinator-model=")
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

// pickCoordinatorKind cycles the coordinator runtime on the interactive page.
// The default (the goal runtime) has native goal mode and Claude runs in goal
// mode through amux's hooks; the page shows what is actually chosen.
func pickCoordinatorKind(current string) string {
	kinds := agent.Kinds()
	now := wsops.CoordinatorKind(current)
	for i, k := range kinds {
		if k == now {
			return kinds[(i+1)%len(kinds)]
		}
	}
	return wsops.CoordinatorKind("")
}

// idAndForce parses `<id> [--force|-f]` (flag on either side) for the archive
// and rm verbs. --force skips the open pull-request guard; access policy still
// decides whether the caller may send it.
func idAndForce(args []string) (id string, force bool, err error) {
	for _, a := range args {
		switch a {
		case "--force", "-f", "-force":
			force = true
		default:
			if id != "" || strings.HasPrefix(a, "-") {
				return "", false, fmt.Errorf("unexpected argument %q", a)
			}
			id = a
		}
	}
	if id == "" {
		return "", false, fmt.Errorf("missing id")
	}
	return id, force, nil
}
