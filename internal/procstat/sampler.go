package procstat

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Defaults.
const (
	DefaultInterval = 3 * time.Second
	DefaultTopN     = 5
	// clockTicks is USER_HZ, the unit of /proc/<pid>/stat times. It is 100 on
	// every Linux ABI Go supports.
	clockTicks = 100
	// metaTTL bounds how stale a cached cmdline/cwd may get. Exec changes comm,
	// which invalidates the cache at once; the TTL catches `cd` and same-name
	// re-execs.
	metaTTL = 30 * time.Second
	// pssTTL bounds how often one process's smaps_rollup is read: the kernel
	// walks its page tables to answer, which is the costliest read here for a
	// multi-GiB process like an emulator.
	pssTTL = 15 * time.Second
	// cmdMax caps Process.Cmd.
	cmdMax = 160
)

// Config configures a Sampler. Zero values take the defaults.
type Config struct {
	// Root is the procfs mount ("/proc"). Tests point it at a fixture tree.
	Root string
	// Interval is the sampling period (DefaultInterval).
	Interval time.Duration
	// TopN caps SessionUsage.Top and Self's child list (DefaultTopN).
	TopN int
	// SessionsDir and WorkspacesDir are amux's sandbox parents, matched in
	// process cwds and cmdlines (core.SessionsDir / core.WorkspacesDir).
	SessionsDir   string
	WorkspacesDir string
	// SelfNames are executable basenames that count as amux itself
	// (default {"amux"}).
	SelfNames []string
	// PageSize converts stat's rss pages to bytes (os.Getpagesize).
	PageSize int
	// CPUs overrides the reported CPU count (runtime.NumCPU).
	CPUs int
	// Now overrides the clock (tests).
	Now func() time.Time
}

// procKey identifies one process instance: a pid reused by a new process has a
// different starttime.
type procKey struct {
	pid   int
	start uint64
}

type procMeta struct {
	comm   string
	argv   []string
	cwd    string
	claim  claim
	readAt time.Time
}

type pssEntry struct {
	bytes  uint64
	ok     bool
	readAt time.Time
}

// Sampler periodically samples procfs and caches the latest Snapshot.
type Sampler struct {
	cfg  Config
	attr attributor

	mu     sync.RWMutex
	latest Snapshot

	// Sampling state, owned by whichever goroutine is inside Sample (samples
	// are serialized by sampleMu).
	sampleMu   sync.Mutex
	rd         reader
	prevTicks  map[procKey]uint64
	prevUptime float64 // seconds; 0 before the first sample
	prevAt     time.Time
	meta       map[procKey]*procMeta
	pss        map[procKey]pssEntry
	memTotal   uint64 // read once; MemTotal does not change
}

// New returns a Sampler; it does nothing until Run or Sample is called.
func New(cfg Config) *Sampler {
	if cfg.Root == "" {
		cfg.Root = "/proc"
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.TopN <= 0 {
		cfg.TopN = DefaultTopN
	}
	if len(cfg.SelfNames) == 0 {
		cfg.SelfNames = []string{"amux"}
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = os.Getpagesize()
	}
	if cfg.CPUs <= 0 {
		cfg.CPUs = runtime.NumCPU()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Sampler{
		cfg:       cfg,
		attr:      newAttributor(cfg.SessionsDir, cfg.WorkspacesDir, cfg.SelfNames),
		rd:        reader{root: cfg.Root},
		prevTicks: map[procKey]uint64{},
		meta:      map[procKey]*procMeta{},
		pss:       map[procKey]pssEntry{},
		latest:    Snapshot{IntervalMS: cfg.Interval.Milliseconds(), Error: "no sample yet"},
	}
}

// Latest returns the most recent snapshot. It never samples. The returned
// value shares slices with the sampler's cache; callers must not mutate it.
func (s *Sampler) Latest() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest
}

// Run samples every Interval until ctx ends. It pins itself to one OS thread so
// the sampler's own CPU time can be measured per sample (SelfUsage.Sampler).
func (s *Sampler) Run(ctx context.Context) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		s.sampleMeasured()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Sampler) sampleMeasured() {
	cpu0, cpuOK := threadCPU()
	wall0 := time.Now()
	snap, err := s.Sample()
	wall := time.Since(wall0)
	cpu1, _ := threadCPU()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.latest.Error = err.Error()
		return
	}
	snap.Self.Sampler.WallMS = ms(wall)
	if cpuOK {
		snap.Self.Sampler.CPUMS = ms(cpu1 - cpu0)
		snap.Self.Sampler.CPUPercent = round1(100 * float64(cpu1-cpu0) / float64(s.cfg.Interval))
	}
	s.latest = snap
}

