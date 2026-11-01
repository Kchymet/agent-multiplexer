package procstat

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

// spawnChildren starts AMUX_PROCSTAT_BENCH_PROCS idle children (default 0)
// so /proc approaches host scale, and kills exactly those on cleanup.
func spawnChildren(b *testing.B) {
	n, _ := strconv.Atoi(os.Getenv("AMUX_PROCSTAT_BENCH_PROCS"))
	var cmds []*exec.Cmd
	b.Cleanup(func() {
		for _, c := range cmds {
			_ = c.Process.Kill()
			_ = c.Wait()
		}
	})
	for i := 0; i < n; i++ {
		c := exec.Command("sleep", "300")
		if err := c.Start(); err != nil {
			b.Fatal(err)
		}
		cmds = append(cmds, c)
	}
}

// BenchmarkSampleProc samples the real /proc in steady state (cmdline/cwd
// cached, as between Run ticks). Per-process cost = ns/op / procs.
func BenchmarkSampleProc(b *testing.B) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		b.Skip("no procfs")
	}
	spawnChildren(b)
	s := New(Config{SessionsDir: "/nonexistent/sessions", WorkspacesDir: "/nonexistent/workspaces"})
	snap, _ := s.Sample()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if snap, _ = s.Sample(); snap.Total.Processes == 0 {
			b.Fatal("no processes")
		}
	}
	b.ReportMetric(float64(snap.Total.Processes), "procs")
}

// BenchmarkSampleProcCold is a sample whose metadata cache is empty (first
// sample, or every process new).
func BenchmarkSampleProcCold(b *testing.B) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		b.Skip("no procfs")
	}
	spawnChildren(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := New(Config{SessionsDir: "/nonexistent/sessions", Now: func() time.Time { return time.Unix(0, 0) }})
		if _, err := s.Sample(); err != nil {
			b.Fatal(err)
		}
	}
}
