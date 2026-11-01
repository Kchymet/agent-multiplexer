package nativetui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"amux/internal/core"
	"amux/internal/procstat"
)

// Resource figures come from the daemon's background sampler (procstat) via
// core.QueryResources; the TUI never reads /proc itself. The query rides the
// dashboard's existing stream (the reply is a Data frame the read loop already
// receives), so polling costs no extra connection. Each reply that carries a
// new sample rebuilds a small cache of pre-formatted cells; rendering only
// reads that cache, so a frame costs a map lookup per rail row.

const (
	// resourcesDefaultInterval is the poll period until the daemon reports its
	// sampler interval; polls then follow that interval, clamped to this range
	// so a misconfigured sampler can neither spin the TUI nor freeze the view.
	resourcesDefaultInterval = 3 * time.Second
	resourcesMinInterval     = 2 * time.Second
	resourcesMaxInterval     = 5 * time.Second
	// resourcesBackoff is the poll period while the daemon cannot answer (an
	// older daemon without the query, or a disabled sampler).
	resourcesBackoff = 30 * time.Second
	// resourcesTopProcs caps the child processes listed under a session.
	resourcesTopProcs = 3
	// metricWidth is the fixed rail cell: "CPU% MEM" in exactly 9 cells, plus
	// a separating space. It is reserved on every row once samples flow, so
	// rows never re-truncate as processes come and go.
	metricWidth = 10
)

type resourcesTickMsg struct{ serial uint64 }

// resourcesState is the TUI's cached view of the latest resource sample.
type resourcesState struct {
	serial      uint64             // poll-chain fence: bumped on (dis)connect
	snap        *procstat.Snapshot // latest sample with SampledAt != 0
	unavailable bool               // the last reply failed or sampling is off
	reason      string             // why, for the panel

	// Derived once per sample (rebuild):
	rows     map[string]metric // session id -> its rail cell
	groups   map[string]metric // workgroup id -> its rail cell
	self     string            // 9-cell amux overhead
	byCPU    []procstat.SessionUsage
	byMem    []procstat.SessionUsage
	overhead string // the panel's amux breakdown line (unstyled)

	// The top panel (t on the rail).
	open    bool
	sortMem bool // sort by RSS instead of CPU
	scroll  int
	cache   string
	cacheAt resourcesCacheKey
}

// metric is a rail cell, " CPU% MEM" (metricWidth cells), plain for the
// selected row and pre-styled for the others, so a frame never styles it.
type metric struct{ plain, styled string }

func newMetric(u procstat.Usage) metric {
	plain := " " + metricCell(u)
	return metric{plain, dimStyle.Render(plain)}
}

// blankMetric reserves the column on rows without attributed processes.
var blankMetric = metric{strings.Repeat(" ", metricWidth), strings.Repeat(" ", metricWidth)}

type resourcesCacheKey struct {
	sampledAt     int64
	sortMem       bool
	scroll, w, h  int
	sessions      int
	unavailable   bool
	snapshotValid bool
	reason        string
}

// active reports whether resource columns are shown: once a sample arrived
// they stay (with the last figures) through transient failures.
func (r *resourcesState) active() bool { return r.snap != nil }

// interval is the next poll delay.
func (r *resourcesState) interval() time.Duration {
	if r.unavailable && r.snap == nil {
		return resourcesBackoff
	}
	if r.snap == nil || r.snap.IntervalMS <= 0 {
		return resourcesDefaultInterval
	}
	d := time.Duration(r.snap.IntervalMS) * time.Millisecond
	return min(max(d, resourcesMinInterval), resourcesMaxInterval)
}

// startResources begins a fresh poll chain on a new connection; any chain from
// the previous connection is fenced off by the serial.
func (m *model) startResources() tea.Cmd {
	m.res.serial++
	return m.pollResources()
}