// Sample takes one sample synchronously, caches it as Latest, and returns it.
// CPU percentages are relative to the previous Sample call. Run calls this; it
// is exported for tests and for one-shot use.
func (s *Sampler) Sample() (Snapshot, error) {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	snap, err := s.sample()
	if err == nil {
		s.mu.Lock()
		s.latest = snap
		s.mu.Unlock()
	}
	return snap, err
}

// live is one process in the current sample.
type live struct {
	key   procKey
	st    procStat
	meta  *procMeta
	claim claim
	cpu   float64
	// resolved owner after inheritance
	owner    claim
	resolved bool
	visiting bool // on the current resolve walk (ppid cycle guard)
}

func (s *Sampler) sample() (Snapshot, error) {
	now := s.cfg.Now()
	dir, err := os.Open(s.cfg.Root)
	if err != nil {
		return Snapshot{}, fmt.Errorf("procfs unavailable: %w", err)
	}
	names, err := dir.Readdirnames(-1)
	dir.Close()
	if err != nil {
		return Snapshot{}, fmt.Errorf("procfs unreadable: %w", err)
	}
	if s.memTotal == 0 {
		s.memTotal = s.rd.memTotal()
	}
	uptime, uptimeOK := s.rd.uptimeSeconds()
	window := 0.0 // seconds
	havePrev := s.prevUptime > 0 || !s.prevAt.IsZero()
	if havePrev {
		if uptimeOK && s.prevUptime > 0 {
			window = uptime - s.prevUptime
		} else {
			window = now.Sub(s.prevAt).Seconds()
		}
	}
	prevUptimeTicks := uint64(s.prevUptime * clockTicks)

	procs := make(map[int]*live, len(names))
	ticks := make(map[procKey]uint64, len(names))
	for _, name := range names {
		if !isPID(name) {
			continue
		}
		st, err := s.rd.stat(name)
		if err != nil {
			continue // vanished between readdir and read, or unreadable
		}
		key := procKey{pid: st.pid, start: st.start}
		ticks[key] = st.ticks
		p := &live{key: key, st: st}
		if prev, ok := s.prevTicks[key]; ok {
			if st.ticks >= prev {
				p.cpu = float64(st.ticks - prev)
			}
		} else if havePrev && uptimeOK && s.prevUptime > 0 && st.start >= prevUptimeTicks {
			// Started after the previous sample: all of its CPU time falls in
			// this window. (A process merely unseen last time — say it was
			// unreadable — has no baseline and reports 0 until next sample.)
			p.cpu = float64(st.ticks)
		}
		if window > 0 {
			p.cpu = p.cpu / clockTicks / window * 100
		} else {
			p.cpu = 0
		}
		p.meta = s.metaFor(name, key, st.comm, now)
		p.claim = p.meta.claim
		procs[st.pid] = p
	}

	// Inherit claims down the tree.
	for _, p := range procs {
		resolve(p, procs)
	}

	snap := Snapshot{
		SampledAt:     now.UnixMilli(),
		IntervalMS:    s.cfg.Interval.Milliseconds(),
		WindowMS:      int64(window * 1000),
		CPUs:          s.cfg.CPUs,
		MemTotalBytes: s.memTotal,
	}
	type group struct {
		wg    string
		usage Usage
		procs []*live
	}
	sessions := map[string]*group{}
	var selfProcs, selfChildren []*live
	for _, p := range procs {
		add(&snap.Total, p, s.cfg.PageSize)
		switch p.owner.kind {
		case claimSession:
			g := sessions[p.owner.session]
			if g == nil {
				g = &group{wg: p.owner.workgroup}
				sessions[p.owner.session] = g
			}
			add(&g.usage, p, s.cfg.PageSize)
			g.procs = append(g.procs, p)
		case claimSelf:
			add(&snap.Self.Usage, p, s.cfg.PageSize)
			if p.claim.kind == claimSelf {
				selfProcs = append(selfProcs, p)
			} else {
				selfChildren = append(selfChildren, p)
			}
		default:
			add(&snap.Unattributed, p, s.cfg.PageSize)
		}
	}
	snap.Self.Sampler.Scanned = len(procs)

	wgs := map[string]*WorkgroupUsage{}
	for id, g := range sessions {
		su := SessionUsage{ID: id, WorkgroupID: g.wg, Usage: roundUsage(g.usage)}
		for _, p := range s.top(g.procs, now) {
			su.Top = append(su.Top, p)
		}
		snap.Sessions = append(snap.Sessions, su)
		w := wgs[g.wg]
		if w == nil {
			w = &WorkgroupUsage{ID: g.wg}
			wgs[g.wg] = w
		}
		w.CPUPercent += g.usage.CPUPercent
		w.RSSBytes += g.usage.RSSBytes
		w.Processes += g.usage.Processes
		w.Threads += g.usage.Threads
		w.Sessions++
	}
	for _, w := range wgs {
		w.Usage = roundUsage(w.Usage)
		snap.Workgroups = append(snap.Workgroups, *w)
	}
	sort.Slice(snap.Sessions, func(i, j int) bool {
		return heavier(snap.Sessions[i].Usage, snap.Sessions[j].Usage, snap.Sessions[i].ID, snap.Sessions[j].ID)
	})
	sort.Slice(snap.Workgroups, func(i, j int) bool {
		return heavier(snap.Workgroups[i].Usage, snap.Workgroups[j].Usage, snap.Workgroups[i].ID, snap.Workgroups[j].ID)
	})
	sortLive(selfProcs)
	for _, p := range selfProcs {
		snap.Self.Procs = append(snap.Self.Procs, s.process(p, p.claim.role, now))
	}
	for _, p := range s.top(selfChildren, now) {
		p.Role = RoleChild
		snap.Self.Procs = append(snap.Self.Procs, p)
	}
	snap.Self.Usage = roundUsage(snap.Self.Usage)
	snap.Unattributed = roundUsage(snap.Unattributed)
	snap.Total = roundUsage(snap.Total)

	// Commit sample state; forget processes that are gone.
	s.prevTicks = ticks
	s.prevAt = now
	if uptimeOK {
		s.prevUptime = uptime
	}
	for k := range s.meta {
		if _, ok := ticks[k]; !ok {
			delete(s.meta, k)
		}
	}
	for k := range s.pss {
		if _, ok := ticks[k]; !ok {
			delete(s.pss, k)
		}
	}
	return snap, nil
}

