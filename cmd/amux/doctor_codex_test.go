package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"amux/internal/amuxcfg"
	"amux/internal/core"
)

func fakeControlDaemon(t *testing.T, selection *amuxcfg.Control) {
	t.Helper()
	dir, err := os.MkdirTemp("", "age233-doctor-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("AMUX_SOCK", filepath.Join(dir, "d.sock"))
	ln, err := net.Listen("unix", core.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		var action core.Action
		if err := json.NewDecoder(conn).Decode(&action); err != nil {
			return
		}
		reply := core.Data{Type: core.FrameData, Query: action.Query, Error: "unknown query"}
		if action.Query == core.QueryCodexControl && selection != nil {
			reply.OK, reply.Error = true, ""
			reply.Rows, _ = json.Marshal(selection)
		}
		_ = json.NewEncoder(conn).Encode(reply)
	}()
	t.Cleanup(func() { _ = ln.Close(); <-done })
}

func TestDoctorControlReportsDaemonNotShell(t *testing.T) {
	sandboxCLI(t)
	if err := amuxcfg.SetCodexControl(amuxcfg.AppServer); err != nil {
		t.Fatal(err)
	}
	t.Setenv(amuxcfg.ControlEnv, amuxcfg.AppServer)
	fakeControlDaemon(t, &amuxcfg.Control{
		ConfigPath: core.ConfigPath(), Persisted: amuxcfg.AppServer,
		Override: "pty", OverrideSet: true, Effective: "pty", Source: "environment",
	})
	out, err := captureOutput(t, func() error { reportCodexControl(); return nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"saved codex.control: app-server", "next start from this shell: app-server",
		` shell AMUX_CODEX_CONTROL: "app-server" (not daemon state)`,
		"running daemon: pty (source=environment", `AMUX_CODEX_CONTROL="pty"`,
		"daemon restart is needed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorControlUnknownOlderDaemon(t *testing.T) {
	sandboxCLI(t)
	t.Setenv(amuxcfg.ControlEnv, amuxcfg.AppServer)
	fakeControlDaemon(t, nil)
	out, err := captureOutput(t, func() error { reportCodexControl(); return nil })
	if err != nil || !strings.Contains(out, "running daemon: selection unknown") || strings.Contains(out, "running daemon: app-server") {
		t.Fatalf("doctor guessed an older daemon's mode: %s, %v", out, err)
	}
}

func TestDoctorControlInvalidSavedAndOffline(t *testing.T) {
	sandboxCLI(t)
	t.Setenv(amuxcfg.ControlEnv, "pty")
	if err := os.MkdirAll(core.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(core.ConfigPath(), []byte(`{"codex":{"control":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := captureOutput(t, func() error { reportCodexControl(); return nil })
	if err != nil || !strings.Contains(out, "startup rejected") || !strings.Contains(out, "running daemon: unknown/offline") {
		t.Fatalf("doctor: %s, %v", out, err)
	}
}
