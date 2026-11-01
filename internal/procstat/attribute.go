package procstat

import (
	"path/filepath"
	"strings"
)

// claimKind says who a process belongs to.
type claimKind uint8

const (
	claimNone    claimKind = iota
	claimSession           // an amux sandbox path: a session (or workgroup-level) claim
	claimSelf              // an amux process outside any sandbox
)

// claim is a process's direct ownership, before ancestor inheritance.
type claim struct {
	kind      claimKind
	session   string // claimSession: session id
	workgroup string // claimSession: workgroup root id
	role      string // claimSelf: Role*
}

// attributor maps paths and argv to claims.
type attributor struct {
	sessionsMarker   string // "<SessionsDir>/"
	workspacesMarker string // "<WorkspacesDir>/"
	selfNames        map[string]bool
}

func newAttributor(sessionsDir, workspacesDir string, selfNames []string) attributor {
	a := attributor{selfNames: map[string]bool{}}
	if sessionsDir != "" {
		a.sessionsMarker = strings.TrimRight(filepath.ToSlash(sessionsDir), "/") + "/"
	}
	if workspacesDir != "" {
		a.workspacesMarker = strings.TrimRight(filepath.ToSlash(workspacesDir), "/") + "/"
	}
	for _, n := range selfNames {
		a.selfNames[n] = true
	}
	return a
}

// direct returns the process's own claim. A sandbox path in cwd wins over one
// in the cmdline; a sandbox claim wins over being an amux binary (an `amux
// agent ...` hook run by an agent is that agent's cost).
func (a attributor) direct(comm, cwd string, argv []string) claim {
	if c, ok := a.pathClaim(cwd); ok {
		return c
	}
	if len(argv) > 0 {
		if c, ok := a.pathClaim(strings.Join(argv, " ")); ok {
			return c
		}
	}
	if a.isSelf(comm, argv) {
		return claim{kind: claimSelf, role: selfRole(argv)}
	}
	return claim{}
}

func (a attributor) isSelf(comm string, argv []string) bool {
	if len(argv) > 0 {
		return a.selfNames[filepath.Base(argv[0])]
	}
	return a.selfNames[comm]
}

// pathClaim finds the earliest sandbox path in s.
func (a attributor) pathClaim(s string) (claim, bool) {
	if s == "" {
		return claim{}, false
	}
	si, wi := -1, -1
	if a.sessionsMarker != "" {
		si = strings.Index(s, a.sessionsMarker)
	}
	if a.workspacesMarker != "" {
		wi = strings.Index(s, a.workspacesMarker)
	}
	switch {
	case si >= 0 && (wi < 0 || si <= wi):
		rest := s[si+len(a.sessionsMarker):]
		wg, rest := segment(rest)
		if wg == "" {
			return claim{}, false
		}
		agent := ""
		if strings.HasPrefix(rest, "/") {
			agent, _ = segment(rest[1:])
		}
		if agent == "" || agent == "coordinator" {
			// The workgroup container itself or its coordinator sandbox: the
			// workgroup's own (root) session.
			agent = wg
		}
		return claim{kind: claimSession, session: agent, workgroup: wg}, true
	case wi >= 0:
		wg, _ := segment(s[wi+len(a.workspacesMarker):])
		if wg == "" {
			return claim{}, false
		}
		return claim{kind: claimSession, session: wg, workgroup: wg}, true
	}
	return claim{}, false
}

// segmentStops end a path segment inside a joined cmdline.
const segmentStops = "/ \t\n\"'=:,;"

func segment(s string) (seg, rest string) {
	i := strings.IndexAny(s, segmentStops)
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i:]
}

// selfRole classifies an amux invocation by its subcommand.
func selfRole(argv []string) string {
	args := argv
	if len(args) > 0 {
		args = args[1:]
	}
	// Skip leading flags (e.g. `amux --debug`).
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		args = args[1:]
	}
	if len(args) == 0 {
		return RoleTUI
	}
	next := ""
	if len(args) > 1 {
		next = args[1]
	}
	switch args[0] {
	case "daemon":
		if next == "" {
			return RoleDaemon
		}
	case "provide":
		if next != "install" && next != "uninstall" {
			return RoleProvide
		}
	case "serve":
		return RoleServe
	case "workgroup", "wg", "session", "ses", "workspace", "ws":
		if next == "open" {
			return RoleTUI
		}
	}
	return RoleCLI
}