// resolve sets p.owner to its own claim or its nearest claimed ancestor's.
func resolve(p *live, procs map[int]*live) claim {
	// Walk up iteratively, then fill the chain, so the whole tree costs O(n).
	// A ppid cycle (only possible from reads torn across a pid's reuse) ends
	// the walk unclaimed.
	var chain []*live
	cur := p
	var owner claim
	for cur != nil && !cur.visiting {
		if cur.resolved {
			owner = cur.owner
			break
		}
		cur.visiting = true
		chain = append(chain, cur)
		if cur.claim.kind != claimNone {
			owner = cur.claim
			break
		}
		if cur.st.ppid == cur.st.pid {
			break
		}
		cur = procs[cur.st.ppid]
	}
	// Each process in chain up to (not including) the claimant inherits owner;
	// the claimant (if it is in chain) owns itself.
	for _, c := range chain {
		c.resolved, c.visiting = true, false
		if c.claim.kind != claimNone {
			c.owner = c.claim
		} else {
			c.owner = owner
		}
	}
	return p.owner
}

func (s *Sampler) metaFor(name string, key procKey, comm string, now time.Time) *procMeta {
	// Spread refreshes over a few samples rather than rereading every process
	// in the same one.
	ttl := metaTTL + time.Duration(key.pid%16)*time.Second
	if m, ok := s.meta[key]; ok && m.comm == comm && now.Sub(m.readAt) < ttl {
		return m
	}
	m := &procMeta{comm: comm, argv: s.rd.cmdline(name), cwd: s.rd.cwd(name), readAt: now}
	m.claim = s.attr.direct(comm, m.cwd, m.argv)
	s.meta[key] = m
	return m
}

