package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/store"
	"amux/internal/wsops"
)

// This file is the everyday verbs (new, ls, open, send, add, archive) and the
// less frequent ones next to them (rename, unarchive, rm, move, recreate). Each
// is a thin wrapper over one daemon control action — the same action `amux do`
// sends — so the verbs, the dashboard and scripts share one code path.

// verbFlags is a small hand-rolled parser for the verbs: --name value,
// --name=value and repeatable flags, operands anywhere. Booleans take no value.
type verbFlags struct {
	values   map[string][]string
	bools    map[string]bool
	operands []string
}

// parseVerbFlags parses args against the value flags and boolean flags named.
// aliases maps an old flag spelling to its current name (--agent: --harness).
func parseVerbFlags(args []string, value, boolean []string, aliases map[string]string) (verbFlags, error) {
	f := verbFlags{values: map[string][]string{}, bools: map[string]bool{}}
	isValue := map[string]bool{}
	for _, v := range value {
		isValue[v] = true
	}
	isBool := map[string]bool{}
	for _, b := range boolean {
		isBool[b] = true
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			f.operands = append(f.operands, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			f.operands = append(f.operands, a)
			continue
		}
		name, inline, hasInline := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if to, ok := aliases[name]; ok {
			name = to
		}
		switch {
		case isBool[name] && !hasInline:
			f.bools[name] = true
		case isValue[name]:
			v := inline
			if !hasInline {
				if i+1 >= len(args) {
					return f, usagef("flag --%s needs a value", name)
				}
				i++
				v = args[i]
			}
			f.values[name] = append(f.values[name], v)
		default:
			return f, usagef("unknown flag %q", a)
		}
	}
	return f, nil
}

