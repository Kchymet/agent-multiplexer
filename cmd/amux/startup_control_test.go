package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"amux/internal/amuxcfg"
	"amux/internal/core"
	"amux/internal/daemon"
)

// The shim below redirects the real detached-spawn path into this child. The
// daemon uses an empty, private store and cannot launch a paid model.
func TestControlDaemonChild(t *testing.T) {
	if os.Getenv("AGE233_TEST_DAEMON") != "1" {
		return
	}
	if err := daemonRun(); err != nil {
		t.Fatal(err)
	}
}

func TestAutoAndManualControl(t *testing.T) {
	for _, manual := range []bool{false, true} {
		for _, override := range []string{"absent", "", "pty"} {
			name := "auto/" + override
			if manual {
				name = "manual/" + override
			}
			t.Run(name, func(t *testing.T) {
				sandboxCLI(t)
				t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
				t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "claude"))
				// Keep the Unix socket path short, even under Go's long test temp path.
				sockDir, err := os.MkdirTemp("", "age233-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
				t.Setenv("AMUX_SOCK", filepath.Join(sockDir, "daemon.sock"))
				t.Setenv(amuxcfg.ControlEnv, override)
				if override == "absent" {
					if err := os.Unsetenv(amuxcfg.ControlEnv); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv("AMUX_CODEX_BIN", filepath.Join(sockDir, "no-model-binary"))
				if err := amuxcfg.SetCodexControl(amuxcfg.AppServer); err != nil {
					t.Fatal(err)
				}
				self, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("AGE233_TEST_BINARY", self)
				t.Setenv("AGE233_TEST_DAEMON", "1")
				shim := filepath.Join(sockDir, "amux")
				if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec \"$AGE233_TEST_BINARY\" -test.run='^TestControlDaemonChild$'\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				// Register cleanup before spawning, including a failed-start case.
				t.Cleanup(func() {
					if pid, err := daemonPid(); err == nil {
						_ = syscall.Kill(pid, syscall.SIGTERM)
						deadline := time.Now().Add(5 * time.Second)
						for time.Now().Before(deadline) {
							if _, err := os.Stat(core.PidPath()); os.IsNotExist(err) {
								return
							}
							time.Sleep(10 * time.Millisecond)
						}
						t.Error("private test daemon failed to stop")
					}
				})
				start := ensureDaemon
				if manual {
					start = daemonStart
				}
				if err := start(shim); err != nil {
					log, _ := os.ReadFile(core.LogPath())
					t.Fatalf("start: %v\n%s", err, log)
				}
				c, err := daemon.Dial()
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				selection, err := c.CodexControl()
				want := amuxcfg.AppServer
				if override == "pty" {
					want = amuxcfg.PTY
				}
				if err != nil || selection.Effective != want || selection.Persisted != amuxcfg.AppServer || selection.OverrideSet != (override != "absent") {
					t.Fatalf("startup selection = %+v, %v; want %s", selection, err, want)
				}
				// An ordinary ensure/start of an already-running daemon does not
				// reload config. Explicit restart rejects malformed config before
				// it can signal even this private daemon.
				if err := os.WriteFile(core.ConfigPath(), []byte(`{"codex":{"control":"bad"}}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := start(shim); err != nil {
					t.Fatal(err)
				}
				if err := daemonRestart(); err == nil || !strings.Contains(err.Error(), "codex.control") {
					t.Fatalf("restart: %v", err)
				}
				after, err := c.CodexControl()
				if err != nil || after != selection {
					t.Fatalf("existing daemon changed: %+v, %v", after, err)
				}
			})
		}
	}
}

func TestInvalidControlStartupPaths(t *testing.T) {
	sandboxCLI(t)
	if err := os.MkdirAll(core.ConfigDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(core.ConfigPath(), []byte(`{"codex":{"control":null}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, start := range []func(string) error{ensureDaemon, daemonStart} {
		if err := start("/does-not-exist"); err == nil || !strings.Contains(err.Error(), "codex.control") {
			t.Fatalf("startup should fail on config before exec: %v", err)
		}
	}
}
