package claudecfg

import (
	"errors"
	"io/fs"
	"os"
	"strings"

	"amux/internal/cfghome"
	"amux/internal/hostprep"
)

// Unattended launch: amux agents run with nobody watching the pane, so every
// Claude Code dialog that waits for a keypress stalls the agent until a human
// notices. These are the settings and state that pre-answer the auto-mode ones
// (the inventory is in docs/sandbox-config.md, "Unattended launch"):
//
//   - The auto-mode opt-in / entry warning is skipped when skipAutoPermissionPrompt
//     is true in user, flag, or managed settings (or the hasSeenAutoModeEntryWarning
//     state flag is set). Older releases showed it as a blocking dialog.
//   - The "teach auto mode about your environment" onboarding dialog is skipped
//     once autoMode.environment is non-empty in user, flag, or managed settings
//     (project and local settings are ignored for autoMode). Otherwise it appears
//     after five startups and five classifier denials — routine for a long-lived
//     agent.
//   - The first-run theme/login onboarding is skipped by hasCompletedOnboarding in
//     .claude.json; folder trust by projects.<dir>.hasTrustDialogAccepted (TrustDir).
//
// The settings keys are written into the agent's private settings.json — the
// only user-level settings source an agent has — at seed time and again before
// every launch (EnsureUnattendedRooted), as an idempotent merge that leaves every
// other key alone. Drift scans normalize them away (normalizeSettings), so the
// values amux manages never read as the agent or the user diverging.
const (
	// SkipAutoPermissionPromptKey records acceptance of the auto mode opt-in.
	SkipAutoPermissionPromptKey = "skipAutoPermissionPrompt"
	// AutoModeKey is the auto mode classifier customization object; its
	// AutoModeEnvironmentKey array is the classifier's environment section.
	AutoModeKey            = "autoMode"
	AutoModeEnvironmentKey = "environment"
	// AutoModeDefaults is the entry that splices Claude's built-in entries into
	// an autoMode array at its position. An environment of just this entry is
	// exactly the built-in environment: it answers the onboarding dialog without
	// changing what the classifier is told.
	AutoModeDefaults = "$defaults"
	// HasCompletedOnboardingKey is the .claude.json flag that skips first-run
	// onboarding (theme picker, login walkthrough).
	HasCompletedOnboardingKey = "hasCompletedOnboarding"
	// PermissionModeEnv overrides the --permission-mode amux launches Claude with
	// ("none" omits the flag, leaving the settings' defaultMode in charge).
	PermissionModeEnv = "AMUX_PERMISSION_MODE"
)

// LaunchPermissionMode is the --permission-mode amux launches Claude with: auto
// by default, AMUX_PERMISSION_MODE to override, "" when the flag is omitted.
func LaunchPermissionMode() string {
	pm := os.Getenv(PermissionModeEnv)
	switch pm {
	case "":
		return "auto"
	case "none":
		return ""
	}
	return pm
}

// ApplyUnattendedSettings merges the auto-mode pre-answers into a settings.json
// body: skipAutoPermissionPrompt=true when the agent will run in auto mode (the
// launch flag says so, or no flag is passed and permissions.defaultMode is auto),
// and an autoMode.environment that contains "$defaults" — the user's own entries
// when they have some, else just ["$defaults"]. allow/soft_deny/hard_deny rules
// are never touched. It reports whether anything changed; unchanged input is
// returned byte-for-byte, so the merge is idempotent and never reformats a file
// it has nothing to add to. Malformed JSON is returned unchanged (Claude reports
// it; amux must not clobber it).
func ApplyUnattendedSettings(b []byte, launchMode string) ([]byte, bool) {
	root := map[string]any{}
	if len(strings.TrimSpace(string(b))) > 0 {
		var ok bool
		if root, ok = decodeObject(b); !ok {
			return b, false
		}
	}
	changed := false
	if runsAuto(root, launchMode) {
		if v, _ := root[SkipAutoPermissionPromptKey].(bool); !v {
			root[SkipAutoPermissionPromptKey] = true
			changed = true
		}
	}
	am, isObject := root[AutoModeKey].(map[string]any)
	if _, present := root[AutoModeKey]; !present {
		am, isObject = map[string]any{}, true
	}
	// A non-object autoMode is the user's (invalid) value: left alone.
	if isObject {
		if env, ok := withDefaults(am[AutoModeEnvironmentKey]); ok {
			am[AutoModeEnvironmentKey] = env
			root[AutoModeKey] = am
			changed = true
		}
	}
	if !changed {
		return b, false
	}
	return encodeObject(root), true
}

// runsAuto reports whether a Claude launched with launchMode under these
// settings starts in auto mode.
func runsAuto(settings map[string]any, launchMode string) bool {
	if launchMode != "" {
		return launchMode == "auto"
	}
	perms, _ := settings["permissions"].(map[string]any)
	mode, _ := perms["defaultMode"].(string)
	return mode == "auto"
}

