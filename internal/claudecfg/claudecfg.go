// Package claudecfg makes minimal, safe edits to Claude Code's user config
// (~/.claude.json) on amux's behalf. Today that's pre-trusting directories amux
// creates so Claude Code doesn't show the interactive "trust this folder?"
// dialog for a freshly-spawned workspace.
package claudecfg

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var mu sync.Mutex // serialize our own read-modify-write

// projectsRoot is where Claude Code stores per-directory session transcripts.
func projectsRoot() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects")
}

// munge maps an absolute path to Claude Code's project-dir name ('/' and '.'
// become '-'), e.g. /home/u/.local/x -> -home-u--local-x.
func munge(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '.' {
			return '-'
		}
		return r
	}, abs)
}

// SessionExists reports whether a saved session with uuid exists for cwd.
func SessionExists(cwd, uuid string) bool {
	if uuid == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(projectsRoot(), munge(cwd), uuid+".jsonl"))
	return err == nil
}

// AnySession reports whether cwd has any saved Claude session transcript.
func AnySession(cwd string) bool {
	ents, err := os.ReadDir(filepath.Join(projectsRoot(), munge(cwd)))
	if err != nil {
		return false
	}
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			return true
		}
	}
	return false
}

// ConfigPath is ~/.claude.json (honoring CLAUDE_CONFIG_DIR if set).
func ConfigPath() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, ".claude.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}

// TrustDir marks dir as trusted in ~/.claude.json. Best-effort: on any error the
// caller should proceed (Claude will just show the trust dialog once). The whole
// file is round-tripped with json.Number so large integer fields aren't mangled,
// and written atomically so a concurrent Claude process never sees a partial file.
func TrustDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()

	path := ConfigPath()
	root := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.UseNumber()
		_ = dec.Decode(&root)
	}

	projects, ok := root["projects"].(map[string]any)
	if !ok || projects == nil {
		projects = map[string]any{}
		root["projects"] = projects
	}
	entry, ok := projects[abs].(map[string]any)
	if !ok || entry == nil {
		entry = map[string]any{}
		projects[abs] = entry
	}
	if t, _ := entry["hasTrustDialogAccepted"].(bool); t {
		return nil // already trusted; don't rewrite
	}
	entry["hasTrustDialogAccepted"] = true

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".amux.tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
