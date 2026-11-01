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
	// owners maps a stored session directory (cleaned, no trailing slash) to
	// its owner. It is authoritative: a member moved to another workgroup
	// keeps its sandbox under the old workgroup's directory, and a legacy
	// sub-session's directory is named after its repo and branch rather than
	// its id, so neither can be recovered from the path.
	owners map[string]claim
}

// withOwners returns a copy of a that consults owners before path parsing.
func (a attributor) withOwners(owners []Owner) attributor {
	a.owners = make(map[string]claim, len(owners))
	for _, o := range owners {
		if o.Dir == "" || o.Session == "" {
			continue
		}
		dir := filepath.Clean(filepath.ToSlash(o.Dir))
		if dir == "/" || dir == "." {
			continue
		}
		wg := o.Workgroup
		if wg == "" {
			wg = o.Session
		}
		a.owners[dir] = claim{kind: claimSession, session: o.Session, workgroup: wg}
	}
	return a
}

// ownerOf finds the deepest stored session directory containing path.
func (a attributor) ownerOf(path string) (claim, bool) {
	if len(a.owners) == 0 || !strings.HasPrefix(path, "/") {
		return claim{}, false
	}
	p := filepath.Clean(path)
	for p != "/" && p != "." {
		if c, ok := a.owners[p]; ok {
			return c, true
		}
		p = filepath.Dir(p)
	}
	return claim{}, false
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

// direct returns the process's own claim.
//
// Long-lived amux processes (daemon, provider, serve, the TUI) are always
// amux's own: they inherit whatever cwd they were started from, so a daemon
// restarted from inside an agent's worktree must not be charged to that agent
// (nor take its git/gh children with it). Any other amux invocation (an
// agent's `amux agent ...` hook, a status-line report) is charged to the
// sandbox it runs in, if any.
//
// Otherwise a sandbox path in cwd wins over one in the cmdline. The cmdline
// match is a substring match over every argument, so a process merely
// *naming* a sandbox path, e.g. `vim …/sessions/W/A/x.go` run from elsewhere,
// is attributed to that agent too; that is deliberate (bwrap names the
// sandbox only in its arguments) and cheap to be wrong about.
func (a attributor) direct(comm, cwd string, argv []string) claim {
	self := a.isSelf(comm, argv)
	role := ""
	if self {
		role = selfRole(argv)
		if role != RoleCLI {
			return claim{kind: claimSelf, role: role}
		}
	}
	if c, ok := a.pathClaim(cwd); ok {
		return c
	}
	if len(argv) > 0 {
		if c, ok := a.argvClaim(argv); ok {
			return c
		}
	}
	if self {
		return claim{kind: claimSelf, role: role}
	}
	return claim{}
}

// argvClaim looks for a sandbox path among the arguments: first any argument
// (or key=value / key:value value) under a stored session directory, then the
// path-pattern fallback over the joined cmdline.
func (a attributor) argvClaim(argv []string) (claim, bool) {
	if len(a.owners) > 0 {
		for _, arg := range argv {
			for _, cand := range pathCandidates(arg) {
				if c, ok := a.ownerOf(cand); ok {
					return c, true
				}
			}
		}
	}
	return a.patternClaim(strings.Join(argv, " "))
}

// pathCandidates extracts absolute paths from one argument: the argument
// itself, or what follows '=' or ':' (--chdir=/x, /a:/b).
func pathCandidates(arg string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(arg, func(r rune) bool { return r == '=' || r == ':' || r == ' ' || r == ',' }) {
		if strings.HasPrefix(f, "/") {
			out = append(out, f)
		}
	}
	return out
}

func (a attributor) isSelf(comm string, argv []string) bool {
	if len(argv) > 0 {
		return a.selfNames[filepath.Base(argv[0])]
	}
	return a.selfNames[comm]
}

// pathClaim attributes a single path (a cwd): stored ownership first, then
// the path pattern.
func (a attributor) pathClaim(path string) (claim, bool) {
	if c, ok := a.ownerOf(path); ok {
		return c, true
	}
	return a.patternClaim(path)
}

// patternClaim finds the earliest sandbox path in s and derives the owner
// from its shape. It is the fallback for directories the daemon has not
// reported (a session created since the last owner refresh, a test host).
func (a attributor) patternClaim(s string) (claim, bool) {
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
