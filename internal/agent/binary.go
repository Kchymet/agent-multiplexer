package agent

import (
	"context"
	"os"
	"strings"
	"time"

	"amux/internal/harnessbin"
)

// Binary sources, in resolution precedence order.
const (
	BinaryFromEnv     = "env"     // AMUX_<KIND>_BIN
	BinaryFromConfig  = "config"  // config.json harnesses.<kind>.bin
	BinaryFromManaged = "managed" // amux-managed install (amux harness install)
	BinaryFromPath    = "path"    // PATH, login shell, well-known install dirs
)

// Binary is the resolved launch executable for a harness kind and where the
// choice came from.
type Binary struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Source string `json:"source"`
	// Version is the managed version when Source is managed.
	Version string `json:"version,omitempty"`
	// Var names the override variable/setting that selected it (env/config).
	Var string `json:"var,omitempty"`
}

// binaryResolvers are seams for tests.
var (
	managedBin    = harnessbin.ActiveBin
	configuredBin = func(kind string) string {
		s, err := harnessbin.LoadSettings()
		if err != nil {
			return ""
		}
		return s.Kind(kind).Bin
	}
)

// ResolveBinary picks kind's launch executable: an explicit override
// (AMUX_<KIND>_BIN, then config harnesses.<kind>.bin) wins, then the
// amux-managed install, then the user's own install found via PATH, login
// shell, and well-known locations (degrading to the bare name). A managed
// install resolves to its concrete version directory, so the launched process
// keeps that version even if amux activates an update while it runs.
func ResolveBinary(ctx context.Context, kind string) Binary {
	kind = Canonical(kind)
	envVar := "AMUX_" + strings.ToUpper(kind) + "_BIN"
	if v := os.Getenv(envVar); v != "" {
		return Binary{Kind: kind, Path: resolveContext(ctx, v), Source: BinaryFromEnv, Var: envVar}
	}
	if v := configuredBin(kind); v != "" {
		return Binary{Kind: kind, Path: resolveContext(ctx, v), Source: BinaryFromConfig, Var: "harnesses." + kind + ".bin"}
	}
	if p, ver, ok := managedBin(kind); ok {
		return Binary{Kind: kind, Path: p, Source: BinaryFromManaged, Version: ver}
	}
	return Binary{Kind: kind, Path: resolveContext(ctx, kind), Source: BinaryFromPath}
}

// launchBin is ResolveBinary's path with the launcher's default timeout.
func launchBin(kind string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return ResolveBinary(ctx, kind).Path
}

// Label is a short human description of where the binary came from.
func (b Binary) Label() string {
	switch b.Source {
	case BinaryFromManaged:
		return "amux-managed " + b.Version
	case BinaryFromEnv, BinaryFromConfig:
		return "override " + b.Var
	default:
		return "PATH"
	}
}
