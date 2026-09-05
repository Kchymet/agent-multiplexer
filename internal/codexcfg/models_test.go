package codexcfg

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestCachedModels pins how amux reads Codex's models cache: only picker-visible
// entries, in ascending-priority order with file order breaking ties, without
// duplicates; and every degraded case (no file, malformed, nothing listed) reads
// as nil so the harness falls back to its built-in catalog.
func TestCachedModels(t *testing.T) {
	cases := []struct {
		name    string
		content string // "" means write no file at all
		want    []string
	}{
		{
			name: "catalog entries ordered by priority, hidden dropped",
			content: `{"fetched_at":"2026-09-01T00:00:00Z","etag":"x","client_version":"1.0.0","models":[
				{"slug":"gpt-5.4","visibility":"list","priority":2},
				{"slug":"gpt-5.5-astra","visibility":"list","priority":1},
				{"slug":"gpt-5.5-astra","visibility":"list","priority":1},
				{"slug":"gpt-5.3-codex","visibility":"hide","priority":0},
				{"slug":"gpt-5.4-mini","visibility":"list","priority":2},
				{"slug":"gpt-5.5","visibility":"list"},
				{"slug":"","visibility":"list","priority":0}]}`,
			want: []string{"gpt-5.5-astra", "gpt-5.4", "gpt-5.4-mini", "gpt-5.5"},
		},
		{
			name: "picker presets honor show_in_picker",
			content: `{"models":[
				{"id":"a","model":"gpt-5.5","show_in_picker":true},
				{"id":"b","model":"gpt-5.5-internal","show_in_picker":false},
				{"id":"c","model":"gpt-5.4"}]}`,
			want: []string{"gpt-5.5", "gpt-5.4"},
		},
		{name: "nothing listed", content: `{"models":[{"slug":"x","visibility":"hide"}]}`, want: nil},
		{name: "empty catalog", content: `{"models":[]}`, want: nil},
		{name: "missing file", content: "", want: nil},
		{name: "malformed json", content: `{not json`, want: nil},
		{name: "unexpected shape", content: `{"models":"gpt-5.5"}`, want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", dir)
			if tc.content != "" {
				if err := os.WriteFile(filepath.Join(dir, ModelsCacheFile), []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := UserHome().CachedModels(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("CachedModels() = %v, want %v", got, tc.want)
			}
		})
	}
}
