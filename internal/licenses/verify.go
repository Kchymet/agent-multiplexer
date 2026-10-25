package licenses

import (
	"debug/buildinfo"
	"fmt"
	"strings"
)

// VerifyBuildInfo checks that an artifact's recorded Go toolchain, target, and
// linked module provenance are represented in the embedded notice bundle.
func VerifyBuildInfo(info *buildinfo.BuildInfo, bundle string) error {
	if !strings.Contains(bundle, "Go toolchain: "+info.GoVersion+"\n") {
		return fmt.Errorf("binary toolchain %s is not the bundled notice toolchain", info.GoVersion)
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	target := settings["GOOS"] + "/" + settings["GOARCH"]
	if target == "/" || !strings.Contains(bundle, "Supported targets: ") || !strings.Contains(bundle, target) {
		return fmt.Errorf("binary target %s is not represented in the notice bundle", target)
	}
	if settings["CGO_ENABLED"] != "0" {
		return fmt.Errorf("binary CGO_ENABLED=%s, supported notice build requires 0", settings["CGO_ENABLED"])
	}
	for _, dep := range info.Deps {
		module := dep
		if dep.Replace != nil {
			if dep.Path == "github.com/kchymet/agent-multiplexer/harnessproto" &&
				dep.Replace.Version == "(devel)" &&
				strings.Contains(bundle, "First-party module paths excluded") &&
				strings.Contains(bundle, dep.Path) {
				// A local replacement has no independent checksum/version. The
				// generator records this module path as an explicit first-party
				// exclusion instead of inventing third-party provenance.
				continue
			}
			module = dep.Replace
		}
		if module.Path == "" || module.Version == "" {
			continue
		}
		if !strings.Contains(bundle, module.Path+"@"+module.Version+"\n") {
			return fmt.Errorf("linked module %s@%s is absent from notice inventory", module.Path, module.Version)
		}
		if module.Sum != "" && !strings.Contains(bundle, "module checksum: "+module.Sum+"\n") {
			return fmt.Errorf("linked module %s@%s checksum is absent from notice inventory", module.Path, module.Version)
		}
	}
	return nil
}