// pollResources sends one query on the dashboard stream and schedules the next.
// The chain does not wait for replies, so a lost reply never stalls it.
func (m *model) pollResources() tea.Cmd {
	c, serial := m.client, m.res.serial
	if c == nil {
		return nil
	}
	send := func() tea.Msg {
		_ = c.Send(core.Action{Action: core.ActionQuery, Query: core.QueryResources})
		return nil
	}
	tick := tea.Tick(m.res.interval(), func(time.Time) tea.Msg { return resourcesTickMsg{serial} })
	return tea.Batch(send, tick)
}

// applyResources folds a QueryResources reply into the cache. A reply carrying
// the sample already shown changes nothing.
func (m *model) applyResources(d *core.Data) {
	r := &m.res
	var snap procstat.Snapshot
	switch {
	case !d.OK:
		r.unavailable, r.reason = true, d.Error
		return
	case json.Unmarshal(d.Rows, &snap) != nil:
		r.unavailable, r.reason = true, "unreadable resource sample"
		return
	case snap.SampledAt == 0:
		// Sampling is off or unsupported (Error says why), or the sampler has
		// not completed its first sample yet (keep polling at the usual pace).
		r.unavailable, r.reason = snap.Error != "" || snap.Unsupported, snap.Error
		if snap.Unsupported && r.reason == "" {
			r.reason = "this host has no procfs"
		}
		return
	}
	r.unavailable, r.reason = false, ""
	if r.snap != nil && r.snap.SampledAt == snap.SampledAt && r.snap.Error == snap.Error {
		return
	}
	r.snap = &snap
	r.rebuild()
}

// rebuild derives every pre-formatted string from r.snap.
func (r *resourcesState) rebuild() {
	s := r.snap
	r.rows = make(map[string]metric, len(s.Sessions))
	r.groups = make(map[string]metric, len(s.Workgroups))
	for _, u := range s.Sessions {
		r.rows[u.ID] = newMetric(u.Usage)
	}
	for _, g := range s.Workgroups {
		r.groups[g.ID] = newMetric(g.Usage)
	}
	r.self = metricCell(s.Self.Usage)
	r.byCPU = append([]procstat.SessionUsage(nil), s.Sessions...)
	sort.SliceStable(r.byCPU, func(i, j int) bool { return r.byCPU[i].CPUPercent > r.byCPU[j].CPUPercent })
	r.byMem = append([]procstat.SessionUsage(nil), s.Sessions...)
	sort.SliceStable(r.byMem, func(i, j int) bool { return r.byMem[i].RSSBytes > r.byMem[j].RSSBytes })

	// Overhead: the amux processes by role, then the sampler's own cost.
	var parts []string
	for _, role := range []string{procstat.RoleDaemon, procstat.RoleProvide, procstat.RoleServe, procstat.RoleTUI, procstat.RoleCLI} {
		var u procstat.Usage
		n := 0
		for _, p := range s.Self.Procs {
			if p.Role == role {
				u.CPUPercent += p.CPUPercent
				u.RSSBytes += p.RSSBytes
				n++
			}
		}
		if n == 0 {
			continue
		}
		label := role
		if n > 1 {
			label = fmt.Sprintf("%s×%d", role, n)
		}
		parts = append(parts, label+" "+cpuCell(u.CPUPercent, false)+" "+memCell(u.RSSBytes, false))
	}
	parts = append(parts, "sampler "+cpuCell(s.Self.Sampler.CPUPercent, false))
	r.overhead = strings.Join(parts, " · ")
}

// metricCell is the fixed 9-cell "CPU% MEM" rail cell.
func metricCell(u procstat.Usage) string {
	return cpuCell(u.CPUPercent, true) + " " + memCell(u.RSSBytes, true)
}

