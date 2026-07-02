package agent

import "path/filepath"

// agentsSkillsDir / agentsGuideFile are the vendor-neutral Agent Skills layout —
// project skills under .agents/skills and the guide in AGENTS.md, both under the
// workspace root. Every harness without its own native location (Codex, Hermes,
// an unrecognized kind) reads here, so amux installs its skill library and writes
// the sandbox guide to the file each CLI actually loads.
func agentsSkillsDir(root string) string { return filepath.Join(root, ".agents", "skills") }
func agentsGuideFile(root string) string { return filepath.Join(root, "AGENTS.md") }
