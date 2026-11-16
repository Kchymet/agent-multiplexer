package harnessbin

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const helperEnv = "AMUX_HARNESSBIN_TEST_HELPER"

// TestHelperBlockUntilKilled is the child of TestGCSparesVersionWithLiveProcess
// (a no-op in a normal run).
func TestHelperBlockUntilKilled(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	fmt.Println("ready")
	select {}
}

// A real running process executing from a version dir protects it from GC.
func TestGCSparesVersionWithLiveProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process scan is unix-only")
	}
	m, _ := newTestManager(t)
	m.InUse = nil // the real scan
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		mustInstall(t, m, v)
	}
	// Run a copy of this test binary as 1.0.0's binary, in helper mode: a
	// native executable we control, so it neither dispatches on its name
	// (multi-call coreutils exit at once when named "claude") nor exits
	// before it is killed.
	self, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Skip(err)
	}
	bin := m.BinPath("claude", "1.0.0")
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestHelperBlockUntilKilled$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot exec copied binary: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	// Readiness: the helper prints "ready" only after it is running from the
	// copied path, and then blocks until killed, so it is live (not a zombie)
	// for the rest of the test.
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(out).ReadString('\n')
		if err == nil && strings.TrimSpace(line) != "ready" {
			err = fmt.Errorf("helper said %q", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("helper not ready: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("helper never became ready")
	}
	exes, err := runningExecutables()
	if err != nil {
		t.Skipf("process scan unavailable: %v", err)
	}
	realBin, _ := filepath.EvalSymlinks(bin)
	found := false
	for _, e := range exes {
		if e == bin || e == realBin {
			found = true
		}
	}
	if !found {
		if runtime.GOOS == "darwin" {
			t.Skip("scan cannot see child executables here (macOS ps comm truncation)")
		}
		t.Fatalf("a live, ready child running %s is invisible to the scan: %v", bin, exes)
	}
	// 1.2.0 current, 1.1.0 previous, 1.0.0 running.
	removed, _, err := m.GC("claude")
	if err != nil || len(removed) != 0 {
		t.Fatalf("GC removed %v, %v; want nothing (1.0.0 is running)", removed, err)
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	removed, _, err = m.GC("claude")
	if err != nil || len(removed) != 1 || removed[0] != "1.0.0" {
		t.Fatalf("after exit GC removed %v, %v", removed, err)
	}
}
