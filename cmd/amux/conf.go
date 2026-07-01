package main

import "amux/internal/claudecfg"

// cleanupGlobalHooks removes amux's legacy global Claude Code hooks from the
// user's settings.json. amux used to install its status hooks there, pinned to
// the running binary's path — which broke once that binary (often a per-session
// dev build) was removed, since the dead path then failed for every session.
// Hooks are now installed per-agent in each agent's own directory (see
// wsops.AgentCommand → claudecfg.InstallHooksIn), so the global entries are pure
// breakage to strip. Idempotent and best-effort.
func cleanupGlobalHooks() {
	_ = claudecfg.UninstallHooks()
}
