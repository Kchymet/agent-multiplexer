package main

import "testing"

// TestSelfAgentID covers how `amux agent done` resolves the caller's own store id
// without being handed one: an explicit --id flag wins, then $AMUX_WORKGROUP,
// then its legacy $AMUX_WORKSPACE alias, and it's empty when nothing identifies
// the agent (so the verb no-ops rather than archiving the wrong session).
func TestSelfAgentID(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}

	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{
			name: "AMUX_WORKGROUP resolves the id",
			env:  map[string]string{"AMUX_WORKGROUP": "wg-1"},
			want: "wg-1",
		},
		{
			name: "AMUX_WORKSPACE is the fallback alias",
			env:  map[string]string{"AMUX_WORKSPACE": "wg-2"},
			want: "wg-2",
		},
		{
			name: "AMUX_WORKGROUP wins over the alias",
			env:  map[string]string{"AMUX_WORKGROUP": "wg-1", "AMUX_WORKSPACE": "wg-2"},
			want: "wg-1",
		},
		{
			name: "--id flag overrides the environment",
			args: []string{"--id", "flag-id"},
			env:  map[string]string{"AMUX_WORKGROUP": "wg-1"},
			want: "flag-id",
		},
		{
			name: "--id=value form",
			args: []string{"--id=flag-id"},
			want: "flag-id",
		},
		{
			name: "no id anywhere is empty (no-op)",
			env:  map[string]string{},
			want: "",
		},
		{
			name: "blank env var is treated as unset",
			env:  map[string]string{"AMUX_WORKGROUP": "  "},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selfAgentID(tt.args, env(tt.env)); got != tt.want {
				t.Errorf("selfAgentID(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}
