package harnessbin

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// A real running process executing from a version dir protects it from GC.
func TestGCSparesVersionWithLiveProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("process scan is unix-only")
	}
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	m, _ := newTestManager(t)
	m.InUse = nil // the real scan
	for _, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		mustInstall(t, m, v)
	}
	// Run a copy of sleep as 1.0.0's binary (a native executable, so the
	// kernel reports it as the process's exe).
	bin := m.BinPath("claude", "1.0.0")
	b, err := os.ReadFile(sleep)
	if err != nil {
		t.Skip(err)
	}
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "30")
	// argv[0] stays "sleep": a multi-call coreutils (uutils on newer Ubuntu
	// runners) dispatches on argv[0], and named "claude" it would exit at once
	// — possibly after the readiness scan below but before GC. The kernel's
	// exe is still 1.0.0's binary, which is what the scan must see.
	cmd.Args[0] = "sleep"
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot exec copied sleep: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		exes, err := runningExecutables()
		if err != nil {
			t.Skipf("process scan unavailable: %v", err)
		}
		found := false
		realBin, _ := filepath.EvalSymlinks(bin)
		for _, e := range exes {
			if e == bin || e == realBin {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Skip("scan cannot see child executables here (e.g. macOS ps comm truncation)")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The child must still be running when GC scans (not merely seen once).
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("copied sleep exited before GC: %v", err)
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