func (f verbFlags) last(name string) string {
	if v := f.values[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

// list is a repeatable flag's values, comma lists split.
func (f verbFlags) list(name string) []string {
	var out []string
	for _, v := range f.values[name] {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// startCreatedFromHost starts a session a host user just created, as the
// dashboard does. From an agent session the creation is left exactly as the
// matching amux do action leaves it (the coordinator guide starts members
// explicitly), so amux new/add and amux do behave the same for agents.
func startCreatedFromHost(id string) {
	if !sessionContextRestricted() {
		startCreated(id)
	}
}

// memberMode is the mode a new member gets: members are tasks, --interactive
// the advanced override. Coordinators and repo homes are interactive by role.
func memberMode(interactive bool) string {
	if interactive {
		return store.ModeInteractive
	}
	return store.ModeTask
}

// ---- amux new --------------------------------------------------------------

var harnessFlagAlias = map[string]string{"agent": "harness"}

func cmdNew(args []string) error {
	f, err := parseVerbFlags(args,
		[]string{"repo", "harness", "model", "coordinator", "coordinator-model", "name"},
		[]string{"interactive"}, harnessFlagAlias)
	if err != nil {
		return err
	}
	task := strings.TrimSpace(strings.Join(f.operands, " "))
	repos := f.list("repo")
	if len(args) == 0 || (task == "" && len(f.values) == 0 && len(f.bools) == 0) {
		return sessionNew(context.Background(), nil)
	}
	a := newWorkgroupAction(task, repos, f)
	rootID, err := sendActionID(a)
	if err != nil {
		return err
	}
	switch {
	case len(repos) > 0:
		fmt.Printf("created workgroup %s limited to %s\n", rootID, strings.Join(repos, ", "))
	default:
		fmt.Printf("created workgroup %s\n", rootID)
	}
	startCreatedFromHost(rootID)
	return nil
}

// newWorkgroupAction is the new-workgroup action amux new sends. The task is
// the coordinator's; repos limit the workgroup and seed a first member, whose
// harness/model/mode the member flags set. Unset fields are left out so the
// daemon's defaults (and its coordinator default) apply.
func newWorkgroupAction(task string, repos []string, f verbFlags) core.Action {
	fields := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			fields[k] = v
		}
	}
	set("prompt", task)
	set("name", f.last("name"))
	set("repos", strings.Join(repos, ","))
	if len(repos) > 0 {
		harness := f.last("harness")
		if harness == "" {
			harness = agent.DefaultKind()
		}
		set("agent", harness)
		set("model", f.last("model"))
		set("mode", memberMode(f.bools["interactive"]))
	}
	set(wsops.FieldCoordinator, f.last("coordinator"))
	set(wsops.FieldCoordinatorModel, f.last("coordinator-model"))
	return core.Action{Action: core.ActionNewWorkgroup, Fields: fields}
}

// ---- amux ls -----------------------------------------------------------------

// cmdLs lists workgroups and their agents (it is what amux status was).
func cmdLs(args []string) error { return cmdStatus(args) }

// renderLs prints sessions as workgroups with their agents indented beneath:
// id, name, harness, state and PR/review chips. Archived rows are left out
// unless all is set.
// grants maps a workgroup id to its repo-ceiling tag (" · repos: api").
func renderLs(w io.Writer, sessions []core.Session, all bool, grants map[string]string) {
	children := map[string][]core.Session{}
	known := map[string]bool{}
	for _, s := range sessions {
		known[s.ID] = true
	}
	var roots []core.Session
	for _, s := range sessions {
		if !all && s.Archived {
			continue
		}
		if s.RootID != "" && s.RootID != s.ID && known[s.RootID] {
			children[s.RootID] = append(children[s.RootID], s)
			continue
		}
		roots = append(roots, s)
	}
	state := func(s core.Session) string {
		st := s.State
		if st == "" || st == core.StateIdle {
			st = core.StateStopped
		}
		if s.Archived {
			st += " [archived]"
		}
		return st
	}
	for _, r := range roots {
		fmt.Fprintf(w, "%-8s %-28s %-7s %-8s%s\n", r.ID, lsTitle(r), agent.Canonical(r.Kind), state(r), statusEnrichment(r)+grants[r.ID])
		for _, c := range children[r.ID] {
			fmt.Fprintf(w, "  %-8s %-26s %-7s %-8s%s\n", c.ID, lsTitle(c), agent.Canonical(c.Kind), state(c), statusEnrichment(c))
		}
	}
}

// workgroupGrants is each workgroup's repo-ceiling tag for amux ls, best
// effort: a daemon that cannot answer leaves the tags off.
func workgroupGrants() map[string]string {
	rows, err := querySessions()
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = grantsTag(r)
	}
	return out
}

func lsTitle(s core.Session) string {
	t := strings.TrimSpace(s.Title)
	if t == "" {
		t = s.ID
	}
	if r := []rune(t); len(r) > 26 {
		t = string(r[:25]) + "…"
	}
	return t
}

// ---- amux open / send / add ------------------------------------------------------

func cmdOpen(args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return usageErr("open")
	}
	return sessionOpen(args[0])
}

// cmdSend is amux send: the steer action's prompt, interject or stop verb —
// the daemon delivers it through the shared typed-delivery path
// (docs/steering.md), queueing a prompt while the agent is busy.
func cmdSend(args []string) error {
	a, err := steerForSend(args, os.Stdin)
	if err != nil {
		return err
	}
	if err := sendAction(a); err != nil {
		// A busy composer (a draft in the box, an open dialog) queues the text
		// for automatic delivery: for amux send that is success.
		if strings.Contains(err.Error(), steerQueuedPhrase) {
			fmt.Printf("queued for %s: it is delivered when the agent can take it (do not resend)\n", a.ID)
			return nil
		}
		return err
	}
	switch a.Fields[core.SteerVerb] {
	case core.SteerStop:
		fmt.Printf("stopped %s's turn\n", a.ID)
	case core.SteerInterject:
		fmt.Printf("interjected to %s\n", a.ID)
	default:
		fmt.Printf("sent to %s\n", a.ID)
	}
	return nil
}

// steerQueuedPhrase is how the daemon says a busy agent's steer was queued
// (internal/daemon composerBusyError); TestSteerQueuedPhraseMatchesDaemon pins it.
const steerQueuedPhrase = "the message is queued"

