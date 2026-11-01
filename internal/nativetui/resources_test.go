package nativetui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/procstat"
)

const mib = 1024 * 1024

func testSnapshot(at int64) procstat.Snapshot {
	return procstat.Snapshot{
		SampledAt: at, IntervalMS: 2000, WindowMS: 2000, CPUs: 16, MemTotalBytes: 64 * 1024 * mib,
		Self: procstat.SelfUsage{
			Usage: procstat.Usage{CPUPercent: 47.5, RSSBytes: 180 * mib, Processes: 4},
			Procs: []procstat.Process{
				{PID: 10, Comm: "amux", Role: procstat.RoleDaemon, CPUPercent: 1.2, RSSBytes: 80 * mib},
				{PID: 11, Comm: "amux", Role: procstat.RoleProvide, CPUPercent: 0.3, RSSBytes: 40 * mib},
				{PID: 12, Comm: "amux", Role: procstat.RoleTUI, CPUPercent: 45, RSSBytes: 60 * mib},
			},
			Sampler: procstat.SamplerCost{CPUPercent: 0.4},
		},
		Sessions: []procstat.SessionUsage{
			{ID: "ag1", WorkgroupID: "wg1", Usage: procstat.Usage{CPUPercent: 250, RSSBytes: 2 * 1024 * mib, Processes: 9},
				Top: []procstat.Process{
					{PID: 100, Comm: "java", Cmd: "gradle daemon", CPUPercent: 200, RSSBytes: 1500 * mib},
					{PID: 101, Comm: "node", CPUPercent: 40, RSSBytes: 300 * mib},
					{PID: 102, Comm: "git", CPUPercent: 5, RSSBytes: 10 * mib},
					{PID: 103, Comm: "sh", CPUPercent: 1, RSSBytes: 1 * mib},
				}},
			{ID: "ag2", WorkgroupID: "wg1", Usage: procstat.Usage{CPUPercent: 3, RSSBytes: 6 * 1024 * mib, Processes: 3},
				Top: []procstat.Process{{PID: 200, Comm: "qemu-system-x86", CPUPercent: 3, RSSBytes: 6 * 1024 * mib}}},
		},
		Workgroups: []procstat.WorkgroupUsage{
			{ID: "wg1", Usage: procstat.Usage{CPUPercent: 253, RSSBytes: 8 * 1024 * mib, Processes: 12}, Sessions: 2},
		},
		Total: procstat.Usage{CPUPercent: 400, RSSBytes: 20 * 1024 * mib},
	}
}

func resourcesFrame(t *testing.T, s procstat.Snapshot) frameMsg {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return frameMsg{daemon.Frame{Data: &core.Data{Type: core.FrameData, Query: core.QueryResources, OK: true, Rows: b}}}
}

func resourcesModel() *model {
	return &model{w: 120, h: 30, sessions: []core.Session{
		{ID: "wg1", Title: "payments", IsRoot: true, Section: core.SectionWorkgroups},
		{ID: "ag1", Title: "build", RootID: "wg1", Section: core.SectionWorkgroups},
		{ID: "ag2", Title: "emulator", RootID: "wg1", Section: core.SectionWorkgroups},
		{ID: "ag3", Title: "idle-agent-with-a-long-title", RootID: "wg1", Section: core.SectionWorkgroups},
	}, cursor: 3}
}

func TestMetricCellsAreFixedWidth(t *testing.T) {
	for _, p := range []float64{0, 0.04, 3.21, 9.94, 9.96, 45, 99.6, 999.4, 999.6, 1600, 9949, 9951, 64000} {
		if got := cpuCell(p, true); len(got) != 4 {
			t.Errorf("cpuCell(%v) = %q, want 4 cells", p, got)
		}
	}
	for _, b := range []uint64{0, 512, 1023 * 1024, 5 * mib, 10443000, 999 * mib, 1000 * mib, 3 * 1024 * mib, 1 << 50} {
		if got := memCell(b, true); len(got) != 4 {
			t.Errorf("memCell(%v) = %q, want 4 cells", b, got)
		}
	}
	if got := metricCell(procstat.Usage{CPUPercent: 250, RSSBytes: 2 * 1024 * mib}); got != "250% 2.0G" {
		t.Fatalf("metricCell = %q", got)
	}
}

