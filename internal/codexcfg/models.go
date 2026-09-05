package codexcfg

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ModelsCacheFile is Codex's cached copy of the model catalog its backend serves
// for the signed-in account (<home>/models_cache.json). Codex refreshes it on
// launch and drives its own /model picker from it, so it is the authoritative
// "what can this account run" list — including models that post-date amux's
// built-in catalog and account-gated previews.
const ModelsCacheFile = "models_cache.json"

// ModelsCachePath is <home>/models_cache.json.
func (h Home) ModelsCachePath() string { return filepath.Join(h.Dir(), ModelsCacheFile) }

// cachedModel is the subset of one models-cache entry amux reads, tolerant of
// the two shapes Codex has written: the raw catalog entry (slug + visibility +
// priority) and the older picker preset (model + show_in_picker). Every field is
// optional so a catalog that grows new fields still parses.
type cachedModel struct {
	Slug         string   `json:"slug"`
	Model        string   `json:"model"`
	ID           string   `json:"id"`
	Visibility   string   `json:"visibility"`
	ShowInPicker *bool    `json:"show_in_picker"`
	Priority     *float64 `json:"priority"`
}

// name is the model identifier Codex accepts on --model.
func (m cachedModel) name() string {
	for _, s := range []string{m.Slug, m.Model, m.ID} {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

// listed reports whether Codex would offer the model in its own picker: a
// "visibility" of list (anything else — "hide" — is excluded), or a true
// show_in_picker; an entry that says neither is offered.
func (m cachedModel) listed() bool {
	if m.Visibility != "" {
		return strings.EqualFold(m.Visibility, "list")
	}
	if m.ShowInPicker != nil {
		return *m.ShowInPicker
	}
	return true
}

// rank is the picker sort key: ascending priority, with unranked entries last.
func (m cachedModel) rank() float64 {
	if m.Priority == nil {
		return math.Inf(1)
	}
	return *m.Priority
}

// CachedModels returns the picker-visible models from this home's models cache,
// in Codex's own picker order (ascending priority, ties in file order, no
// duplicates), or nil when the cache is missing, unreadable, or lists nothing —
// callers fall back to a built-in catalog. Best-effort by design: Codex owns the
// file and its format, so any surprise reads as "no cache" rather than an error.
func (h Home) CachedModels() []string {
	b, err := os.ReadFile(h.ModelsCachePath())
	if err != nil {
		return nil
	}
	var cache struct {
		Models []cachedModel `json:"models"`
	}
	if json.Unmarshal(b, &cache) != nil {
		return nil
	}
	var listed []cachedModel
	for _, m := range cache.Models {
		if m.name() != "" && m.listed() {
			listed = append(listed, m)
		}
	}
	sort.SliceStable(listed, func(i, j int) bool { return listed[i].rank() < listed[j].rank() })
	var out []string
	seen := map[string]bool{}
	for _, m := range listed {
		if n := m.name(); !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
