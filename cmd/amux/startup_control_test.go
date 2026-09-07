package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"amux/internal/amuxcfg"
	"amux/internal/core"
	"amux/internal/daemon"
)

func stubStartupDial(t *testing.T, err error) {
	t.Helper()
	old := startupDial
	startupDial = func() (*daemon.Client, error) { return nil, err }
	t.Cleanup(func() { startupDial = old })
}

func deniedSocketError() error {
	return &net.OpError{
		Op:  "dial",
		Net: "unix",
		Err: os.NewSyscallError("connect", syscall.EPERM),
	}
}

func TestDaemonStartupPreservesPermissionDenialWithoutExec(t *testing.T) {
	for name, errno := range map[string]error{
		"EPERM":  syscall.EPERM,
		"EACCES": syscall.EACCES,
	} {
		t.Run(name, func(t *testing.T) {
			sandboxCLI(t)
			stubStartupDial(t, &net.OpError{
				Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", errno),
			})
			spawnCalls := 0
			oldCommand := daemonCommand
			daemonCommand = func(string, ...string) *exec.Cmd {
				spawnCalls++
				return exec.Command("/bin/false")
			}
			t.Cleanup(func() { daemonCommand = oldCommand })

			for lifecycle, start := range map[string]func(string) error{
				"automatic": ensureDaemon,
				"manual":    daemonStart,
			} {
				err := start("would-be-daemon")
				if !errors.Is(err, errno) {
					t.Fatalf("%s error = %v, want wrapped %v", lifecycle, err, errno)
				}
				if strings.Contains(err.Error(), "timeout") {
					t.Fatalf("%s denial was replaced by a startup timeout: %v", lifecycle, err)
				}
			}
			if err := daemonRun(); !errors.Is(err, errno) {
				t.Fatalf("foreground daemon error = %v, want wrapped %v", err, errno)
			}
			if err := daemonRestart(); !errors.Is(err, errno) {
				t.Fatalf("restart error = %v, want wrapped %v", err, errno)
			}
			if spawnCalls != 0 {
				t.Fatalf("permission-denied lifecycle attempted %d daemon exec(s)", spawnCalls)
			}
		})
	}
}

func TestDeniedRestartDoesNotSignalPidfileProcess(t *testing.T) {
	sandboxCLI(t)
	stubStartupDial(t, deniedSocketError())
	if err := os.MkdirAll(core.StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}

	child := exec.Command("/bin/sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	reaped := false
	t.Cleanup(func() {
		if reaped {
			return
		}
		_ = child.Process.Kill()
		<-exited
	})
	if err := os.WriteFile(core.PidPath(), []byte(strconv.Itoa(child.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := daemonRestart(); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("restart error = %v, want wrapped EPERM", err)
	}
	select {
	case waitErr := <-exited:
		reaped = true
		t.Fatalf("denied restart signaled unrelated pidfile process: %v", waitErr)
	case <-time.After(100 * time.Millisecond):
		// Still running: the denied probe returned before pidfile handling.
	}
}

func TestDaemonStartupRejectsUnexpectedConnectionErrorWithoutExec(t *testing.T) {
	sandboxCLI(t)
	errRejected := errors.New("daemon credential rejected")
	stubStartupDial(t, errRejected)

	for name, start := range map[string]func(string) error{
		"automatic": ensureDaemon,
		"manual":    daemonStart,
	} {
		t.Run(name, func(t *testing.T) {
			err := start("/executable-must-not-run")
			if !errors.Is(err, errRejected) {
				t.Fatalf("start error = %v, want original rejection", err)
			}
			if errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "not starting") {
				t.Fatalf("unexpected connection error fell through to exec: %v", err)
			}
		})
	}
	if err := daemonRun(); !errors.Is(err, errRejected) {
		t.Fatalf("foreground daemon error = %v, want original rejection", err)
	}
	if err := daemonRestart(); !errors.Is(err, errRejected) {
		t.Fatalf("restart error = %v, want original rejection", err)
	}
}

func TestDaemonStartupPreservesConnectionAndSpawnErrors(t *testing.T) {
	sandboxCLI(t)
	stubStartupDial(t, syscall.ECONNREFUSED)

	err := ensureDaemon("/executable-does-not-exist")
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("start error = %v, want original ECONNREFUSED", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("start error = %v, want executable failure too", err)
	}
}

func TestDaemonConnectionErrorOnlyCallsAbsentOrStaleOffline(t *testing.T) {
	errRejected := errors.New("credential revoked")
	for name, tc := range map[string]struct {
		err     error
		offline bool
	}{
		"absent":     {err: syscall.ENOENT, offline: true},
		"stale":      {err: syscall.ECONNREFUSED, offline: true},
		"permission": {err: deniedSocketError()},
		"rejected":   {err: errRejected},
	} {
		t.Run(name, func(t *testing.T) {
			err := daemonConnectionError(tc.err)
			if !errors.Is(err, tc.err) {
				t.Fatalf("connection error %v lost original %v", err, tc.err)
			}
			if got := strings.Contains(err.Error(), "daemon offline"); got != tc.offline {
				t.Fatalf("connection error = %q, offline=%v want %v", err, got, tc.offline)
			}
			if !tc.offline && !strings.Contains(err.Error(), "state unknown") {
				t.Fatalf("unexpected connection error was not unknown: %v", err)
			}
		})
	}
}

func TestDaemonLifecycleDoesNotStartOrRestartInsideAgent(t *testing.T) {
	sandboxCLI(t)
	t.Setenv("AMUX_WORKGROUP", "agent-123")
	stubStartupDial(t, syscall.ENOENT)

	for name, run := range map[string]func() error{
		"automatic":  func() error { return ensureDaemon("/executable-must-not-run") },
		"manual":     func() error { return daemonStart("/executable-must-not-run") },
		"foreground": daemonRun,
		"restart":    daemonRestart,
	} {
		t.Run(name, func(t *testing.T) {
			err := run()
			if err == nil || !strings.Contains(err.Error(), "inside an amux agent") {
				t.Fatalf("lifecycle error = %v, want agent-session refusal", err)
			}
		})
	}
}

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
