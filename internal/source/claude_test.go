package source

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeStub creates an executable shell script that prints the given stdout,
// used to stand in for the real `claude` binary.
func writeStub(t *testing.T, stdout string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub uses /bin/sh")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "claude-stub")
	script := "#!/bin/sh\ncat <<'EOF'\n" + stdout + "\nEOF\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClaudePollClassifies(t *testing.T) {
	stub := writeStub(t, `[
      {"pid": 4242, "cwd": "/home/u/projects/web-app", "kind": "interactive", "startedAt": 1782054596574, "sessionId": "af8e2985-1c33-4a64-84bd-240c8e48f4b3", "status": "busy"},
      {"pid": 0, "cwd": "/home/u/projects/api-service", "kind": "background", "startedAt": 1782054000000, "sessionId": "bb28740e-cloud", "status": "queued"}
    ]`)

	c := &Claude{Bin: stub}
	sessions, err := c.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("want 2 sessions, got %d", len(sessions))
	}

	local := sessions[0]
	if local.Source != "claude" || local.Kind != "interactive" {
		t.Errorf("local: unexpected source/kind: %+v", local)
	}
	if local.Title != "web-app" {
		t.Errorf("local title = %q, want web-app", local.Title)
	}
	if !local.CanKill || local.CanResume {
		t.Errorf("local: want CanKill && !CanResume, got kill=%v resume=%v", local.CanKill, local.CanResume)
	}

	cloud := sessions[1]
	if cloud.Kind != "background" {
		t.Errorf("cloud kind = %q", cloud.Kind)
	}
	if cloud.CanAttach || !cloud.CanResume {
		t.Errorf("cloud: want !CanAttach && CanResume, got attach=%v resume=%v", cloud.CanAttach, cloud.CanResume)
	}
}

func TestClaudePollNoBinary(t *testing.T) {
	c := &Claude{Bin: "/nonexistent/claude-binary-xyz"}
	sessions, err := c.Poll(context.Background())
	if err != nil {
		t.Fatalf("missing binary should be silent, got err: %v", err)
	}
	if sessions != nil {
		t.Fatalf("want nil sessions, got %v", sessions)
	}
}