// steerForSend builds amux send's steer action. Text "-" is read from stdin.
func steerForSend(args []string, stdin io.Reader) (core.Action, error) {
	f, err := parseVerbFlags(args, nil, []string{"now", "stop"}, nil)
	if err != nil {
		return core.Action{}, err
	}
	if len(f.operands) == 0 {
		return core.Action{}, usageErr("send")
	}
	id, words := f.operands[0], f.operands[1:]
	text := strings.Join(words, " ")
	if text == "-" {
		b, err := io.ReadAll(bufio.NewReader(stdin))
		if err != nil {
			return core.Action{}, err
		}
		text = strings.TrimRight(string(b), "\n")
	}
	verb := core.SteerPrompt
	switch {
	case f.bools["now"] && f.bools["stop"]:
		return core.Action{}, usagef("choose --now or --stop, not both")
	case f.bools["stop"]:
		if strings.TrimSpace(text) != "" {
			return core.Action{}, usagef("--stop takes no text (send it after the turn stops)")
		}
		verb = core.SteerStop
	case f.bools["now"]:
		verb = core.SteerInterject
	}
	if verb != core.SteerStop && strings.TrimSpace(text) == "" {
		return core.Action{}, usagef("usage: %s (the text is missing)", synopsis(mustHelpNode("send")))
	}
	fields := map[string]string{core.SteerVerb: verb}
	if verb != core.SteerStop {
		fields[core.SteerText] = text
	}
	return core.Action{Action: core.ActionSteer, ID: id, Fields: fields}, nil
}

func cmdAdd(args []string) error {
	f, err := parseVerbFlags(args, []string{"repo", "harness", "model"}, []string{"interactive"}, harnessFlagAlias)
	if err != nil {
		return err
	}
	if len(f.operands) == 0 {
		return usageErr("add")
	}
	root := f.operands[0]
	if len(args) == 1 {
		return sessionAdd(context.Background(), root) // interactive
	}
	a := addAgentAction(root, strings.TrimSpace(strings.Join(f.operands[1:], " ")), f)
	id, err := sendActionID(a)
	if err != nil {
		return err
	}
	fmt.Printf("added agent %s to %s (repos: %s)\n", id, root, orNone(strings.ReplaceAll(a.Fields["repos"], ",", ", ")))
	startCreatedFromHost(id)
	return nil
}

// addAgentAction is the add-agent action amux add sends.
func addAgentAction(root, task string, f verbFlags) core.Action {
	harness := f.last("harness")
	if harness == "" {
		harness = agent.DefaultKind()
	}
	// A host user gets their preferred model pre-filled, as the form does; an
	// agent leaves it to the daemon, as amux do add-agent does.
	model := f.last("model")
	if model == "" && !sessionContextRestricted() {
		model = defaultModelFor(harness)
	}
	return core.Action{Action: core.ActionAddAgent, ID: root, Fields: map[string]string{
		"repos": strings.Join(f.list("repo"), ","), "agent": harness, "model": model,
		"mode": memberMode(f.bools["interactive"]), "prompt": task,
	}}
}

// ---- archive and the rarer per-session verbs ---------------------------------------

func cmdArchive(args []string) error {
	id, force, err := idAndForce(args)
	if err != nil {
		return usageErr("archive")
	}
	fields := map[string]string{"archived": "true"}
	if force {
		fields[wsops.FieldForce] = "true"
	}
	if err := sendAction(core.Action{Action: core.ActionSetArchived, ID: id, Fields: fields}); err != nil {
		return err
	}
	fmt.Printf("archived %s\n", id)
	return nil
}

func cmdUnarchive(args []string) error {
	if len(args) != 1 {
		return usageErr("unarchive")
	}
	if err := sendAction(core.Action{Action: core.ActionSetArchived, ID: args[0], Fields: map[string]string{"archived": "false"}}); err != nil {
		return err
	}
	fmt.Printf("restored %s\n", args[0])
	return nil
}

func cmdRename(args []string) error {
	if len(args) < 2 {
		return usageErr("rename")
	}
	return sendAction(core.Action{Action: core.ActionRename, ID: args[0], Fields: map[string]string{"name": strings.Join(args[1:], " ")}})
}

func cmdRm(args []string) error {
	id, force, err := idAndForce(args)
	if err != nil {
		return usageErr("rm")
	}
	a := core.Action{Action: core.ActionDelete, ID: id}
	if force {
		a.Fields = map[string]string{wsops.FieldForce: "true"}
	}
	if err := sendAction(a); err != nil {
		return err
	}
	fmt.Printf("removed %s\n", id)
	return nil
}

