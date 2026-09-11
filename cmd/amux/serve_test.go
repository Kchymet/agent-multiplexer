package main

import (
	"strings"
	"testing"
)

func TestStandaloneHarnessFailsClosed(t *testing.T) {
	err := cmdHarness()
	if err == nil || !strings.Contains(err.Error(), "authenticated inherited host channel") {
		t.Fatalf("cmdHarness() error = %v", err)
	}
}