// cpuCell formats percent-of-one-core. padded yields exactly 4 cells
// (" 3.2%"-style values are kept to "3.2%"; ≥1000% shows as "1.6k").
func cpuCell(p float64, padded bool) string {
	var s string
	switch {
	case p < 9.95:
		s = fmt.Sprintf("%.1f%%", max(p, 0))
	case p < 999.5:
		s = fmt.Sprintf("%.0f%%", p)
	case p < 9950:
		s = fmt.Sprintf("%.1fk", p/1000)
	default:
		s = fmt.Sprintf("%.0fk", p/1000)
	}
	if padded {
		s = fmt.Sprintf("%4s", s)
	}
	return s
}

// memCell formats bytes in binary units. padded yields exactly 4 cells.
func memCell(b uint64, padded bool) string {
	const k = 1024.0
	v, units := float64(b)/k, []string{"K", "M", "G", "T", "P", "E"}
	i := 0
	for v >= 999.5 && i < len(units)-1 {
		v /= k
		i++
	}
	var s string
	if v < 9.95 && i > 0 {
		s = fmt.Sprintf("%.1f%s", v, units[i])
	} else {
		s = fmt.Sprintf("%.0f%s", v, units[i])
	}
	if padded {
		s = fmt.Sprintf("%4s", s)
	}
	return s
}

// railMetric is a rail row's resource cell (metricWidth cells, leading space),
// blank when the row has no attributed processes; ok is false when columns are
// off.
func (m *model) railMetric(s *core.Session) (metric, bool) {
	r := &m.res
	if !r.active() {
		return metric{}, false
	}
	if s.IsRoot {
		if c, ok := r.groups[s.ID]; ok {
			return c, true
		}
	}
	if c, ok := r.rows[s.ID]; ok {
		return c, true
	}
	return blankMetric, true
}

// selfMetric is amux's own overhead for the top border, or "".
func (m *model) selfMetric() string {
	if !m.res.active() {
		return ""
	}
	return m.res.self
}

// ---- top panel ----

func (m *model) toggleResources() {
	m.res.open = !m.res.open
	m.res.scroll = 0
}

// handleResourcesKey drives the open panel: it captures every key.
func (m *model) handleResourcesKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	r := &m.res
	switch k.String() {
	case "t", "esc", "q", "ctrl+c":
		r.open = false
	case "s", "tab":
		r.sortMem = !r.sortMem
		r.scroll = 0
	case "c":
		r.sortMem, r.scroll = false, 0
	case "m":
		r.sortMem, r.scroll = true, 0
	case "up", "k":
		r.scroll--
	case "down", "j":
		r.scroll++
	case "pgup", "ctrl+b":
		r.scroll -= max(1, m.paneRows()-5)
	case "pgdown", "ctrl+f":
		r.scroll += max(1, m.paneRows()-5)
	case "home", "g":
		r.scroll = 0
	case "end", "G":
		r.scroll = 1 << 30
	}
	if r.scroll < 0 {
		r.scroll = 0
	}
	return m, nil
}

var (
	resHotStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("214")) // ≥ 1 core
	resBodyStyle = lipgloss.NewStyle()
)

// renderResources draws the top-style panel into the main pane. The rendered
// block is cached until the sample, sort, scroll or geometry changes.
func (m *model) renderResources() string {
	r := &m.res
	w, h := m.mainWidth(), m.paneRows()
	key := resourcesCacheKey{0, r.sortMem, r.scroll, w, h, len(m.sessions), r.unavailable, r.snap != nil, r.reason}
	if r.snap != nil {
		key.sampledAt = r.snap.SampledAt
	}
	if key == r.cacheAt && r.cache != "" {
		return r.cache
	}
	lines := m.resourcesLines(w, h)
	r.cache = lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(strings.Join(lines, "\n"))
	r.cacheAt = key
	return r.cache
}

