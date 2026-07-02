// Package skills installs amux's built-in agent skill library into an agent's
// workspace. The library is a small, opinionated set of playbooks — e.g. the
// end-to-end PR flow — that every amux agent should share, so an agent opens and
// shepherds a pull request the same way regardless of which repo it's working in.
//
// Each skill is a SKILL.md in the shared, cross-tool Agent Skills format,
// embedded in the amux binary. Where the skills are written is a *provider*
// decision, not this package's: different CLIs read skills from different places
// (Claude Code from .claude/skills, others from .agents/skills), so the caller
// passes the destination — see agent.Harness.SkillsDir. Install runs on every
// launch so the on-disk skills always track the running binary — the same
// freshness contract as the status hooks.
package skills

import (
	"embed"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// library holds the skill sources shipped with amux. Each subdirectory under
// library/ is one skill (a SKILL.md plus any supporting files).
//
//go:embed library
var library embed.FS

// libraryRoot is the embed prefix stripped when materializing files on disk.
const libraryRoot = "library"

// Install writes the embedded skill library into dest (the provider's skills
// directory — e.g. <root>/.claude/skills), overwriting amux's own skill files so
// they track the installed binary (the same freshness contract as the status
// hooks). It only writes the files it ships, so a user's own skills alongside
// them are never touched. Best-effort by contract for callers: like hook install,
// a failure here should not block launching the agent.
func Install(dest string) error {
	root := dest
	return fs.WalkDir(library, libraryRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, libraryRoot), "/")
		if rel == "" {
			return nil // the library root itself
		}
		dst := filepath.Join(root, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := library.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}
