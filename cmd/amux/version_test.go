package main

import (
	"errors"
	"strings"
	"testing"

	"amux/internal/buildinfo"
	"amux/internal/core"
)

func TestVersionLinesOfflineStillReportsCLI(t *testing.T) {
	lines, incompatible := versionLines(versionReport{CLI: "1.2.3"}, false)
	got := strings.Join(lines, "\n")
	for _, want := range []string{"cli       1.2.3", "daemon    offline", "database  unavailable"} {
		if !strings.Contains(got, want) {
			t.Errorf("version output missing %q:\n%s", want, got)
		}
	}
	if incompatible {
		t.Fatal("an offline daemon is not a known incompatibility")
	}
}

func TestVersionLinesChecksProtocolAndSchemaIndependently(t *testing.T) {
	r := versionReport{
		CLI:       "1.2.3",
		Connected: true,
		Runtime: core.VersionInfo{
			DaemonVersion:     "1.1.0",
			DaemonProtocol:    buildinfo.DaemonProtocol,
			DatabaseSchema:    2,
			DatabaseMinSchema: 1,
			DatabaseMaxSchema: 2,
		},
	}
	lines, incompatible := versionLines(r, true)
	got := strings.Join(lines, "\n")
	for _, want := range []string{"✓ daemon", "1.1.0", "protocol 1", "compatible with CLI", "✓ database", "schema 2", "range 1–2"} {
		if !strings.Contains(got, want) {
			t.Errorf("doctor version output missing %q:\n%s", want, got)
		}
	}
	if incompatible {
		t.Fatal("different product versions with compatible contracts were rejected")
	}

	r.Runtime.DaemonProtocol++
	r.Runtime.DatabaseSchema++
	lines, incompatible = versionLines(r, true)
	got = strings.Join(lines, "\n")
	for _, want := range []string{"✗ daemon", "incompatible with CLI protocol", "✗ database", "incompatible with daemon range"} {
		if !strings.Contains(got, want) {
			t.Errorf("incompatible output missing %q:\n%s", want, got)
		}
	}
	if !incompatible {
		t.Fatal("known protocol/schema mismatches must fail doctor")
	}
}

func TestVersionLinesDoesNotGuessAboutAnOlderDaemon(t *testing.T) {
	lines, incompatible := versionLines(versionReport{
		CLI: "1.3.0", Connected: true, QueryErr: errors.New(`unknown query "version"`),
	}, true)
	got := strings.Join(lines, "\n")
	if !strings.Contains(got, "version unavailable") || !strings.Contains(got, "restart") || !strings.Contains(got, "schema unavailable") {
		t.Fatalf("old-daemon output lacks the bounded unknown state:\n%s", got)
	}
	if incompatible {
		t.Fatal("an unreporting older daemon cannot be proven incompatible")
	}
}
