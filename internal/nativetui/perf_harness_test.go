package nativetui

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"amux/internal/core"
	"amux/internal/daemon"
	"amux/internal/vterm"
)

// TestTUICPUHarness measures the dashboard's CPU under a synthetic daemon: a
// 30-session rail, one attached agent pane streaming a redraw every 20ms, a
// snapshot every second and (when the TUI asks) a resource sample every 2s.
// It runs the real Bubble Tea program loop and renderer, writing to
// io.Discard, and reports process CPU over the window. Skipped unless
// AMUX_TUI_PERF is set; AMUX_TUI_PERF=panel also opens the resources panel.
//
//	AMUX_TUI_PERF=1     go test ./internal/nativetui -run TestTUICPUHarness -v
//	AMUX_TUI_PERF=panel go test ./internal/nativetui -run TestTUICPUHarness -v
//
// It deliberately uses no resources-specific identifiers so the same file
// runs on older trees for a baseline.
func TestTUICPUHarness(t *testing.T) {
	mode := os.Getenv("AMUX_TUI_PERF")
	if mode == "" {
		t.Skip("set AMUX_TUI_PERF=1 (or panel) to measure TUI CPU")
	}
	window := 20 * time.Second
	if s := os.Getenv("AMUX_TUI_PERF_SECONDS"); s != "" {
		n, _ := strconv.Atoi(s)
		window = time.Duration(n) * time.Second
	}
	c := newPerfClient()
	defer c.Close()
	m := &model{
		terms:  map[paneKey]*vterm.Terminal{},
		byPane: map[string]paneKey{},
		dataCh: make(chan struct{}, 1),
	}
	p := tea.NewProgram(perfModel{m}, tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutSignals())
	go func() {
		p.Send(tea.WindowSizeMsg{Width: 200, Height: 50})
		p.Send(connectedMsg{c})
	}()
	go func() {
		// Attach the first agent, then hand focus back to the rail.
		time.Sleep(time.Second)
		for i := 0; i < 3; i++ {
			p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
		}
		p.Send(tea.KeyMsg{Type: tea.KeyEnter})
		time.Sleep(300 * time.Millisecond)
		p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h"), Alt: true})
		if mode == "panel" {
			p.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
		}
		time.Sleep(2 * time.Second) // warm up
		before := cpuTime()
		start := time.Now()
		time.Sleep(window)
		used := cpuTime() - before
		pct := 100 * used.Seconds() / time.Since(start).Seconds()
		c.mu.Lock()
		pane := c.pane
		c.mu.Unlock()
		fmt.Printf("TUI CPU harness (mode=%s): %.1f%% of one core over %s (%s CPU), pane %q, %d resource queries\n",
			mode, pct, window, used.Round(time.Millisecond), pane, c.queries())
		p.Quit()
	}()
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}
}

// perfModel skips Init's dial of the real daemon; the harness injects its
// fake connection instead.
type perfModel struct{ *model }

func (p perfModel) Init() tea.Cmd { return waitData(p.dataCh) }
func (p perfModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := p.model.Update(msg)
	return p, cmd
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

type perfClient struct {
	frames chan daemon.Frame
	done   chan struct{}
	once   sync.Once
	mu     sync.Mutex
	nq     int
	pane   string
}

func newPerfClient() *perfClient {
	c := &perfClient{frames: make(chan daemon.Frame, 64), done: make(chan struct{})}
	go c.snapshots()
	return c
}

func (c *perfClient) sessions() []core.Session {
	var out []core.Session
	for g := 0; g < 5; g++ {
		wg := fmt.Sprintf("wg%d", g)
		out = append(out, core.Session{ID: wg, Title: "workgroup " + wg, IsRoot: true, Section: core.SectionWorkgroups})
		for a := 0; a < 5; a++ {
			out = append(out, core.Session{ID: fmt.Sprintf("%s-ag%d", wg, a), Title: fmt.Sprintf("agent task %d of %s", a, wg), RootID: wg,
				Section: core.SectionWorkgroups, State: core.StateRunning, Status: "working · 3m"})
		}
	}
	return out
}

func (c *perfClient) snapshots() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	out := time.NewTicker(20 * time.Millisecond)
	defer out.Stop()
	n := 0
	c.push(daemon.Frame{Snapshot: &core.Snapshot{Sessions: c.sessions()}})
	for {
		select {
		case <-c.done:
			return
		case <-tick.C:
			c.push(daemon.Frame{Snapshot: &core.Snapshot{Sessions: c.sessions()}})
		case <-out.C:
			c.mu.Lock()
			pane := c.pane
			c.mu.Unlock()
			if pane == "" {
				continue
			}
			n++
			// A spinner/status redraw like a working agent's: move, clear, repaint.
			data := fmt.Sprintf("\x1b[40;1H\x1b[2K\x1b[38;5;114m%c Working… (%ds · esc to interrupt)\x1b[0m\r\n\x1b[2Kline %d %s", "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏"[n%10], n/50, n, "lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor")
			c.push(daemon.Frame{Pane: &core.PaneFrame{Type: core.FramePaneOutput, PaneID: pane, Data: []byte(data)}})
		}
	}
}

