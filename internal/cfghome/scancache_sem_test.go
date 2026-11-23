package cfghome

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// probeFleet is fleetWith whose probe can be armed after seeding (Seed also
// runs Normalize, and setup must not trip a test's probe).
func probeFleet(t *testing.T, n int) ([]Spec, *atomic.Pointer[func(Spec)]) {
	t.Helper()
	var p atomic.Pointer[func(Spec)]
	specs := fleetWith(t, n, func(s Spec) {
		if f := p.Load(); f != nil {
			(*f)(s)
		}
	})
	return specs, &p
}

// ScanAll never runs more scans at once than Workers, and does run them in
// parallel (the pool is not secretly serial).
func TestScanAllBoundsParallelism(t *testing.T) {
	specs, probe := probeFleet(t, 24)
	var inflight, peak atomic.Int64
	f := func(Spec) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
		inflight.Add(-1)
	}
	probe.Store(&f)
	for _, workers := range []int{1, 3} {
		peak.Store(0)
		for _, r := range ScanAll(specs, ScanOptions{Workers: workers}) {
			if r.Err != nil {
				t.Fatal(r.Err)
			}
		}
		if got := peak.Load(); got > int64(workers) || (workers > 1 && got < 2) {
			t.Fatalf("workers=%d: peak concurrent scans = %d", workers, got)
		}
	}
}

// Results land at their spec's index however completion is ordered, and the
// summary reports the first failing spec in input order, not completion order.
func TestScanAllStableOrderAndFirstError(t *testing.T) {
	specs, probe := probeFleet(t, 12)
	f := func(s Spec) {
		// Earlier agents finish later.
		if s.AgentID < "a06" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	probe.Store(&f)
	results := ScanAll(specs, ScanOptions{Workers: 6})
	for i, r := range results {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
		edited := i%4 == 0
		if edited != (len(r.Changes) == 1) {
			t.Fatalf("result %d = %v: not at its spec's index", i, r.Changes)
		}
		if edited && !strings.HasPrefix(r.Changes[0].Copy, specs[i].Dir) {
			t.Fatalf("result %d copy %s, want under %s", i, r.Changes[0].Copy, specs[i].Dir)
		}
	}
	for _, i := range []int{9, 3} {
		bad := specs[i]
		bad.Root = ""
		specs[i] = bad
	}
	_, _, err := DriftSummary(specs, ScanOptions{Workers: 6})
	if err == nil || !strings.HasPrefix(err.Error(), "a03 (k): ") {
		t.Fatalf("err = %v, want the first failing spec in input order (a03)", err)
	}
}

// blocked reports whether ch stays open for a moment.
func blocked(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return false
	case <-time.After(150 * time.Millisecond):
		return true
	}
}

// A Reset (a template-lock writer) waits for an in-flight scan of another agent
// to finish: it can never interleave with a scan reading the template.
func TestResetWaitsForInFlightScan(t *testing.T) {
	specs, probe := probeFleet(t, 2)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f := func(s Spec) {
		if s.AgentID == "a00" {
			once.Do(func() { close(entered); <-release })
		}
	}
	probe.Store(&f)
	scanDone := make(chan struct{})
	go func() { defer close(scanDone); _, _ = Scan(specs[0]) }()
	<-entered
	resetDone := make(chan struct{})
	go func() {
		defer close(resetDone)
		if err := Reset(specs[1], "commands/c0.md"); err != nil {
			t.Error(err)
		}
	}()
	if !blocked(resetDone) {
		t.Fatal("Reset completed while a scan held the template")
	}
	close(release)
	<-scanDone
	<-resetDone
}

// A scan waits for an in-flight Promote of another agent, which is rewriting
// the shared template, so no scan ever reads it half-written.
func TestScanWaitsForInFlightPromote(t *testing.T) {
	specs := fleet(t, 2)
	entered, release := make(chan struct{}), make(chan struct{})
	specs[0].Entries[0].Merge = func(_ Spec, _, copy []byte) ([]byte, error) {
		close(entered)
		<-release
		return copy, nil
	}
	promoteDone := make(chan struct{})
	go func() {
		defer close(promoteDone)
		if err := Promote(specs[0], "settings.json"); err != nil {
			t.Error(err)
		}
	}()
	<-entered
	scanDone := make(chan struct{})
	go func() { defer close(scanDone); _, _ = Scan(specs[1]) }()
	if !blocked(scanDone) {
		t.Fatal("scan completed while Promote held the template")
	}
	close(release)
	<-promoteDone
	<-scanDone
}

