package procstat

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixture tree (testdata/proc) models one host:
//
//	1   init
//	├─ 100 amux daemon ──┬─ 101 git fetch            (self child)
//	│                     ├─ 200 bwrap --bind …/sessions/771951/374179
//	│                     │   └─ 201 claude (cwd in sandbox)
//	│                     │       ├─ 202 qemu-system-x86 (cwd /: inherits)
//	│                     │       └─ 204 amux agent done (session, not self)
//	│                     └─ 300 claude (cwd …/sessions/771951/coordinator)
//	├─ 203 java gradle daemon (reparented to init; cwd in sandbox)
//	├─ 400 node (cwd …/workspaces/555555/web)
//	├─ 500 amux provide, 501 amux (tui), 502 amux status --json
//	├─ 700 comm with spaces and parens
//	├─ 600 no stat (unreadable), 601 malformed stat
//	2   kthreadd (no cmdline/cwd)
const (
	fixSessions   = "/data/amux/sessions"
	fixWorkspaces = "/data/amux/workspaces"
)

// fixture copies testdata/proc into a temp dir so tests can mutate it.
func fixture(t *testing.T) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "proc")
	err := filepath.WalkDir("testdata/proc", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel("testdata/proc", path)
		out := filepath.Join(dst, rel)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(target, out)
		case d.IsDir():
			return os.MkdirAll(out, 0o755)
		default:
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(out, b, 0o644)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestSampler(root string, c *clock) *Sampler {
	return New(Config{
		Root: root, Interval: 3 * time.Second, TopN: 3,
		SessionsDir: fixSessions, WorkspacesDir: fixWorkspaces,
		PageSize: 4096, CPUs: 32, Now: c.now,
	})
}

func writeStat(t *testing.T, root string, pid int, comm string, ppid int, ticks, threads, start, rss uint64) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fields := []string{fmt.Sprint(pid), "(" + comm + ")", "S", fmt.Sprint(ppid)}
	for i := 0; i < 9; i++ {
		fields = append(fields, "0")
	}
	fields = append(fields, fmt.Sprint(ticks), "0", "0", "0", "20", "0", fmt.Sprint(threads), "0",
		fmt.Sprint(start), "1000000", fmt.Sprint(rss), "0")
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(strings.Join(fields, " ")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// addTicks bumps a fixture process's utime by n ticks, keeping the rest.
func addTicks(t *testing.T, root string, pid int, n uint64) {
	t.Helper()
	path := filepath.Join(root, fmt.Sprint(pid), "stat")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := parseStat(b)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite with utime=ticks+n, stime=0 (the sum is what is sampled).
	writeStat(t, root, pid, st.comm, st.ppid, st.ticks+n, uint64(st.threads), st.start, st.rssPages)
}

func setUptime(t *testing.T, root string, secs float64) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "uptime"), []byte(fmt.Sprintf("%.2f 0.00\n", secs)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func session(snap Snapshot, id string) *SessionUsage {
	for i := range snap.Sessions {
		if snap.Sessions[i].ID == id {
			return &snap.Sessions[i]
		}
	}
	return nil
}

func workgroup(snap Snapshot, id string) *WorkgroupUsage {
	for i := range snap.Workgroups {
		if snap.Workgroups[i].ID == id {
			return &snap.Workgroups[i]
		}
	}
	return nil
}

func pids(ps []Process) []int {
	out := make([]int, len(ps))
	for i, p := range ps {
		out[i] = p.PID
	}
	return out
}

func TestAttribution(t *testing.T) {
	c := &clock{t: time.Unix(1000, 0)}
	snap, err := newTestSampler("testdata/proc", c).Sample()
	if err != nil {
		t.Fatal(err)
	}
	agent := session(snap, "374179")
	if agent == nil || agent.WorkgroupID != "771951" {
		t.Fatalf("agent session = %+v", agent)
	}
	// bwrap (cmdline), claude (cwd), qemu (inherits), gradle (cwd, despite
	// being reparented to init), and the agent's own amux hook.
	if agent.Processes != 5 {
		t.Fatalf("agent processes = %d, want 5 (top %v)", agent.Processes, pids(agent.Top))
	}
	wantRSS := uint64(300+90000+1310720+250000+3000) * 4096
	if agent.RSSBytes != wantRSS {
		t.Fatalf("agent rss = %d, want %d", agent.RSSBytes, wantRSS)
	}
	if len(agent.Top) != 3 || agent.Top[0].PID != 202 {
		t.Fatalf("agent top = %v, want qemu first and 3 entries", pids(agent.Top))
	}
	qemu := agent.Top[0]
	if qemu.Comm != "qemu-system-x86" || qemu.Cmd != "qemu-system-x86_64 -avd Pixel" || qemu.PSSBytes != 5000000*1024 || qemu.Threads != 40 {
		t.Fatalf("qemu = %+v", qemu)
	}

	coord := session(snap, "771951")
	if coord == nil || coord.WorkgroupID != "771951" || coord.Processes != 1 {
		t.Fatalf("coordinator session = %+v", coord)
	}
	ws := session(snap, "555555")
	if ws == nil || ws.WorkgroupID != "555555" || ws.Processes != 1 {
		t.Fatalf("workspace-level session = %+v", ws)
	}
	wg := workgroup(snap, "771951")
	if wg == nil || wg.Sessions != 2 || wg.Processes != 6 || wg.RSSBytes != wantRSS+50000*4096 {
		t.Fatalf("workgroup rollup = %+v", wg)
	}

	roles := map[int]string{}
	for _, p := range snap.Self.Procs {
		roles[p.PID] = p.Role
	}
	want := map[int]string{100: RoleDaemon, 500: RoleProvide, 501: RoleTUI, 502: RoleCLI, 101: RoleChild}
	if fmt.Sprint(roles) != fmt.Sprint(want) {
		t.Fatalf("self roles = %v, want %v", roles, want)
	}
	if snap.Self.Processes != 5 {
		t.Fatalf("self processes = %d, want 5", snap.Self.Processes)
	}
	// init, kthreadd, tmux. The unreadable (600) and malformed (601) entries
	// are skipped entirely.
	if snap.Unattributed.Processes != 3 || snap.Total.Processes != 15 {
		t.Fatalf("unattributed=%d total=%d", snap.Unattributed.Processes, snap.Total.Processes)
	}
	if snap.MemTotalBytes != 65536000*1024 || snap.CPUs != 32 {
		t.Fatalf("host = %d bytes, %d cpus", snap.MemTotalBytes, snap.CPUs)
	}
	// First sample: no window, so no CPU.
	if snap.WindowMS != 0 || snap.Total.CPUPercent != 0 {
		t.Fatalf("first sample window=%d cpu=%v", snap.WindowMS, snap.Total.CPUPercent)
	}
}

func TestCPUDelta(t *testing.T) {
	root := fixture(t)
	c := &clock{t: time.Unix(1000, 0)}
	s := newTestSampler(root, c)
	if _, err := s.Sample(); err != nil {
		t.Fatal(err)
	}
	// 2 seconds later qemu used 2.2 cores (440 ticks), the daemon 1.5 cores.
	c.t = c.t.Add(2 * time.Second)
	setUptime(t, root, 1002)
	addTicks(t, root, 202, 440)
	addTicks(t, root, 100, 300)
	addTicks(t, root, 101, 10)
	snap, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if snap.WindowMS != 2000 {
		t.Fatalf("window = %d", snap.WindowMS)
	}
	agent := session(snap, "374179")
	if agent.CPUPercent != 220 || agent.Top[0].CPUPercent != 220 {
		t.Fatalf("agent cpu = %v top=%+v", agent.CPUPercent, agent.Top[0])
	}
	if wg := workgroup(snap, "771951"); wg.CPUPercent != 220 {
		t.Fatalf("workgroup cpu = %v", wg.CPUPercent)
	}
	if snap.Self.CPUPercent != 155 {
		t.Fatalf("self cpu = %v, want daemon 150 + git 5", snap.Self.CPUPercent)
	}
	if snap.Self.Procs[0].PID != 100 || snap.Self.Procs[0].CPUPercent != 150 {
		t.Fatalf("self procs = %+v", snap.Self.Procs)
	}
	if snap.Total.CPUPercent != 375 {
		t.Fatalf("total cpu = %v", snap.Total.CPUPercent)
	}
	if snap.Sessions[0].ID != "374179" {
		t.Fatalf("sessions not sorted by cpu: %+v", snap.Sessions)
	}
	// Latest serves the cached copy.
	if got := s.Latest(); got.SampledAt != snap.SampledAt || got.Total.CPUPercent != 375 {
		t.Fatalf("latest = %+v", got.Total)
	}
}

func TestVanishedAndNewProcesses(t *testing.T) {
	root := fixture(t)
	c := &clock{t: time.Unix(1000, 0)}
	s := newTestSampler(root, c)
	if _, err := s.Sample(); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Second)
	setUptime(t, root, 1001)
	// qemu exits; its child-free parent stays.
	if err := os.RemoveAll(filepath.Join(root, "202")); err != nil {
		t.Fatal(err)
	}
	// A new compiler starts under claude after the last sample (start tick
	// 100050 > 100000) and has burned 50 ticks: all inside this window.
	writeStat(t, root, 900, "cc1", 201, 50, 1, 100050, 100)
	// A process merely unseen before (start long ago) has no baseline.
	writeStat(t, root, 901, "sleep", 201, 5000, 1, 10, 100)
	// The stat of a process that vanishes mid-scan becomes unreadable.
	if err := os.Remove(filepath.Join(root, "300", "stat")); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	agent := session(snap, "374179")
	if agent.Processes != 6 { // 5 - qemu + cc1 + sleep
		t.Fatalf("agent processes = %d", agent.Processes)
	}
	if agent.CPUPercent != 50 || agent.Top[0].PID != 900 {
		t.Fatalf("agent cpu = %v top = %+v", agent.CPUPercent, agent.Top)
	}
	if session(snap, "771951") != nil {
		t.Fatalf("coordinator with unreadable stat should drop out: %+v", snap.Sessions)
	}
	if wg := workgroup(snap, "771951"); wg.Sessions != 1 {
		t.Fatalf("workgroup = %+v", wg)
	}
	if _, ok := s.meta[procKey{202, 2000}]; ok {
		t.Fatal("vanished process metadata retained")
	}
}

func TestPIDReuse(t *testing.T) {
	root := fixture(t)
	c := &clock{t: time.Unix(1000, 0)}
	s := newTestSampler(root, c)
	if _, err := s.Sample(); err != nil {
		t.Fatal(err)
	}
	c.t = c.t.Add(time.Second)
	setUptime(t, root, 1001)
	// pid 202 (qemu, 110000 ticks) exits and the pid is reused by an unrelated
	// short-lived process under init with only 30 ticks. A pid-keyed delta
	// would underflow or reuse qemu's baseline; it must count 30 ticks, and
	// read the new process's cmdline/cwd rather than qemu's cached ones.
	writeStat(t, root, 202, "make", 1, 30, 1, 100020, 10)
	if err := os.WriteFile(filepath.Join(root, "202", "cmdline"), []byte("make\x00all\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "202", "cwd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/home/u/src", filepath.Join(root, "202", "cwd")); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if agent := session(snap, "374179"); agent.Processes != 4 || agent.CPUPercent != 0 {
		t.Fatalf("agent kept the reused pid: %+v", agent)
	}
	if snap.Unattributed.Processes != 4 || snap.Unattributed.CPUPercent != 30 {
		t.Fatalf("unattributed = %+v", snap.Unattributed)
	}
}

func TestCycleAndDeepTree(t *testing.T) {
	root := t.TempDir()
	setUptime(t, root, 10)
	// A ppid cycle (possible only from a torn read) must terminate.
	writeStat(t, root, 10, "a", 11, 0, 1, 1, 1)
	writeStat(t, root, 11, "b", 10, 0, 1, 1, 1)
	// A 300-deep chain under a sandbox claim.
	if err := os.MkdirAll(filepath.Join(root, "20"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStat(t, root, 20, "claude", 1, 0, 1, 1, 1)
	if err := os.Symlink(fixSessions+"/w/a", filepath.Join(root, "20", "cwd")); err != nil {
		t.Fatal(err)
	}
	for i := 21; i < 320; i++ {
		writeStat(t, root, i, "sh", i-1, 0, 1, 1, 1)
	}
	snap, err := newTestSampler(root, &clock{t: time.Unix(10, 0)}).Sample()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Total.Processes != 302 || snap.Unattributed.Processes != 2 {
		t.Fatalf("total=%d unattributed=%d", snap.Total.Processes, snap.Unattributed.Processes)
	}
	if a := session(snap, "a"); a == nil || a.Processes != 300 {
		t.Fatalf("deep chain = %+v", a)
	}
}

func TestMissingProcfs(t *testing.T) {
	s := New(Config{Root: filepath.Join(t.TempDir(), "nope")})
	if _, err := s.Sample(); err == nil {
		t.Fatal("expected error")
	}
	if s.Latest().Error == "" {
		t.Fatal("latest should carry an error before any sample")
	}
}

func TestParseStat(t *testing.T) {
	st, err := parseStat([]byte("700 (tmux: (srv) )) S 1 0 0 0 0 0 0 0 0 0 20 20 0 0 20 0 1 0 950 1000000 1000 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if st.comm != "tmux: (srv) )" || st.ppid != 1 || st.ticks != 40 || st.start != 950 || st.rssPages != 1000 {
		t.Fatalf("parsed %+v", st)
	}
	for _, bad := range []string{"", "12", "12 (x", "12 (x) S 1", "x (y) S 1 0 0 0 0 0 0 0 0 0 1 1 0 0 0 0 1 0 1 1 1"} {
		if _, err := parseStat([]byte(bad)); err == nil {
			t.Errorf("parseStat(%q) accepted", bad)
		}
	}
}

func TestPathClaim(t *testing.T) {
	a := newAttributor(fixSessions, fixWorkspaces, []string{"amux"})
	cases := []struct {
		in       string
		ok       bool
		sess, wg string
	}{
		{fixSessions + "/w1/a1", true, "a1", "w1"},
		{fixSessions + "/w1/a1/repo/sub", true, "a1", "w1"},
		{fixSessions + "/w1", true, "w1", "w1"},
		{fixSessions + "/w1/coordinator", true, "w1", "w1"},
		{fixWorkspaces + "/w2/repo", true, "w2", "w2"},
		{"bwrap --bind " + fixSessions + "/w1/a1 " + fixSessions + "/w1/a1 --", true, "a1", "w1"},
		{"x --chdir=" + fixWorkspaces + "/w3:y " + fixSessions + "/w9/a9", true, "w3", "w3"},
		{fixSessions + "/", false, "", ""},
		{fixSessions, false, "", ""},
		{"/data/other/sessions/w/a", false, "", ""},
	}
	for _, c := range cases {
		got, ok := a.pathClaim(c.in)
		if ok != c.ok || got.session != c.sess || got.workgroup != c.wg {
			t.Errorf("pathClaim(%q) = %+v,%v want %s/%s,%v", c.in, got, ok, c.wg, c.sess, c.ok)
		}
	}
}

func TestSelfRole(t *testing.T) {
	cases := map[string]string{
		"amux":                     RoleTUI,
		"amux --debug":             RoleTUI,
		"amux daemon":              RoleDaemon,
		"amux daemon restart":      RoleCLI,
		"amux provide":             RoleProvide,
		"amux provide host:1":      RoleProvide,
		"amux provide install":     RoleCLI,
		"amux serve":               RoleServe,
		"amux workgroup open abc1": RoleTUI,
		"amux wg ls":               RoleCLI,
		"amux status --json":       RoleCLI,
	}
	for in, want := range cases {
		if got := selfRole(strings.Fields(in)); got != want {
			t.Errorf("selfRole(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestParseInterval(t *testing.T) {
	cases := map[string]time.Duration{
		"": DefaultInterval, "2s": 2 * time.Second, "5": 5 * time.Second,
		"off": 0, "0": 0, "100ms": time.Second, "10m": time.Minute,
	}
	for in, want := range cases {
		got, err := ParseInterval(in)
		if err != nil || got != want {
			t.Errorf("ParseInterval(%q) = %v, %v want %v", in, got, err, want)
		}
	}
	if _, err := ParseInterval("soon"); err == nil {
		t.Error("ParseInterval(soon) accepted")
	}
}

func TestShortCmd(t *testing.T) {
	long := strings.Repeat("é", 200)
	got := shortCmd([]string{"/usr/bin/x", long})
	if !strings.HasSuffix(got, "…") || len(got) > cmdMax+len("…") || !strings.HasPrefix(got, "x é") {
		t.Fatalf("shortCmd = %q", got)
	}
	if !strings.Contains(got[:len(got)-len("…")], "é") || strings.ContainsRune(got, '�') {
		t.Fatalf("cut mid-rune: %q", got)
	}
}