// Rail rows gain a fixed CPU/MEM column once samples flow; workgroup rows show
// the workgroup roll-up and rows without processes keep the column blank so
// truncation never changes as processes come and go.
func TestRailRowsShowResourceColumns(t *testing.T) {
	m := resourcesModel()
	before := ansi.Strip(m.renderRow(1, m.sessions[1]))
	if strings.Contains(before, "%") {
		t.Fatalf("no columns expected before a sample: %q", before)
	}
	m.Update(resourcesFrame(t, testSnapshot(1)))
	for i, want := range map[int]string{0: "253% 8.0G", 1: "250% 2.0G", 2: "3.0% 6.0G"} {
		row := m.renderRow(i, m.sessions[i])
		first := strings.Split(row, "\n")[0]
		if w := lipgloss.Width(first); w != sidebarWidth {
			t.Errorf("row %d width %d, want %d: %q", i, w, sidebarWidth, ansi.Strip(first))
		}
		if !strings.HasSuffix(ansi.Strip(first), want) {
			t.Errorf("row %d = %q, want suffix %q", i, ansi.Strip(first), want)
		}
	}
	// The selected idle row reserves the column (blank) and truncates its title.
	sel := ansi.Strip(strings.Split(m.renderRow(3, m.sessions[3]), "\n")[0])
	if len([]rune(sel)) != sidebarWidth || !strings.HasSuffix(sel, strings.Repeat(" ", metricWidth)) || !strings.Contains(sel, "…") {
		t.Fatalf("idle row = %q", sel)
	}
	top := ansi.Strip(m.renderTopBorder())
	if !strings.Contains(top, " amux  48% 180M") {
		t.Fatalf("top border lacks amux overhead: %q", top)
	}
}

// A reply carrying the sample already cached changes nothing (the panel cache
// stays valid); a new sample rebuilds.
func TestResourcesReplyDedupesSamples(t *testing.T) {
	m := resourcesModel()
	m.Update(resourcesFrame(t, testSnapshot(1)))
	m.res.open = true
	first := m.renderResources()
	snap := m.res.snap
	m.Update(resourcesFrame(t, testSnapshot(1)))
	if m.res.snap != snap {
		t.Fatal("unchanged sample was re-applied")
	}
	if m.renderResources() != first {
		t.Fatal("panel re-rendered without a data change")
	}
	next := testSnapshot(2)
	next.Sessions[0].CPUPercent = 12
	m.Update(resourcesFrame(t, next))
	if m.res.snap == snap || m.renderResources() == first {
		t.Fatal("new sample not applied")
	}
}

