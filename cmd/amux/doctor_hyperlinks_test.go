package main

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckHyperlinks(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	inTmux := env(map[string]string{"TMUX": "/tmp/tmux-501/default,1,0"})
	feats := func(s string, err error) func() (string, error) {
		return func() (string, error) { return s, err }
	}
	for _, tc := range []struct {
		name   string
		getenv func(string) string
		feats  func() (string, error)
		want   string // substring of the first line; "" means no output
		fix    bool
	}{
		{"outside tmux", env(nil), feats("", errors.New("unused")), "", false},
		{"enabled", inTmux, feats("bpaste,ccolour,clipboard,hyperlinks,cstyle,focus,RGB,title", nil), "✓ links", false},
		{"tmux default features", inTmux, feats("bpaste,ccolour,clipboard,cstyle,focus,RGB,title", nil), "⚠ links", true},
		{"no client", inTmux, feats("", nil), "⚠ links", true},
		{"unreadable", inTmux, feats("", errors.New("no server")), "· links", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := checkHyperlinks(tc.getenv, tc.feats)
			if tc.want == "" {
				if lines != nil {
					t.Fatalf("got %q, want no output", lines)
				}
				return
			}
			if len(lines) == 0 || !strings.Contains(lines[0], tc.want) {
				t.Fatalf("got %q, want first line containing %q", lines, tc.want)
			}
			hasFix := strings.Contains(strings.Join(lines, "\n"), `set -as terminal-features ",*:hyperlinks"`)
			if hasFix != tc.fix {
				t.Fatalf("fix shown = %v, want %v: %q", hasFix, tc.fix, lines)
			}
		})
	}
}
