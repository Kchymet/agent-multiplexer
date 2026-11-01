package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"amux/internal/core"
	"amux/internal/procstat"
)

// cmdTop prints the daemon's latest resource sample: amux's own overhead, then
// workgroups and their sessions by CPU (or memory), each session with its
// heaviest processes. It reads the daemon's cached sample; it never walks
// /proc itself.
func cmdTop(args []string) error {
	fs := flag.NewFlagSet("top", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the raw resource snapshot as JSON")
	byMem := fs.Bool("mem", false, "sort by resident memory instead of CPU")
	fs.BoolVar(byMem, "m", false, "shorthand for --mem")
	procs := fs.Int("n", 3, "top processes to show per session (0 hides them)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	snap, err := queryResources()
	if err != nil {
		return err
	}
	if *asJSON {
		b, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		return nil
	}
	if snap.Unsupported {
		fmt.Printf("resource sampling is unsupported on this host: %s\n", snap.Error)
		return nil
	}
	if snap.SampledAt == 0 && snap.Error != "" {
		return errors.New(snap.Error)
	}
	var sessions []core.Session
	_ = queryRows(core.QuerySnapshot, &sessions) // titles only; best effort
	titles := make(map[string]string, len(sessions))
	for _, s := range sessions {
		titles[s.ID] = s.Title
	}
	renderTop(os.Stdout, snap, titles, topOptions{byMem: *byMem, procs: *procs, now: time.Now()})
	return nil
}

// queryResources fetches core.QueryResources, translating an older daemon's
// unknown-query reply into what to do about it.
func queryResources() (procstat.Snapshot, error) {
	var snap procstat.Snapshot
	if sessionContextRestricted() {
		return snap, errors.New("resource usage is host-only; run amux top from a host terminal")
	}
	if err := queryRows(core.QueryResources, &snap); err != nil {
		if strings.Contains(err.Error(), "unknown query") {
			return snap, errors.New("the running daemon predates resource sampling; it is available after `amux daemon restart`")
		}
		return snap, err
	}
	return snap, nil
}

type topOptions struct {
	byMem bool
	procs int
	now   time.Time
}

func renderTop(w io.Writer, snap procstat.Snapshot, titles map[string]string, o topOptions) {
	// Header: amux's own overhead, by role.
	type roleSum struct {
		cpu   float64
		count int
	}
	roles := map[string]*roleSum{}
	var order []string
	for _, p := range snap.Self.Procs {
		if p.Role == procstat.RoleChild {
			continue
		}
		r := roles[p.Role]
		if r == nil {
			r = &roleSum{}
			roles[p.Role] = r
			order = append(order, p.Role)
		}
		r.cpu += p.CPUPercent
		r.count++
	}
	var parts []string
	for _, role := range order {
		r := roles[role]
		s := fmt.Sprintf("%s %.1f%%", role, r.cpu)
		if r.count > 1 {
			s += fmt.Sprintf(" (×%d)", r.count)
		}
		parts = append(parts, s)
	}
	fmt.Fprintf(w, "amux   %6.1f%% CPU  %6s RSS  %d procs", snap.Self.CPUPercent, humanBytes(snap.Self.RSSBytes), snap.Self.Processes)
	if len(parts) > 0 {
		fmt.Fprintf(w, "  [%s]", strings.Join(parts, " · "))
	}
	fmt.Fprintf(w, "  sampler %.2f%%\n", snap.Self.Sampler.CPUPercent)
	age := o.now.Sub(time.UnixMilli(snap.SampledAt)).Round(100 * time.Millisecond)
	fmt.Fprintf(w, "host   %6.1f%% CPU  %6s RSS  %d procs  (%d CPUs, %s; sampled %s ago over %.1fs)\n",
		snap.Total.CPUPercent, humanBytes(snap.Total.RSSBytes), snap.Total.Processes,
		snap.CPUs, humanBytes(snap.MemTotalBytes), age, float64(snap.WindowMS)/1000)
	if snap.Error != "" {
		fmt.Fprintf(w, "warning: %s (showing the last good sample)\n", snap.Error)
	}
	if snap.WindowMS == 0 {
		fmt.Fprintln(w, "note: first sample; CPU figures appear after the next one")
	}
	fmt.Fprintln(w)

	less := func(a, b procstat.Usage, ida, idb string) bool {
		if o.byMem && a.RSSBytes != b.RSSBytes {
			return a.RSSBytes > b.RSSBytes
		}
		if a.CPUPercent != b.CPUPercent {
			return a.CPUPercent > b.CPUPercent
		}
		if a.RSSBytes != b.RSSBytes {
			return a.RSSBytes > b.RSSBytes
		}
		return ida < idb
	}
	wgs := append([]procstat.WorkgroupUsage(nil), snap.Workgroups...)
	sort.Slice(wgs, func(i, j int) bool { return less(wgs[i].Usage, wgs[j].Usage, wgs[i].ID, wgs[j].ID) })
	members := map[string][]procstat.SessionUsage{}
	for _, s := range snap.Sessions {
		members[s.WorkgroupID] = append(members[s.WorkgroupID], s)
	}

	fmt.Fprintf(w, "%7s %7s %6s  %-16s %s\n", "CPU%", "RSS", "PROCS", "ID", "NAME")
	row := func(u procstat.Usage, id, title string) {
		fmt.Fprintf(w, "%7.1f %7s %6d  %-16s %s\n", u.CPUPercent, humanBytes(u.RSSBytes), u.Processes, id, title)
	}
	for _, wg := range wgs {
		row(wg.Usage, wg.ID, titles[wg.ID])
		ms := members[wg.ID]
		sort.Slice(ms, func(i, j int) bool { return less(ms[i].Usage, ms[j].Usage, ms[i].ID, ms[j].ID) })
		for _, s := range ms {
			title := titles[s.ID]
			if s.ID == wg.ID {
				title = "(workgroup / coordinator)"
			}
			row(s.Usage, "  "+s.ID, title)
			top := s.Top
			if len(top) > o.procs {
				top = top[:o.procs]
			}
			for _, p := range top {
				fmt.Fprintf(w, "%7.1f %7s %6s      %-8d %s\n", p.CPUPercent, humanBytes(p.RSSBytes), "", p.PID, procLabel(p))
			}
		}
	}
	if len(wgs) == 0 {
		fmt.Fprintln(w, "(no session processes)")
	}
	fmt.Fprintln(w)
	row(snap.Unattributed, "other", "processes outside amux")
}

func procLabel(p procstat.Process) string {
	if p.Cmd != "" {
		return truncate(p.Cmd, 80)
	}
	return p.Comm
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// humanBytes renders a byte count in binary units (5.0G, 412M, 900K).
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	v := float64(b)
	for _, suffix := range []string{"K", "M", "G", "T"} {
		v /= unit
		if v < unit || suffix == "T" {
			if v < 10 {
				return fmt.Sprintf("%.1f%s", v, suffix)
			}
			return fmt.Sprintf("%.0f%s", v, suffix)
		}
	}
	return fmt.Sprintf("%dB", b)
}

// statusSession is one `amux status --json` row: the session plus its
// resource usage from the daemon's latest sample.
type statusSession struct {
	core.Session
	// Resources is the session's own processes (for a workgroup root, its
	// coordinator / workspace-level processes). Absent when nothing is
	// attributed to it.
	Resources *procstat.Usage `json:"resources,omitempty"`
	// WorkgroupResources is set on root rows: the whole workgroup's rollup.
	WorkgroupResources *procstat.Usage `json:"workgroupResources,omitempty"`
}

// statusJSON is `amux status --json`: the snapshot, with per-session
// resources and a top-level resources section (amux self-overhead, host
// totals, sampler cost). Resources is omitted when the daemon cannot provide
// it (an older daemon, or a restricted session context).
type statusJSON struct {
	Type      string           `json:"type"`
	Sessions  []statusSession  `json:"sessions"`
	UpdatedAt int64            `json:"updatedAt"`
	Resources *statusResources `json:"resources,omitempty"`
}

// statusResources is the top-level resources section of status --json. The
// per-session rows live on the sessions themselves.
type statusResources struct {
	SampledAt     int64                     `json:"sampledAt"`
	WindowMS      int64                     `json:"windowMs"`
	CPUs          int                       `json:"cpus"`
	MemTotalBytes uint64                    `json:"memTotalBytes,omitempty"`
	Self          procstat.SelfUsage        `json:"self"`
	Workgroups    []procstat.WorkgroupUsage `json:"workgroups"`
	Unattributed  procstat.Usage            `json:"unattributed"`
	Total         procstat.Usage            `json:"total"`
	Error         string                    `json:"error,omitempty"`
}

func statusWithResources(snapshot core.Snapshot, res *procstat.Snapshot) statusJSON {
	out := statusJSON{Type: snapshot.Type, UpdatedAt: snapshot.UpdatedAt, Sessions: make([]statusSession, 0, len(snapshot.Sessions))}
	own := map[string]procstat.Usage{}
	wgs := map[string]procstat.Usage{}
	if res != nil {
		for _, s := range res.Sessions {
			own[s.ID] = s.Usage
		}
		for _, w := range res.Workgroups {
			wgs[w.ID] = w.Usage
		}
		out.Resources = &statusResources{
			SampledAt: res.SampledAt, WindowMS: res.WindowMS, CPUs: res.CPUs, MemTotalBytes: res.MemTotalBytes,
			Self: res.Self, Workgroups: res.Workgroups, Unattributed: res.Unattributed, Total: res.Total, Error: res.Error,
		}
	}
	for _, s := range snapshot.Sessions {
		row := statusSession{Session: s}
		if u, ok := own[s.ID]; ok {
			row.Resources = &u
		}
		if s.IsRoot {
			if u, ok := wgs[s.ID]; ok {
				row.WorkgroupResources = &u
			}
		}
		out.Sessions = append(out.Sessions, row)
	}
	return out
}
