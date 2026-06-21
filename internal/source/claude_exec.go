package source

import (
	"bytes"
	"context"
	"os/exec"
	"time"
)

// runClaudeAgents runs `claude agents --json` with a bounded timeout and
// returns the raw JSON array bytes.
func runClaudeAgents(ctx context.Context, bin string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "agents", "--json")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
