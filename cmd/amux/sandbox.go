package main

import (
	"encoding/json"
	"fmt"
	"os"

	"amux/internal/agent"
	"amux/internal/cfghome"
	"amux/internal/console"
	"amux/internal/store"
)

// cmdSandbox is the user's side of the templated-config loop. Every agent runs
// its harness (Claude Code, Codex) against a PRIVATE COPY of the user's config
// dir, seeded from that dir as a template; the agent may edit its copy. amux
// compares each copy with the template and reports what diverged, so a change an
// agent made to its own configuration is a visible decision for the user —
// propagate it into the template (promote), or discard it (reset) — instead of
// something that either silently leaked into ~/.claude or silently died with the
// agent. The daemon flags pending edits on the rail; this is where they are
// listed and acted on.
func cmdSandbox(args []string) error {
	sub := "drift"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	if isHelpFlag(sub) {
		sandboxUsage()
		return nil
	}
	switch sub {
	case "drift", "changes", "ls", "list":
		return sandboxDrift(args)
	case "promote":
		return sandboxApply(args, "promote", cfghome.Promote)
	case "reset":
		return sandboxApply(args, "reset", cfghome.Reset)
	case "path":
		return sandboxPath(args)
	default:
		sandboxUsage()
		return fmt.Errorf("unknown sandbox subcommand %q", sub)
	}
}

func sandboxUsage() {
	fmt.Fprint(os.Stderr, `amux sandbox — each agent's private copy of your harness configuration

Every agent runs Claude Code / Codex against a private copy of your own config
dir (~/.claude, $CODEX_HOME), seeded from it as a template and pointed at via
CLAUDE_CONFIG_DIR / CODEX_HOME. The agent may edit its copy. amux compares each
copy with the template and reports what diverged, so you decide whether an
agent's change should propagate to your config (and so to every new agent) or
be discarded. Nothing propagates on its own. Credentials are shared, never copied.

usage: amux sandbox <command>

  drift [<id>] [--json]     list config paths that diverged from the template, per
                            agent (all agents when no id). Statuses:
                              agent-changed | agent-added | agent-removed
                                — the agent's edit; promote or reset it
                              template-changed — you changed the template since the
                                copy was seeded; reset pulls the new version in
                              conflict — both sides changed; pick with promote/reset
                              shared-detached — the agent's auth file became a private
                                copy; reset re-links it to yours
  promote <id> <path>       propagate the agent's version of <path> into your config
  reset <id> <path>         discard the agent's version; re-copy the template's
  path <id>                 print the agent's private config dir(s) and env

<id> is the agent's store id (from amux workgroup ls), or "console".
`)
}

// sandboxAgent is one agent whose config copies the CLI can inspect.
type sandboxAgent struct {
	s     store.Session
	specs []cfghome.Spec
}

// sandboxAgents resolves the agents to inspect — one by id, or every tracked
// agent plus the console — through the daemon's session model (the daemon owns
// the store), building the minimal session each harness's Config needs.
func sandboxAgents(id string) ([]sandboxAgent, error) {
	var sessions []store.Session
	if id == "" || id == console.ID {
		sessions = append(sessions, console.Session())
	}
	if id != console.ID {
		roots, err := querySessions()
		if err != nil {
			return nil, fmt.Errorf("%v\n  the daemon resolves agents; start it with `amux` or `amux daemon start`", err)
		}
		for _, wg := range roots {
			for _, a := range wg.Agents {
				if id != "" && a.ID != id {
					continue
				}
				sessions = append(sessions, store.Session{ID: a.ID, RootID: wg.ID, Agent: a.Agent, Dir: a.Dir})
			}
		}
	}
	if id != "" && len(sessions) == 0 {
		return nil, fmt.Errorf("no such agent %q\n  `amux workgroup ls` lists them", id)
	}
	var out []sandboxAgent
	for _, s := range sessions {
		sa := sandboxAgent{s: s}
		if spec, ok := agent.HarnessFor(s.Agent).Config(s); ok {
			sa.specs = append(sa.specs, spec)
		}
		out = append(out, sa)
	}
	return out, nil
}

// driftRow is one agent's drift for --json.
type driftRow struct {
	Agent   string           `json:"agent"`
	Kind    string           `json:"kind"`
	Copy    string           `json:"copy"`
	Changes []cfghome.Change `json:"changes"`
}

func sandboxDrift(args []string) error {
	asJSON := false
	id := ""
	for _, a := range args {
		if a == "--json" {
			asJSON = true
		} else if id == "" {
			id = a
		}
	}
	agents, err := sandboxAgents(id)
	if err != nil {
		return err
	}
	var rows []driftRow
	for _, sa := range agents {
		for _, spec := range sa.specs {
			changes, err := cfghome.Scan(spec)
			if err != nil {
				fmt.Fprintf(os.Stderr, "amux sandbox: %s (%s): %v\n", sa.s.ID, spec.Kind, err)
				continue
			}
			if len(changes) == 0 && id == "" {
				continue
			}
			rows = append(rows, driftRow{Agent: sa.s.ID, Kind: spec.Kind, Copy: spec.Dir, Changes: changes})
		}
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if rows == nil {
			rows = []driftRow{}
		}
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Println("no config drift: every agent's config copy matches its template")
		return nil
	}
	for _, r := range rows {
		fmt.Printf("%s  %s  %s\n", r.Agent, r.Kind, r.Copy)
		if len(r.Changes) == 0 {
			fmt.Println("    (matches the template)")
			continue
		}
		for _, c := range r.Changes {
			fmt.Printf("    %s\n", c)
		}
	}
	fmt.Printf("\npropagate:  amux sandbox promote <id> <path>    discard:  amux sandbox reset <id> <path>\n")
	return nil
}

// sandboxApply runs promote/reset for one agent's path, against whichever of the
// agent's harness copies covers it.
func sandboxApply(args []string, verb string, apply func(cfghome.Spec, string) error) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: amux sandbox %s <id> <path>", verb)
	}
	id, rel := args[0], args[1]
	agents, err := sandboxAgents(id)
	if err != nil {
		return err
	}
	var lastErr error
	for _, sa := range agents {
		for _, spec := range sa.specs {
			if err := apply(spec, rel); err != nil {
				lastErr = err
				continue
			}
			cfghome.Invalidate(spec)
			fmt.Printf("%s: %s %s (%s)\n", verb, id, rel, spec.Kind)
			return nil
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("agent %s has no templated config", id)
}

func sandboxPath(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: amux sandbox path <id>")
	}
	agents, err := sandboxAgents(args[0])
	if err != nil {
		return err
	}
	for _, sa := range agents {
		if len(sa.specs) == 0 {
			fmt.Printf("%s: the %s harness has no templated config\n", sa.s.ID, agent.Canonical(sa.s.Agent))
		}
		for _, spec := range sa.specs {
			fmt.Printf("%s\n  template  %s\n  env       %s\n", spec.Dir, spec.Template, spec.EnvEntry())
		}
	}
	return nil
}

// sandboxDriftSummary is the doctor's one-line view: how many agents have edits
// awaiting a decision. Best-effort — an unreachable daemon yields nothing.
func sandboxDriftSummary() (agents, edits int) {
	all, err := sandboxAgents("")
	if err != nil {
		return 0, 0
	}
	for _, sa := range all {
		n := 0
		for _, spec := range sa.specs {
			changes, _ := cfghome.Scan(spec)
			n += cfghome.Pending(changes)
		}
		if n > 0 {
			agents++
			edits += n
		}
	}
	return agents, edits
}

// plural is the "s" suffix for a count other than one.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
