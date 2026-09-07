package main

import (
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"

	"amux/internal/buildinfo"
	"amux/internal/core"
	"amux/internal/daemon"
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

func TestDoctorReportsDeniedDaemonStateAndFails(t *testing.T) {
	sandboxCLI(t)
	old := versionDial
	versionDial = func() (*daemon.Client, error) { return nil, deniedSocketError() }
	t.Cleanup(func() { versionDial = old })

	out, err := captureOutput(t, cmdDoctor)
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("doctor error = %v, want wrapped EPERM\n%s", err, out)
	}
	for _, want := range []string{
		"state unknown; access denied",
		"unable to determine daemon liveness",
		"doctor never attempts startup",
		"database  unavailable because daemon state is unknown",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "this client cannot use the running daemon") {
		t.Fatalf("doctor inferred a live daemon from EPERM:\n%s", out)
	}
}

func TestVersionLinesDoNotCallUnexpectedRejectionOffline(t *testing.T) {
	errRejected := errors.New("credential revoked")
	lines, incompatible := versionLines(versionReport{CLI: "1.2.3", ConnectErr: errRejected}, true)
	got := strings.Join(lines, "\n")
	for _, want := range []string{"✗ daemon", "state unknown", "credential revoked", "database", "state unknown"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diagnostic output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "daemon    offline") {
		t.Fatalf("unexpected rejection was called offline:\n%s", got)
	}
	if incompatible {
		t.Fatal("an unknown daemon state is not a demonstrated protocol incompatibility")
	}
}

func TestDoctorFailsWhenUnexpectedConnectionLeavesStateUnknown(t *testing.T) {
	sandboxCLI(t)
	errRejected := errors.New("credential revoked")
	old := versionDial
	versionDial = func() (*daemon.Client, error) { return nil, errRejected }
	t.Cleanup(func() { versionDial = old })

	out, err := captureOutput(t, cmdDoctor)
	if !errors.Is(err, errRejected) {
		t.Fatalf("doctor error = %v, want original rejection\n%s", err, out)
	}
	for _, want := range []string{"state unknown; connection failed", "credential revoked", "refusing to infer offline"} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor output missing %q:\n%s", want, out)
		}
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

func TestVersionQueryFailuresKeepHealthUnknownWithoutRestartAdvice(t *testing.T) {
	for name, queryErr := range map[string]error{
		"timeout":             syscall.ETIMEDOUT,
		"EOF":                 io.EOF,
		"invalid response":    errors.New("daemon returned incomplete version information"),
		"credential rejected": errors.New("credential revoked"),
	} {
		t.Run(name, func(t *testing.T) {
			r := versionReport{CLI: "1.3.0", Connected: true, QueryErr: queryErr}
			lines, incompatible := versionLines(r, true)
			got := strings.Join(lines, "\n")
			for _, want := range []string{"✗ daemon", "state unknown", "version query failed", queryErr.Error(), "schema unavailable"} {
				if !strings.Contains(got, want) {
					t.Fatalf("query diagnostic missing %q:\n%s", want, got)
				}
			}
			for _, reject := range []string{"restart", "unsupported", "legacy daemon"} {
				if strings.Contains(got, reject) {
					t.Fatalf("query diagnostic made unsupported compatibility claim %q:\n%s", reject, got)
				}
			}
			if !errors.Is(versionStateError(r), queryErr) {
				t.Fatalf("versionStateError(%v) did not preserve query error", queryErr)
			}
			if incompatible {
				t.Fatal("unknown query health is not a demonstrated incompatibility")
			}
		})
	}
}

func TestDoctorAndVersionFailOnUnknownQueryHealth(t *testing.T) {
	sandboxCLI(t)
	errRejected := errors.New("credential revoked")
	old := collectVersionReport
	collectVersionReport = func() versionReport {
		return versionReport{CLI: "1.3.0", Connected: true, QueryErr: errRejected}
	}
	t.Cleanup(func() { collectVersionReport = old })

	for name, run := range map[string]func() error{"doctor": cmdDoctor, "version": cmdVersion} {
		t.Run(name, func(t *testing.T) {
			out, err := captureOutput(t, run)
			if !errors.Is(err, errRejected) {
				t.Fatalf("%s error = %v, want original rejection\n%s", name, err, out)
			}
			if !strings.Contains(out, "state unknown") || !strings.Contains(out, "credential revoked") {
				t.Fatalf("%s output lost unknown state/cause:\n%s", name, out)
			}
			if strings.Contains(out, "restart to load") || strings.Contains(out, "version query unsupported") {
				t.Fatalf("%s output gave unsupported restart guidance:\n%s", name, out)
			}
		})
	}
}

func TestVersionLinesRecognizesExplicitLegacyQuerySignal(t *testing.T) {
	lines, incompatible := versionLines(versionReport{
		CLI: "1.3.0", Connected: true, QueryErr: errors.New(`unknown query "version"`),
	}, true)
	got := strings.Join(lines, "\n")
	if !strings.Contains(got, "version query unsupported") || !strings.Contains(got, "restart") || !strings.Contains(got, "schema unavailable") {
		t.Fatalf("old-daemon output lacks the bounded unknown state:\n%s", got)
	}
	if incompatible {
		t.Fatal("an unreporting older daemon cannot be proven incompatible")
	}
}