// withDefaults returns env with "$defaults" ensured, and whether that differs
// from env. A missing or empty environment becomes ["$defaults"]; one that lacks
// it gets it prepended, keeping the user's entries. A non-array value is left
// alone (changed=false).
func withDefaults(v any) ([]any, bool) {
	if v == nil {
		return []any{AutoModeDefaults}, true
	}
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	for _, e := range list {
		if s, _ := e.(string); s == AutoModeDefaults {
			return list, false
		}
	}
	return append([]any{AutoModeDefaults}, list...), true
}

// normalizeSettings projects a settings.json onto the configuration a drift scan
// should compare: canonical key order and formatting (so Claude's own rewrites
// never register), with the amux-managed values factored out — a true
// skipAutoPermissionPrompt is dropped, and autoMode.environment is compared with
// "$defaults" ensured, a bare ["$defaults"] counting as absent. The same
// projection is applied to the template and the copy, so a copy amux merged the
// keys into still matches a template without them, while a real edit to the
// environment entries (or any other key) still shows as drift.
func normalizeSettings(b []byte) []byte {
	root, ok := decodeObject(b)
	if !ok {
		return b
	}
	if v, _ := root[SkipAutoPermissionPromptKey].(bool); v {
		delete(root, SkipAutoPermissionPromptKey)
	}
	if am, ok := root[AutoModeKey].(map[string]any); ok {
		if list, ok := am[AutoModeEnvironmentKey].([]any); ok { // absent counts as ["$defaults"]
			env, _ := withDefaults(list)
			if len(env) == 1 {
				delete(am, AutoModeEnvironmentKey) // just "$defaults"
			} else {
				am[AutoModeEnvironmentKey] = env
			}
		}
		if len(am) == 0 {
			delete(root, AutoModeKey)
		}
	}
	return encodeObject(root)
}

// mergeSettings is settings.json's Promote: the agent's copy, with its paths
// mapped back to the template's, minus the values amux merged in that the
// template does not already hold — a skipAutoPermissionPrompt the template lacks,
// and an autoMode.environment that is just ["$defaults"] where the template has
// none. A promoted settings change therefore carries the agent's edit, not
// amux's launch plumbing.
func mergeSettings(sp cfghome.Spec, template, copy []byte) ([]byte, error) {
	copy = restoreTemplatePaths(sp, copy)
	src, ok := decodeObject(copy)
	if !ok {
		return copy, nil
	}
	dst, _ := decodeObject(template)
	if _, had := dst[SkipAutoPermissionPromptKey]; !had {
		if v, _ := src[SkipAutoPermissionPromptKey].(bool); v {
			delete(src, SkipAutoPermissionPromptKey)
		}
	}
	tam, _ := dst[AutoModeKey].(map[string]any)
	if _, had := tam[AutoModeEnvironmentKey]; !had {
		if am, ok := src[AutoModeKey].(map[string]any); ok {
			if list, _ := am[AutoModeEnvironmentKey].([]any); len(list) == 1 && list[0] == AutoModeDefaults {
				delete(am, AutoModeEnvironmentKey)
				if len(am) == 0 {
					delete(src, AutoModeKey)
				}
			}
		}
	}
	return encodeObject(src), nil
}

// EnsureUnattendedRooted applies the unattended pre-answers to this (private)
// home through the pinned session root: ApplyUnattendedSettings on settings.json
// for launchMode, and hasCompletedOnboarding in .claude.json. It is idempotent —
// a home that already has them is not rewritten — so it runs before every
// launch, bringing sandboxes seeded before amux managed these keys up to date.
func (h Home) EnsureUnattendedRooted(session *hostprep.Root, launchMode string) error {
	mu.Lock()
	defer mu.Unlock()
	settings, err := session.Rel(h.SettingsPath())
	if err != nil {
		return err
	}
	b, err := session.ReadFile(settings)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if out, changed := ApplyUnattendedSettings(b, launchMode); changed {
		mode := fs.FileMode(0o644)
		if fi, err := session.StatFile(settings); err == nil {
			mode = fi.Mode().Perm()
		}
		if err := session.AtomicWrite(settings, out, mode); err != nil {
			return err
		}
	}

	config, err := session.Rel(h.ConfigPath())
	if err != nil {
		return err
	}
	root := map[string]any{}
	if b, err := session.ReadFile(config); err == nil {
		var ok bool
		if root, ok = decodeObject(b); !ok {
			return nil // corrupt: Claude repairs it from its backup; don't clobber
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if done, _ := root[HasCompletedOnboardingKey].(bool); done {
		return nil
	}
	root[HasCompletedOnboardingKey] = true
	return session.AtomicWrite(config, encodeObject(root), 0o600)
}
