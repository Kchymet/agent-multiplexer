package main

import (
	"fmt"
	"strings"

	"amux/internal/buildinfo"
	"amux/internal/core"
	"amux/internal/daemon"
)

// versionReport separates "no daemon" from "a daemon too old to answer the
// version query", and preserves connection errors so diagnostics can distinguish
// expected offline states from denied or otherwise unknown daemon state.
type versionReport struct {
	CLI        string
	Connected  bool
	ConnectErr error
	Runtime    core.VersionInfo
	QueryErr   error
}

var versionDial = daemon.Dial
var collectVersionReport = collectVersions

func collectVersions() versionReport {
	r := versionReport{CLI: buildinfo.Version}
	c, err := versionDial()
	if err != nil {
		r.ConnectErr = err
		return r
	}
	defer c.Close()
	r.Connected = true
	r.Runtime, r.QueryErr = c.Version()
	return r
}

// versionQueryUnsupported recognizes the daemon's explicit legacy response.
// Transport failures, malformed replies, timeouts, EOF, and authorization
// rejection are not compatibility evidence and must not recommend a restart.
func versionQueryUnsupported(err error) bool {
	return err != nil && strings.TrimSpace(err.Error()) == fmt.Sprintf("unknown query %q", core.QueryVersion)
}

// versionStateError is non-nil when diagnostics could not establish runtime
// state. A missing/stale listener is an expected offline state, and an explicit
// legacy unsupported-query response proves a responsive older daemon.
func versionStateError(r versionReport) error {
	if !r.Connected {
		if r.ConnectErr != nil && !daemonMayStart(r.ConnectErr) {
			return r.ConnectErr
		}
		return nil
	}
	if r.QueryErr != nil && !versionQueryUnsupported(r.QueryErr) {
		return r.QueryErr
	}
	return nil
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
		if r.ConnectErr != nil && !daemonMayStart(r.ConnectErr) {
			detail := fmt.Sprintf("state unknown; connection failed (%v)", r.ConnectErr)
			if daemonAccessDenied(r.ConnectErr) {
				detail = fmt.Sprintf("state unknown; access denied (%v)", r.ConnectErr)
			}
			return append(lines,
				line("✗", "daemon", detail),
				line("·", "database", "unavailable (daemon state unknown)"),
			), false
		}
		return append(lines,
			line("·", "daemon", "offline"),
			line("·", "database", "unavailable (daemon offline)"),
		), false
	}
	if r.QueryErr != nil {
		if !versionQueryUnsupported(r.QueryErr) {
			return append(lines,
				line("✗", "daemon", fmt.Sprintf("state unknown; version query failed (%v)", r.QueryErr)),
				line("·", "database", "schema unavailable (daemon state unknown)"),
			), false
		}
		return append(lines,
			line("⚠", "daemon", fmt.Sprintf("running; version query unsupported (%v) — restart to load the current binary", r.QueryErr)),
			line("·", "database", "schema unavailable (legacy daemon)"),
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
	r := collectVersionReport()
	lines, _ := versionLines(r, false)
	for _, line := range lines {
		fmt.Println(line)
	}
	if err := versionStateError(r); err != nil {
		return fmt.Errorf("daemon state unknown: %w", err)
	}
	return nil
}