func (c *perfClient) push(f daemon.Frame) {
	select {
	case c.frames <- f:
	case <-c.done:
	}
}

func (c *perfClient) queries() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nq
}

func (c *perfClient) Next() (daemon.Frame, error) {
	select {
	case f := <-c.frames:
		return f, nil
	case <-c.done:
		return daemon.Frame{}, io.EOF
	}
}

func (c *perfClient) Send(a core.Action) error {
	if a.Action == core.ActionQuery && a.Query == "resources" {
		c.mu.Lock()
		c.nq++
		n := c.nq
		c.mu.Unlock()
		go c.push(daemon.Frame{Data: &core.Data{Type: core.FrameData, Query: a.Query, OK: true, Rows: perfResources(int64(n))}})
	}
	return nil
}

// perfResources is a QueryResources reply in its JSON wire shape.
func perfResources(at int64) []byte {
	s := fmt.Sprintf(`{"sampledAt":%d,"intervalMs":2000,"windowMs":2000,"cpus":16,"memTotalBytes":68719476736,`+
		`"self":{"cpuPercent":46,"rssBytes":188743680,"processes":3,"threads":40,"procs":[`+
		`{"pid":1,"ppid":0,"comm":"amux","role":"daemon","cpuPercent":1.2,"rssBytes":83886080,"threads":12},`+
		`{"pid":2,"ppid":0,"comm":"amux","role":"provide","cpuPercent":0.3,"rssBytes":41943040,"threads":10},`+
		`{"pid":3,"ppid":0,"comm":"amux","role":"tui","cpuPercent":44,"rssBytes":62914560,"threads":18}],`+
		`"sampler":{"cpuMs":8,"wallMs":12,"cpuPercent":0.4,"scanned":600}},"sessions":[`, at)
	wgs := ""
	for g := 0; g < 5; g++ {
		for a := 0; a < 5; a++ {
			if g+a > 0 {
				s += ","
			}
			s += fmt.Sprintf(`{"id":"wg%d-ag%d","workgroupId":"wg%d","cpuPercent":%d,"rssBytes":%d,"processes":6,"threads":30,"top":[`+
				`{"pid":%d,"ppid":1,"comm":"node","cmd":"node claude","cpuPercent":12,"rssBytes":300000000,"threads":11},`+
				`{"pid":%d,"ppid":1,"comm":"java","cmd":"gradle daemon","cpuPercent":%d,"rssBytes":1500000000,"threads":60}]}`,
				g, a, g, (g*5+a)*7+int(at%5), (g*5+a+1)*200000000, 1000+g*10+a, 2000+g*10+a, (g*5+a)*5)
		}
		if g > 0 {
			wgs += ","
		}
		wgs += fmt.Sprintf(`{"id":"wg%d","cpuPercent":%d,"rssBytes":%d,"processes":30,"threads":150,"sessions":5}`, g, 100+g*40, (g+1)*3000000000)
	}
	s += `],"workgroups":[` + wgs + `],"unattributed":{"cpuPercent":20,"rssBytes":2000000000,"processes":200,"threads":800},` +
		`"total":{"cpuPercent":400,"rssBytes":20000000000,"processes":400,"threads":2000}}`
	return []byte(s)
}

func (c *perfClient) PaneOpen(paneID, _ string, _, _, _ int) error {
	c.mu.Lock()
	c.pane = paneID
	c.mu.Unlock()
	return nil
}
func (c *perfClient) PaneInput(string, []byte) error    { return nil }
func (c *perfClient) PaneResize(string, int, int) error { return nil }
func (c *perfClient) Close() error {
	c.once.Do(func() { close(c.done) })
	return nil
}

// BenchmarkDashboardView is the per-frame render cost of a 30-session rail,
// without resource data, with it (rail columns), and with the resources panel
// open (t). Like the harness it uses no resources identifiers, so it also runs
// on older trees, where the latter two cases match the first.
func BenchmarkDashboardView(b *testing.B) {
	c := newPerfClient()
	defer c.Close()
	for _, tc := range []struct {
		name            string
		resources, open bool
	}{{"rail", false, false}, {"rail+resources", true, false}, {"panel", true, true}} {
		b.Run(tc.name, func(b *testing.B) {
			m := &model{w: 200, h: 50, dataCh: make(chan struct{}, 1)}
			m.Update(frameMsg{daemon.Frame{Snapshot: &core.Snapshot{Sessions: c.sessions()}}})
			if tc.resources {
				m.Update(frameMsg{daemon.Frame{Data: &core.Data{Type: core.FrameData, Query: "resources", OK: true, Rows: perfResources(1)}}})
			}
			if tc.open {
				m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = m.View()
			}
		})
	}
}
