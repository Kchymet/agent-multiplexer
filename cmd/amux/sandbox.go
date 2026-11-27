package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"amux/internal/agent"
	"amux/internal/cfghome"
	"amux/internal/console"
	"amux/internal/overseer"
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
		return showHelp("harness sandbox")
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
		return usagef("unknown sandbox subcommand %q", sub)
	}
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
	if id == "" || id == overseer.ID {
		sessions = append(sessions, overseer.Session())
	}
	if id != console.ID && id != overseer.ID {
		roots, err := querySessions()
		if err != nil {
			return nil, fmt.Errorf("%v\n  the daemon resolves agents; start it with `amux` or `amux daemon start`", err)
		}
		for _, wg := range roots {
			// A workgroup's coordinator and a repo's home have private config homes
			// of their own (their sandbox is the container dir).
			if wg.Role != "" && wg.Dir != "" && (id == "" || wg.ID == id) {
				sessions = append(sessions, store.Session{ID: wg.ID, Agent: wg.Agent, Scope: wg.Scope, Dir: wg.Dir})
			}
			for _, a := range wg.Agents {
				if id != "" && a.ID != id {
					continue
				}
				sessions = append(sessions, store.Session{ID: a.ID, RootID: wg.ID, Agent: a.Agent, Dir: a.Dir})
			}
		}
	}
	if id != "" && len(sessions) == 0 {
		return nil, fmt.Errorf("no such agent %q\n  `amux ls` lists them", id)
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
	type ref struct {
		id   string
		spec cfghome.Spec
	}
	var refs []ref
	var specs []cfghome.Spec
	for _, sa := range agents {
		for _, spec := range sa.specs {
			refs = append(refs, ref{sa.s.ID, spec})
			specs = append(specs, spec)
		}
	}
	progress := driftProgress(asJSON, os.Stderr, isTerminal(os.Stderr))
	results := cfghome.ScanAll(specs, cfghome.ScanOptions{Progress: progress})
	var rows []driftRow
	for i, r := range results {
		if r.Err != nil {
			fmt.Fprintf(os.Stderr, "amux harness sandbox: %s (%s): %v\n", refs[i].id, refs[i].spec.Kind, r.Err)
			continue
		}
		if len(r.Changes) == 0 && id == "" {
			continue
		}
		rows = append(rows, driftRow{Agent: refs[i].id, Kind: refs[i].spec.Kind, Copy: refs[i].spec.Dir, Changes: r.Changes})
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
	fmt.Printf("\npropagate:  amux harness sandbox promote <id> <path>    discard:  amux harness sandbox reset <id> <path>\n")
	return nil
}

// sandboxApply runs promote/reset for one agent's path, against whichever of the
// agent's harness copies covers it.
func sandboxApply(args []string, verb string, apply func(cfghome.Spec, string) error) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: amux harness sandbox %s <id> <path>", verb)
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
		return fmt.Errorf("usage: amux harness sandbox path <id>")
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

// sandboxDriftSummary counts agent/path differences awaiting a decision, not
// editing actions. Query or scan failures must not be reported as a clean check.
// Archived agents are included on purpose: archiving keeps the copy and its
// manifest, `amux harness sandbox promote/reset <id>` still resolves the archived id,
// and the agent can be restored — their edits stay actionable until the agent
// is deleted.
func sandboxDriftSummary(progress func(done, total int)) (agents, edits int, err error) {
	all, err := sandboxAgents("")
	if err != nil {
		return 0, 0, err
	}
	var specs []cfghome.Spec
	for _, sa := range all {
		// Each harness's spec carries the session id as AgentID, which is how
		// DriftSummary labels errors and groups an agent's specs.
		specs = append(specs, sa.specs...)
	}
	return cfghome.DriftSummary(specs, cfghome.ScanOptions{Progress: progress})
}

// driftProgress is `amux harness sandbox drift`'s progress on stderr: none with --json,
// whose consumers read a clean machine-readable stream.
func driftProgress(asJSON bool, w io.Writer, tty bool) func(done, total int) {
	if asJSON {
		return nil
	}
	return scanProgress(w, tty, "scanning config copies")
}

// scanProgress returns a Progress callback so a slow comparison over many
// agents is visibly working rather than hung. On a terminal it keeps one
// "label N/M" status line updated and erases it when the last scan is done;
// elsewhere (a log, a wrapper capturing output) it writes one plain line when
// scanning starts and no escape codes.
func scanProgress(w io.Writer, tty bool, label string) func(done, total int) {
	if !tty {
		return func(done, total int) {
			if done == 0 {
				fmt.Fprintf(w, "  … %s (%d)\n", label, total)
			}
		}
	}
	return func(done, total int) {
		if done >= total {
			fmt.Fprint(w, "\r\033[K")
			return
		}
		fmt.Fprintf(w, "\r\033[K  … %s %d/%d", label, done, total)
	}
}

// plural is the "s" suffix for a count other than one.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
