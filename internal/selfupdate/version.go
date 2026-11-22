// Package selfupdate installs published amux releases over the running binary:
// it reads the latest release's manifest, downloads the asset for this
// platform, verifies it against the release's SHA256SUMS, and atomically
// renames it over the installed executable. It also holds the pure gating
// rules the daemon's automatic updater applies (see docs/versioning.md).
package selfupdate

import (
	"fmt"
	"strconv"
	"strings"
)

// Semver is a parsed semantic version. Build metadata (after '+') is kept for
// display but ignored by Compare, as semver specifies.
type Semver struct {
	Major, Minor, Patch uint64
	Pre                 []string
	Build               string
}

// ParseVersion accepts MAJOR.MINOR.PATCH[-pre][+build], with an optional
// leading "v" (release tags are v0.1.N).
func ParseVersion(s string) (Semver, error) {
	var v Semver
	in := strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(in, '+'); i >= 0 {
		v.Build = in[i+1:]
		in = in[:i]
		if v.Build == "" {
			return Semver{}, fmt.Errorf("version %q: empty build metadata", s)
		}
	}
	if i := strings.IndexByte(in, '-'); i >= 0 {
		pre := in[i+1:]
		in = in[:i]
		if pre == "" {
			return Semver{}, fmt.Errorf("version %q: empty prerelease", s)
		}
		v.Pre = strings.Split(pre, ".")
		for _, id := range v.Pre {
			if id == "" {
				return Semver{}, fmt.Errorf("version %q: empty prerelease identifier", s)
			}
		}
	}
	parts := strings.Split(in, ".")
	if len(parts) != 3 {
		return Semver{}, fmt.Errorf("version %q: want MAJOR.MINOR.PATCH", s)
	}
	nums := make([]uint64, 3)
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil || (len(p) > 1 && p[0] == '0') {
			return Semver{}, fmt.Errorf("version %q: bad numeric component %q", s, p)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

// Compare orders a and b by semver precedence: -1, 0 or +1. A prerelease sorts
// below the same release (0.1.5-dev < 0.1.5); numeric identifiers compare
// numerically and below alphanumeric ones.
func (a Semver) Compare(b Semver) int {
	for _, d := range [][2]uint64{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if d[0] != d[1] {
			if d[0] < d[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0:
		return 1
	case len(b.Pre) == 0:
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		if c := compareIdent(a.Pre[i], b.Pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.Pre) < len(b.Pre):
		return -1
	case len(a.Pre) > len(b.Pre):
		return 1
	}
	return 0
}

func compareIdent(a, b string) int {
	an, aErr := strconv.ParseUint(a, 10, 64)
	bn, bErr := strconv.ParseUint(b, 10, 64)
	switch {
	case aErr == nil && bErr == nil:
		if an < bn {
			return -1
		} else if an > bn {
			return 1
		}
		return 0
	case aErr == nil:
		return -1
	case bErr == nil:
		return 1
	}
	return strings.Compare(a, b)
}

// CompareVersions compares two version strings. Unparseable versions sort
// below every parseable one (and equal to each other), so a garbled label can
// never look newer than a real release.
func CompareVersions(a, b string) int {
	va, errA := ParseVersion(a)
	vb, errB := ParseVersion(b)
	switch {
	case errA != nil && errB != nil:
		return 0
	case errA != nil:
		return -1
	case errB != nil:
		return 1
	}
	return va.Compare(vb)
}

// Newer reports whether candidate is strictly newer than current.
func Newer(candidate, current string) bool { return CompareVersions(candidate, current) > 0 }
