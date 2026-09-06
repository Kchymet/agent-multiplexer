package main

import (
	"fmt"

	"amux/internal/buildinfo"
	"amux/internal/core"
	"amux/internal/daemon"
)

// versionReport separates "no daemon" from "a daemon too old to answer the
// version query". Both are normal diagnostic states, but the latter deserves a
// restart hint because it may be a stale binary left running after an upgrade.
type versionReport struct {
	CLI       string
	Connected bool
	Runtime   core.VersionInfo
	QueryErr  error
}

func collectVersions() versionReport {
	r := versionReport{CLI: buildinfo.Version}
	c, err := daemon.Dial()
	if err != nil {
		return r
	}
	defer c.Close()
	r.Connected = true
	r.Runtime, r.QueryErr = c.Version()
	return r
}

// versionLines renders the same facts for `amux version` and doctor. Doctor uses
// health markers and receives whether a known contract is incompatible; an
// offline or pre-version-query daemon is unknown, not falsely declared broken.
func versionLines(r versionReport, doctor bool) (lines []string, incompatible bool) {
	line := func(mark, component, detail string) string {
		if doctor {
			return fmt.Sprintf("  %s %-9s %s", mark, component, detail)
		}
		return fmt.Sprintf("%-9s %s", component, detail)
	}
	lines = append(lines, line("✓", "cli", fmt.Sprintf("%s (protocol %d)", r.CLI, buildinfo.DaemonProtocol)))
	if !r.Connected {
		return append(lines,
			line("·", "daemon", "offline"),
			line("·", "database", "unavailable (daemon offline)"),
		), false
	}
	if r.QueryErr != nil {
		return append(lines,
			line("⚠", "daemon", fmt.Sprintf("running; version unavailable (%v) — restart to load the current binary", r.QueryErr)),
			line("·", "database", "schema unavailable (daemon version query unsupported)"),
		), false
	}

	daemonOK := r.Runtime.DaemonProtocol == buildinfo.DaemonProtocol
	mark, state := "✓", "compatible with CLI"
	if !daemonOK {
		mark = "✗"
		state = fmt.Sprintf("incompatible with CLI protocol %d", buildinfo.DaemonProtocol)
		incompatible = true
	}
	lines = append(lines, line(mark, "daemon", fmt.Sprintf("%s (protocol %d; %s)",
		r.Runtime.DaemonVersion, r.Runtime.DaemonProtocol, state)))

	dbOK := r.Runtime.DatabaseError == "" &&
		r.Runtime.DatabaseSchema >= r.Runtime.DatabaseMinSchema &&
		r.Runtime.DatabaseSchema <= r.Runtime.DatabaseMaxSchema
	mark, state = "✓", fmt.Sprintf("compatible with daemon range %s",
		schemaRange(r.Runtime.DatabaseMinSchema, r.Runtime.DatabaseMaxSchema))
	if r.Runtime.DatabaseError != "" {
		mark, state = "✗", r.Runtime.DatabaseError
	} else if !dbOK {
		mark = "✗"
		state = fmt.Sprintf("incompatible with daemon range %s",
			schemaRange(r.Runtime.DatabaseMinSchema, r.Runtime.DatabaseMaxSchema))
	}
	if !dbOK {
		incompatible = true
	}
	lines = append(lines, line(mark, "database", fmt.Sprintf("schema %d (%s)", r.Runtime.DatabaseSchema, state)))
	return lines, incompatible
}

func schemaRange(min, max int) string {
	if min == max {
		return fmt.Sprintf("%d", min)
	}
	return fmt.Sprintf("%d–%d", min, max)
}

func cmdVersion() error {
	lines, _ := versionLines(collectVersions(), false)
	for _, line := range lines {
		fmt.Println(line)
	}
	return nil
}