// top returns up to TopN of ps, heaviest first, as Process values.
func (s *Sampler) top(ps []*live, now time.Time) []Process {
	sortLive(ps)
	if len(ps) > s.cfg.TopN {
		ps = ps[:s.cfg.TopN]
	}
	out := make([]Process, 0, len(ps))
	for _, p := range ps {
		out = append(out, s.process(p, "", now))
	}
	return out
}

func (s *Sampler) process(p *live, role string, now time.Time) Process {
	out := Process{
		PID: p.st.pid, PPID: p.st.ppid, Comm: p.st.comm, Cmd: shortCmd(p.meta.argv),
		Role: role, CPUPercent: round1(p.cpu), RSSBytes: p.st.rssPages * uint64(s.cfg.PageSize),
		Threads: p.st.threads,
	}
	e, ok := s.pss[p.key]
	if !ok || now.Sub(e.readAt) >= pssTTL {
		e.bytes, e.ok = s.rd.pss(p.st.pid)
		e.readAt = now
		s.pss[p.key] = e
	}
	if e.ok {
		out.PSSBytes = e.bytes
	}
	return out
}

func add(u *Usage, p *live, page int) {
	u.CPUPercent += p.cpu
	u.RSSBytes += p.st.rssPages * uint64(page)
	u.Processes++
	u.Threads += p.st.threads
}

func sortLive(ps []*live) {
	sort.Slice(ps, func(i, j int) bool {
		a, b := ps[i], ps[j]
		if a.cpu != b.cpu {
			return a.cpu > b.cpu
		}
		if a.st.rssPages != b.st.rssPages {
			return a.st.rssPages > b.st.rssPages
		}
		return a.st.pid < b.st.pid
	})
}

func heavier(a, b Usage, ida, idb string) bool {
	if a.CPUPercent != b.CPUPercent {
		return a.CPUPercent > b.CPUPercent
	}
	if a.RSSBytes != b.RSSBytes {
		return a.RSSBytes > b.RSSBytes
	}
	return ida < idb
}

func shortCmd(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	parts := append([]string{filepath.Base(argv[0])}, argv[1:]...)
	s := strings.Join(parts, " ")
	if len(s) > cmdMax {
		// Cut on a rune boundary.
		cut := cmdMax
		for cut > 0 && !utf8Start(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return s
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func roundUsage(u Usage) Usage {
	u.CPUPercent = round1(u.CPUPercent)
	return u
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// ParseInterval parses a sampler interval setting: a Go duration ("2s"), a
// bare number of seconds ("5"), or "off"/"0" to disable (returns 0).
// Enabled intervals are clamped to [1s, 60s].
func ParseInterval(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "":
		return DefaultInterval, nil
	case "off", "0", "false", "disabled":
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		n, nerr := strconv.ParseFloat(v, 64)
		if nerr != nil {
			return 0, errors.New("want a duration like 3s, or off")
		}
		d = time.Duration(n * float64(time.Second))
	}
	if d <= 0 {
		return 0, nil
	}
	if d < time.Second {
		d = time.Second
	}
	if d > time.Minute {
		d = time.Minute
	}
	return d, nil
}

// Disabled is the snapshot a daemon serves when sampling is off or
// unsupported.
func Disabled(reason string) Snapshot { return Snapshot{Error: reason} }
