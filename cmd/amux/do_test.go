package main

import (
	"reflect"
	"testing"

	"amux/internal/core"
)

func TestParseDoArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want core.Action
	}{
		{
			name: "action only",
			args: []string{"refresh"},
			want: core.Action{Action: "refresh"},
		},
		{
			name: "positional id (back-compat)",
			args: []string{"attach", "3f7a"},
			want: core.Action{Action: "attach", ID: "3f7a"},
		},
		{
			name: "positional id and kind (back-compat)",
			args: []string{"new", "3f7a", "claude"},
			want: core.Action{Action: "new", ID: "3f7a", Kind: "claude"},
		},
		{
			name: "target flag",
			args: []string{"move", "3f7a", "--target", "9c1b"},
			want: core.Action{Action: "move", ID: "3f7a", Target: "9c1b"},
		},
		{
			name: "target shorthand inline",
			args: []string{"move", "3f7a", "-t=9c1b"},
			want: core.Action{Action: "move", ID: "3f7a", Target: "9c1b"},
		},
		{
			name: "single field",
			args: []string{"rename", "3f7a", "-f", "name=api spike"},
			want: core.Action{Action: "rename", ID: "3f7a", Fields: map[string]string{"name": "api spike"}},
		},
		{
			name: "repeated fields",
			args: []string{"add-agent", "9c1b", "-f", "repos=api,web", "--field", "prompt=do it"},
			want: core.Action{Action: "add-agent", ID: "9c1b", Fields: map[string]string{"repos": "api,web", "prompt": "do it"}},
		},
		{
			name: "id flag instead of positional",
			args: []string{"attach", "--id", "3f7a"},
			want: core.Action{Action: "attach", ID: "3f7a"},
		},
		{
			name: "field value may contain equals",
			args: []string{"new-workgroup", "-f", "prompt=a=b"},
			want: core.Action{Action: "new-workgroup", Fields: map[string]string{"prompt": "a=b"}},
		},
		{
			name: "empty field value is allowed",
			args: []string{"new-workgroup", "-f", "name="},
			want: core.Action{Action: "new-workgroup", Fields: map[string]string{"name": ""}},
		},
		{
			name: "kind and cwd flags",
			args: []string{"new", "--kind", "claude", "--cwd", "/tmp/x"},
			want: core.Action{Action: "new", Kind: "claude", Cwd: "/tmp/x"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDoArgs(tt.args)
			if err != nil {
				t.Fatalf("parseDoArgs(%v) error: %v", tt.args, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseDoArgs(%v) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

func TestParseDoArgsErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no action", nil},
		{"blank action", []string{"   "}},
		{"unknown flag", []string{"attach", "--nope", "x"}},
		{"flag missing value", []string{"move", "--target"}},
		{"field without equals", []string{"rename", "-f", "name"}},
		{"field with empty key", []string{"rename", "-f", "=value"}},
		{"too many positionals", []string{"new", "id", "kind", "extra"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseDoArgs(tt.args); err == nil {
				t.Errorf("parseDoArgs(%v) = nil error, want error", tt.args)
			}
		})
	}
}
