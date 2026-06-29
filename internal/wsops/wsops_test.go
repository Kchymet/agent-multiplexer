package wsops

import (
	"path/filepath"
	"testing"

	"amux/internal/store"
)

func TestAgentWorkdir(t *testing.T) {
	base := filepath.Join("sessions", "root1", "agent1")
	tests := []struct {
		name string
		repo string
		want string
	}{
		{"single repo drops into the worktree", "acme/api", filepath.Join(base, "acme/api")},
		{"multi repo stays at the agent dir", "acme/api,acme/web", base},
		{"no repo (e.g. console) stays put", "", base},
		{"blank entries are ignored, still single", " acme/api , ", filepath.Join(base, "acme/api")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := agentWorkdir(store.Session{Dir: base, Repo: tt.repo})
			if got != tt.want {
				t.Errorf("agentWorkdir(repo=%q) = %q, want %q", tt.repo, got, tt.want)
			}
		})
	}
}