// cmdMove moves an agent into an existing workgroup, or (no target / --new)
// into a new workgroup of its own, granted exactly the agent's repos.
func cmdMove(args []string) error {
	if len(args) < 1 || len(args) > 2 || strings.HasPrefix(args[0], "-") {
		return usageErr("move")
	}
	agentID, target := args[0], ""
	if len(args) == 2 && args[1] != "--new" {
		target = args[1]
	}
	newID, err := sendActionID(core.Action{Action: core.ActionMove, ID: agentID, Target: target})
	if err != nil {
		return err
	}
	switch {
	case target == "" && newID != "":
		fmt.Printf("moved agent %s into new workgroup %s\n", agentID, newID)
	case target == "":
		fmt.Printf("moved agent %s into a new workgroup\n", agentID)
	default:
		fmt.Printf("moved agent %s into workgroup %s\n", agentID, target)
	}
	return nil
}

func cmdRecreate(args []string) error {
	if len(args) != 1 {
		return usageErr("recreate")
	}
	if sessionContextRestricted() {
		return fmt.Errorf("runtime recreation requires authenticated host control")
	}
	c, err := dial()
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.RecreateSession(args[0]); err != nil {
		return err
	}
	fmt.Printf("recreated runtime %s with current session access mounts\n", args[0])
	return nil
}

// ---- device and cloud --------------------------------------------------------------

// deviceSubcommands is amux device's dispatch: the direct server's handlers,
// re-registered under the device names (amux direct ... rewrites here).
var deviceSubcommands = map[string]func([]string) error{
	"connect":        cmdDirectEnroll,
	"ls":             func(a []string) error { return cmdDirectClients(append([]string{"list"}, a...)) },
	"list":           func(a []string) error { return cmdDirectClients(append([]string{"list"}, a...)) },
	"revoke":         func(a []string) error { return cmdDirectClients(append([]string{"revoke"}, a...)) },
	"status":         cmdDirectStatus,
	"enable":         cmdDirectEnable,
	"disable":        cmdDirectDisable,
	"logs":           cmdDirectLogs,
	"audit":          cmdDirectAudit,
	"serve":          cmdDirectServe,
	"reset-identity": cmdDirectResetIdentity,
}

func cmdDevice(args []string) error {
	if len(args) == 0 || isHelpFlag(args[0]) {
		return showHelp("device")
	}
	run, ok := deviceSubcommands[args[0]]
	if !ok {
		return usagef("unknown device command %q", args[0])
	}
	// The device listener and its administration act with host authority. A
	// restricted agent session must never start or administer it, whatever
	// credential files it can name.
	if sessionContextRestricted() {
		return errors.New("amux device is host-only and is not available inside an agent session")
	}
	return run(args[1:])
}

// cloudSubcommands is amux cloud's dispatch: provider mode (was amux provide)
// plus the cloud-assisted device policies (were amux direct cloud / wake).
var cloudSubcommands = map[string]func([]string) error{
	"connect":    cmdProvideInstall,
	"status":     cmdCloudStatus,
	"disconnect": cmdProvideUninstall,
	"run":        provideRun,
	"revoke":     cmdProvideRevoke,
	"enroll":     hostOnly("amux cloud enroll", cmdDirectCloud),
	"wake":       hostOnly("amux cloud wake", cmdDirectWake),
}

func cmdCloud(args []string) error {
	if len(args) == 0 || isHelpFlag(args[0]) {
		return showHelp("cloud")
	}
	run, ok := cloudSubcommands[args[0]]
	if !ok {
		return usagef("unknown cloud command %q", args[0])
	}
	return run(args[1:])
}

// hostOnly refuses run inside an agent session.
func hostOnly(what string, run func([]string) error) func([]string) error {
	return func(args []string) error {
		if sessionContextRestricted() {
			return fmt.Errorf("%s is host-only and is not available inside an agent session", what)
		}
		return run(args)
	}
}

// cmdCloudStatus prints the cloud connection's state: doctor's provider section.
func cmdCloudStatus(args []string) error {
	if len(args) != 0 {
		return usagef("unexpected argument %q", args[0])
	}
	fmt.Println("Harness cloud (provider mode)")
	reportProvider()
	return nil
}
