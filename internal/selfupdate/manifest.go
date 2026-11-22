package selfupdate

import (
	"encoding/json"
	"fmt"

	"amux/internal/buildinfo"
)

// ManifestAsset is the release asset describing the release itself. The
// release workflow writes it by running the built binary's
// `amux version --manifest`, so its contract numbers cannot drift from code.
const ManifestAsset = "amux-release.json"

// SumsAsset lists "<sha256>  <asset>" for every other asset.
const SumsAsset = "SHA256SUMS"

// ShimAsset is the shell shim installed to the config directory.
const ShimAsset = "amux.sh"

// BinaryAsset is the release binary name for a platform.
func BinaryAsset(goos, goarch string) string { return "amux-" + goos + "-" + goarch }

// Manifest carries a build's product version and the compatibility contracts
// from docs/versioning.md: the CLI↔daemon protocol and the database schema
// range its daemon can open/migrate.
type Manifest struct {
	Version           string `json:"version"`
	Commit            string `json:"commit,omitempty"`
	DaemonProtocol    int    `json:"daemonProtocol"`
	DatabaseMinSchema int    `json:"databaseMinSchema"`
	DatabaseMaxSchema int    `json:"databaseMaxSchema"`
}

// Validate rejects a manifest that is not a usable release description.
func (m Manifest) Validate() error {
	if !buildinfo.IsRelease(m.Version) {
		return fmt.Errorf("release manifest version %q is not a release version", m.Version)
	}
	if m.DaemonProtocol <= 0 {
		return fmt.Errorf("release manifest %s: missing daemon protocol", m.Version)
	}
	if m.DatabaseMinSchema < 0 || m.DatabaseMaxSchema < m.DatabaseMinSchema {
		return fmt.Errorf("release manifest %s: bad schema range %d–%d", m.Version, m.DatabaseMinSchema, m.DatabaseMaxSchema)
	}
	return nil
}

// ParseManifest decodes and validates a release manifest.
func ParseManifest(b []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("decode %s: %w", ManifestAsset, err)
	}
	return m, m.Validate()
}

// Compat classifies how a candidate release may replace the running build.
type Compat int

const (
	// CompatHot: same daemon protocol and database schema. The binary on disk
	// can be replaced while the old daemon keeps running; new CLI processes
	// still speak its protocol, and the daemon loads the new code at its next
	// restart.
	CompatHot Compat = iota
	// CompatRestart: the protocol or schema changes. Swapping the binary under
	// a running daemon would leave new CLIs unable to talk to it, so the binary
	// is swapped only together with a daemon restart.
	CompatRestart
	// CompatNever: the candidate cannot open this installation's database (its
	// schema range ends below the current schema, or starts above it). Installing
	// it would require lowering user_version, which amux never does.
	CompatNever
)

func (c Compat) String() string {
	switch c {
	case CompatHot:
		return "hot"
	case CompatRestart:
		return "restart"
	}
	return "never"
}

// Compatibility compares a candidate release against the running build's own
// manifest. The running build's DatabaseMaxSchema is the schema its daemon has
// migrated the database to.
func Compatibility(candidate, running Manifest) Compat {
	schema := running.DatabaseMaxSchema
	if candidate.DatabaseMaxSchema < schema || candidate.DatabaseMinSchema > schema {
		return CompatNever
	}
	if candidate.DaemonProtocol != running.DaemonProtocol || candidate.DatabaseMaxSchema != schema {
		return CompatRestart
	}
	return CompatHot
}