// Scans of different agents do not serialize on each other (the old global
// mutex): a scan blocked inside one agent leaves another agent scannable.
func TestScansOfDifferentAgentsRunInParallel(t *testing.T) {
	specs, probe := probeFleet(t, 2)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f := func(s Spec) {
		if s.AgentID == "a00" {
			once.Do(func() { close(entered); <-release })
		}
	}
	probe.Store(&f)
	first := make(chan struct{})
	go func() { defer close(first); _, _ = Scan(specs[0]) }()
	<-entered
	done := make(chan struct{})
	go func() { defer close(done); _, _ = Scan(specs[1]) }()
	parallel := !blocked(done)
	close(release)
	<-first
	<-done
	if !parallel {
		t.Fatal("scan of a01 waited on a scan of a00")
	}
}

// The sidecar is an optional performance cache: when it cannot be written (or
// read), scans still succeed with the same result.
func TestUnwritableSidecarDoesNotFailScan(t *testing.T) {
	noSettle(t)
	sp := seeded(t)
	write(t, filepath.Join(sp.Dir, "commands", "x.md"), "edited")
	// A directory where the sidecar file belongs makes every write fail.
	must(t, os.MkdirAll(statCachePath(sp), 0o755))
	for range 2 {
		if st := statuses(t, sp); len(st) != 1 || st["commands/x.md"] != AgentChanged {
			t.Fatalf("statuses = %v, want only commands/x.md agent-changed", st)
		}
	}
	// A corrupt sidecar is ignored the same way.
	must(t, os.Remove(statCachePath(sp)))
	must(t, os.WriteFile(statCachePath(sp), []byte("{not json"), 0o644))
	if st := statuses(t, sp); len(st) != 1 {
		t.Fatalf("statuses = %v with a corrupt sidecar", st)
	}
}

// The run cache shares a snapshot only between specs reading the same files:
// a different template path, harness kind, or external Entry.Src layout gets
// its own snapshot (and its own correct hashes), while the normal case — many
// agents of one template — builds once.
func TestScanCacheKeysByTemplateKindAndLayout(t *testing.T) {
	specs := fleet(t, 3)
	base := specs[0]
	otherTmpl := t.TempDir()
	write(t, filepath.Join(otherTmpl, "commands", "c0.md"), "other")
	ext := filepath.Join(t.TempDir(), "external-settings.json")
	write(t, ext, "{}")
	variant := func(edit func(*Spec)) Spec {
		sp := base
		sp.Entries = append([]Entry(nil), base.Entries...)
		edit(&sp)
		return sp
	}
	cache := NewScanCache()
	builds := templateBuilds.Load()
	snaps := map[string]*templateSnap{}
	for name, sp := range map[string]Spec{
		"same-a":   specs[1],
		"same-b":   specs[2],
		"tmpl":     variant(func(s *Spec) { s.Template = otherTmpl }),
		"kind":     variant(func(s *Spec) { s.Kind = "other" }),
		"src":      variant(func(s *Spec) { s.Entries[0].Src = ext }),
		"unhooked": variant(func(s *Spec) { s.Entries[0].Normalize = nil }),
	} {
		snaps[name] = cache.template(sp)
	}
	if got := templateBuilds.Load() - builds; got != 5 {
		t.Fatalf("built %d snapshots, want 5 (one shared by same-a/same-b)", got)
	}
	if snaps["same-a"] != snaps["same-b"] {
		t.Fatal("agents of one template did not share a snapshot")
	}
	if snaps["tmpl"].hashes["commands/c0.md"] == snaps["same-a"].hashes["commands/c0.md"] {
		t.Fatal("other template shared the first template's hashes")
	}
	if string(snaps["src"].raw["settings.json"]) != "{}" {
		t.Fatalf("external Src snapshot = %q, want the external file", snaps["src"].raw["settings.json"])
	}
	if _, ok := snaps["unhooked"].hashes["settings.json"]; !ok {
		t.Fatal("an unhooked entry should hash as shared bytes")
	}
}
