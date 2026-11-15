package main

import (
	"strings"
	"testing"
)

// The retired stdio pane harness used to answer `amux harness` by refusing to
// spawn processes. The verb now manages harness installs; pin that a bare
// invocation never spawns anything and that unknown verbs fail cleanly.
func TestHarnessVerbNeverSpawnsAPaneHarness(t *testing.T) {
	err := cmdHarnessCLI([]string{"serve-stdio"})
	if err == nil || !strings.Contains(err.Error(), "unknown harness subcommand") {
		t.Fatalf("cmdHarnessCLI(serve-stdio) error = %v", err)
	}
}
