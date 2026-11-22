// Package buildinfo holds the version contracts shared by the amux CLI and
// daemon. Version may be replaced at link time; the protocol number changes
// only when a CLI and daemon can no longer safely speak the same wire format.
package buildinfo

import (
	"regexp"
	"runtime/debug"
)

// devFallback is the product version of a binary linked without an explicit
// version (plain `go build`, `go test`). It is a semver prerelease so it sorts
// below every release and is recognized as a development build.
const devFallback = "0.0.0-dev"

// Version is the product version of this binary. `make build` links
// 0.1.<commit count>-dev+<sha>[.dirty]; the release workflow links the plain
// 0.1.<commit count> it publishes as tag v0.1.<commit count>
// (scripts/version.sh derives both from git).
var Version = devFallback

// Commit is the full Git revision the binary was built from, when known.
// Builds through the Makefile link it; otherwise it is read from the Go
// toolchain's VCS stamp when present.
var Commit = ""

// DaemonProtocol is the local CLI-to-daemon API contract. Additive changes may
// keep the same number; bump it when either side must reject the other.
const DaemonProtocol = 2

func init() {
	if Version != devFallback && Commit != "" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if Commit == "" {
		Commit = rev
	}
	if Version == devFallback && len(rev) >= 12 {
		Version = devFallback + "+" + rev[:12]
		if dirty {
			Version += ".dirty"
		}
	}
}

var releasePattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// IsRelease reports whether v is a published release version: a bare
// MAJOR.MINOR.PATCH with no prerelease or build suffix. Everything else
// (checkout builds, plain go build, custom labels) is a development build that
// self-update never replaces automatically.
func IsRelease(v string) bool { return releasePattern.MatchString(v) }

// Release reports whether this binary is a published release build.
func Release() bool { return IsRelease(Version) }