func (m *model) resourcesLines(w, h int) []string {
	r := &m.res
	sortName, other := "CPU", "mem"
	if r.sortMem {
		sortName, other = "memory", "CPU"
	}
	title := titleStyle.Render(" Resources") + dimStyle.Render(" · by "+sortName+" · ") +
		keyStyle.Render("s") + dimStyle.Render(" sort by "+other+" · ") + keyStyle.Render("t") + dimStyle.Render(" close")
	lines := []string{cellPad(title, w)}
	s := r.snap
	if s == nil {
		msg := "waiting for the daemon's first resource sample…"
		if r.unavailable {
			msg = "resource sampling is unavailable"
			if r.reason != "" {
				msg += ": " + r.reason
			}
		}
		return append(lines, "", dimStyle.Render(" "+truncate(msg, w-1)))
	}
	lines = append(lines, cellPad(" "+keyStyle.Render("amux")+" "+cpuCell(s.Self.CPUPercent, false)+" "+memCell(s.Self.RSSBytes, false)+dimStyle.Render(" · "+r.overhead), w))
	sum := fmt.Sprintf(" host %s of %d CPUs · %s", cpuCell(s.Total.CPUPercent, false), s.CPUs, memCell(s.Total.RSSBytes, false))
	if s.MemTotalBytes > 0 {
		sum += " of " + memCell(s.MemTotalBytes, false)
	}
	sum += fmt.Sprintf(" · other %s %s · every %ds", cpuCell(s.Unattributed.CPUPercent, false), memCell(s.Unattributed.RSSBytes, false), max(1, s.IntervalMS/1000))
	if s.Error != "" {
		sum += " · stale: " + s.Error
	}
	lines = append(lines, dimStyle.Render(truncate(sum, w)))
	lines = append(lines, headerStyle.Render(cellPad("   CPU%    MEM  PROCS  SESSION", w)))

	rows := r.byCPU
	if r.sortMem {
		rows = r.byMem
	}
	titles := make(map[string]string, len(m.sessions))
	for _, ss := range m.sessions {
		titles[ss.ID] = ss.Title
	}
	var body []string
	for _, u := range rows {
		body = append(body, resourceSessionLine(u, titles, w))
		top := u.Top
		if len(top) > resourcesTopProcs {
			top = top[:resourcesTopProcs]
		}
		for i, p := range top {
			branch := "├"
			if i == len(top)-1 {
				branch = "└"
			}
			body = append(body, resourceProcLine(p, branch, w))
		}
	}
	if len(body) == 0 {
		body = []string{dimStyle.Render(" no session processes")}
	}
	avail := max(1, h-len(lines))
	r.scroll = min(r.scroll, max(0, len(body)-avail))
	end := min(len(body), r.scroll+avail)
	return append(lines, body[r.scroll:end]...)
}

func resourceSessionLine(u procstat.SessionUsage, titles map[string]string, w int) string {
	name := u.ID
	if u.ID == u.WorkgroupID {
		name += " (workgroup)"
	} else if u.WorkgroupID != "" {
		name = u.WorkgroupID + "/" + u.ID
	}
	if t := titles[u.ID]; t != "" && t != u.ID {
		name += " · " + t
	}
	cols := fmt.Sprintf(" %6s %6s %6d  ", cpuCell(u.CPUPercent, false), memCell(u.RSSBytes, false), u.Processes)
	style := resBodyStyle
	if u.CPUPercent >= 100 {
		style = resHotStyle
	}
	return style.Render(cols) + keyStyle.Render(truncate(name, w-ansi.StringWidth(cols)))
}

func resourceProcLine(p procstat.Process, branch string, w int) string {
	cols := fmt.Sprintf(" %6s %6s %6s  ", cpuCell(p.CPUPercent, false), memCell(p.RSSBytes, false), "")
	label := fmt.Sprintf("%s %s %d", branch, p.Comm, p.PID)
	if cmd := strings.TrimSpace(p.Cmd); cmd != "" {
		label += "  " + cmd
	}
	return dimStyle.Render(cols + truncate(label, w-ansi.StringWidth(cols)))
}