func TestResourcesPanel(t *testing.T) {
	m := resourcesModel()
	m.Update(resourcesFrame(t, testSnapshot(1)))
	m.handleKey(key("t"))
	if !m.res.open {
		t.Fatal("t did not open the panel")
	}
	out := ansi.Strip(m.renderMain())
	for _, want := range []string{
		"Resources · by CPU",
		"amux 48% 180M · daemon 1.2% 80M · provide 0.3% 40M · tui 45% 60M · sampler 0.4%",
		"host 400% of 16 CPUs",
		"CPU%    MEM  PROCS  SESSION",
		"250%   2.0G      9  wg1/ag1 · build",
		"├ java 100  gradle daemon",
		"├ node 101",
		"└ git 102",
		"qemu-system-x86 200",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("panel lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sh 103") {
		t.Errorf("panel lists more than %d processes per session", resourcesTopProcs)
	}
	order := func() (int, int) {
		o := ansi.Strip(m.renderMain())
		return strings.Index(o, "wg1/ag1"), strings.Index(o, "wg1/ag2")
	}
	if a, b := order(); a < 0 || b < 0 || a > b {
		t.Fatalf("CPU sort: ag1 at %d, ag2 at %d", a, b)
	}
	m.handleKey(key("s"))
	if a, b := order(); a < 0 || b < 0 || b > a {
		t.Fatalf("memory sort: ag1 at %d, ag2 at %d", a, b)
	}
	if !strings.Contains(ansi.Strip(m.renderMain()), "by memory") {
		t.Fatal("title does not show memory sort")
	}
	// Panel keys stay in the panel: q closes it, it does not quit.
	_, cmd := m.handleKey(key("q"))
	if m.res.open || cmd != nil {
		t.Fatalf("q should only close the panel (open=%v)", m.res.open)
	}
}

func TestResourcesPanelBeforeFirstSample(t *testing.T) {
	m := resourcesModel()
	m.res.open = true
	if out := ansi.Strip(m.renderMain()); !strings.Contains(out, "waiting for the daemon") {
		t.Fatalf("panel = %q", out)
	}
	m.Update(frameMsg{daemon.Frame{Data: &core.Data{Query: core.QueryResources, Error: "unknown query"}}})
	if out := ansi.Strip(m.renderMain()); !strings.Contains(out, "unavailable") {
		t.Fatalf("panel = %q", out)
	}
	if m.res.interval() != resourcesBackoff {
		t.Fatalf("interval %v, want backoff", m.res.interval())
	}
	if out := ansi.Strip(m.renderMain()); !strings.Contains(out, "unavailable: unknown query") {
		t.Fatalf("panel lacks the reason: %q", out)
	}
	// A running sampler without its first sample is not a failure.
	m.Update(resourcesFrame(t, procstat.Snapshot{}))
	if m.res.unavailable || m.res.interval() != resourcesDefaultInterval {
		t.Fatalf("pending first sample treated as unavailable")
	}
	// An unsupported host backs off even without an error string.
	m.Update(resourcesFrame(t, procstat.Snapshot{Unsupported: true}))
	if !m.res.unavailable || m.res.interval() != resourcesBackoff {
		t.Fatal("unsupported host should back off")
	}
	// A disabled sampler reports why.
	m.Update(resourcesFrame(t, procstat.Snapshot{Error: "resource sampling disabled"}))
	if out := ansi.Strip(m.renderMain()); !m.res.unavailable || !strings.Contains(out, "sampling disabled") {
		t.Fatalf("panel = %q", out)
	}
	if strings.Contains(ansi.Strip(m.renderRow(1, m.sessions[1])), "%") {
		t.Fatal("no columns without samples")
	}
}

func TestResourcesIntervalFollowsSampler(t *testing.T) {
	for ms, want := range map[int64]time.Duration{0: resourcesDefaultInterval, 500: resourcesMinInterval, 4000: 4 * time.Second, 60000: resourcesMaxInterval} {
		s := testSnapshot(1)
		s.IntervalMS = ms
		r := resourcesState{snap: &s}
		if got := r.interval(); got != want {
			t.Errorf("IntervalMS %d: interval %v, want %v", ms, got, want)
		}
	}
}

// Polling rides the dashboard stream and each connection gets one chain: ticks
// from a retired connection stop.
func TestResourcesPollChain(t *testing.T) {
	c := &reconnectClient{}
	m := &model{dataCh: make(chan struct{}, 1)}
	m.client = c
	runSends(m.startResources())
	queries := func() int {
		n := 0
		for _, a := range c.recorded() {
			if a.Action == core.ActionQuery && a.Query == core.QueryResources {
				n++
			}
		}
		return n
	}
	if queries() != 1 {
		t.Fatalf("queries = %d, want 1", queries())
	}
	serial := m.res.serial
	_, cmd := m.Update(resourcesTickMsg{serial})
	runSends(cmd)
	if queries() != 2 {
		t.Fatalf("tick did not poll: %d", queries())
	}
	m.Update(disconnectedMsg{})
	m.client = c
	if _, cmd := m.Update(resourcesTickMsg{serial}); cmd != nil {
		t.Fatal("a stale tick kept polling after disconnect")
	}
}

// runSends runs a poll command's immediate parts, skipping the timer.
func runSends(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, child := range batch {
				if child == nil {
					continue
				}
				ch := make(chan tea.Msg, 1)
				go func() { ch <- child() }()
				select {
				case <-ch:
				case <-time.After(100 * time.Millisecond): // the tick
				}
			}
		}
	case <-time.After(100 * time.Millisecond):
	}
}
