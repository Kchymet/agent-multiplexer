package cfghome

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// noSettle lets a test reuse hashes of files written a moment ago.
func noSettle(t *testing.T) {
	t.Helper()
	old := hashSettle
	hashSettle = 0
	t.Cleanup(func() { hashSettle = old })
}

// scanReads scans sp and returns the copy-side file reads it cost.
func scanReads(t *testing.T, sp Spec) (map[string]Status, int64) {
	t.Helper()
	before := copyReads.Load()
	st := statuses(t, sp)
	return st, copyReads.Load() - before
}

func seeded(t *testing.T) Spec {
	t.Helper()
	sp := spec(t)
	write(t, filepath.Join(sp.Template, "commands", "y.md"), "yy")
	if _, err := Seed(sp); err != nil {
		t.Fatal(err)
	}
	return sp
}

// A second scan of an unchanged copy reads only hooked files: unhooked files
// are vouched for by their recorded stamps.
func TestStatCacheReusesUnchangedFiles(t *testing.T) {
	noSettle(t)
	sp := seeded(t)
	if st, reads := scanReads(t, sp); len(st) != 0 || reads != 3 {
		t.Fatalf("first scan = %v with %d reads, want clean with 3", st, reads)
	}
	// settings.json has a Normalize hook, so it is always re-read.
	if st, reads := scanReads(t, sp); len(st) != 0 || reads != 1 {
		t.Fatalf("warm scan = %v with %d reads, want clean with 1 (the hooked file)", st, reads)
	}
}

// Without the settle window, a file stamped within it is never trusted.
func TestStatCacheSkipsRacyFiles(t *testing.T) {
	sp := seeded(t) // files written just now; default 2s settle
	scanReads(t, sp)
	if _, reads := scanReads(t, sp); reads != 3 {
		t.Fatalf("scan reused %d-of-3 racy files; want every file re-read", 3-reads)
	}
}

// A same-size edit whose mtime is then restored (touch -r) still advances
// ctime, so the cached hash is not reused and the edit is reported.
func TestStatCacheDetectsSameSizeEditWithRestoredMtime(t *testing.T) {
	noSettle(t)
	sp := seeded(t)
	scanReads(t, sp)
	p := filepath.Join(sp.Dir, "commands", "x.md")
	fi, err := os.Stat(p)
	must(t, err)
	time.Sleep(10 * time.Millisecond) // ctime must be able to move
	must(t, os.WriteFile(p, []byte("z"), 0o644))
	must(t, os.Chtimes(p, fi.ModTime(), fi.ModTime()))
	if st, _ := scanReads(t, sp); st["commands/x.md"] != AgentChanged {
		t.Fatalf("statuses = %v, want commands/x.md agent-changed", st)
	}
}

// Replacing a file by rename (new inode) with identical size and mtime is a
// different file; it is re-read.
func TestStatCacheDetectsInodeSwap(t *testing.T) {
	noSettle(t)
	sp := seeded(t)
	scanReads(t, sp)
	p := filepath.Join(sp.Dir, "commands", "y.md")
	fi, err := os.Stat(p)
	must(t, err)
	tmp := p + ".new"
	must(t, os.WriteFile(tmp, []byte("ZZ"), 0o644))
	must(t, os.Chtimes(tmp, fi.ModTime(), fi.ModTime()))
	must(t, os.Rename(tmp, p))
	if st, _ := scanReads(t, sp); st["commands/y.md"] != AgentChanged {
		t.Fatalf("statuses = %v, want commands/y.md agent-changed", st)
	}
}

// A recorded file replaced by a symlink is not followed or vouched for: the
// regular file is gone from the copy.
func TestStatCacheNeverVouchesForSymlink(t *testing.T) {
	noSettle(t)
	sp := seeded(t)
	scanReads(t, sp)
	p := filepath.Join(sp.Dir, "commands", "y.md")
	must(t, os.Remove(p))
	must(t, os.Symlink(filepath.Join(sp.Template, "commands", "y.md"), p))
	if st, _ := scanReads(t, sp); st["commands/y.md"] != AgentRemoved {
		t.Fatalf("statuses = %v, want commands/y.md agent-removed", st)
	}
}

// A sidecar written for another copy dir, or another version, is ignored.
func TestStatCacheIgnoresForeignSidecar(t *testing.T) {
	noSettle(t)
	sp := seeded(t)
	scanReads(t, sp)
	b, err := os.ReadFile(statCachePath(sp))
	must(t, err)
	must(t, os.WriteFile(statCachePath(sp), bytes.Replace(b, []byte(sp.Dir), []byte(sp.Dir+"x"), 1), 0o644))
	if _, reads := scanReads(t, sp); reads != 3 {
		t.Fatalf("scan with a foreign sidecar read %d files, want 3", reads)
	}
}

// A file written during the read is never recorded as reusable.
func TestStableDuringRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	must(t, os.WriteFile(p, []byte("a"), 0o644))
	f, err := os.Open(p)
	must(t, err)
	defer f.Close()
	fi, err := f.Stat()
	must(t, err)
	st, ok := stampOf(fi)
	if !ok {
		t.Skip("no file stamps on this platform")
	}
	if !stableDuringRead(f, st) {
		t.Fatal("unchanged file reported unstable")
	}
	time.Sleep(10 * time.Millisecond)
	must(t, os.WriteFile(p, []byte("b"), 0o644))
	if stableDuringRead(f, st) {
		t.Fatal("file rewritten during read reported stable")
	}
}

// Forget drops the sidecar with the manifest.
func TestForgetRemovesSidecar(t *testing.T) {
	noSettle(t)
	sp := seeded(t)
	scanReads(t, sp)
	Forget(sp)
	if _, err := os.Stat(statCachePath(sp)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sidecar after Forget: %v", err)
	}
}

// A run-scoped template snapshot the user's edit has overtaken must not be
// persisted as agreement: the agent made the same change the template had at
// snapshot time, then the template moved on. The baseline stays put, so the
// next scan sees a conflict rather than wrongly blaming the template.
func TestStaleTemplateSnapshotDoesNotAdvanceBaseline(t *testing.T) {
	sp := seeded(t)
	tp := filepath.Join(sp.Template, "commands", "x.md")
	write(t, tp, "v1")
	write(t, filepath.Join(sp.Dir, "commands", "x.md"), "v1")
	cache := NewScanCache()
	cache.template(sp) // snapshot taken: template at v1
	write(t, tp, "v2")
	if _, err := cache.Scan(sp); err != nil {
		t.Fatal(err)
	}
	if st := statuses(t, sp); st["commands/x.md"] != Conflict {
		t.Fatalf("statuses = %v, want commands/x.md conflict", st)
	}
}

// fleet seeds n agents of one template whose settings.json Normalize hook
// depends on the spec (it maps the agent's dir back to the template) and
// rewrites its input in place — the shared raw bytes must survive both.
func fleet(t *testing.T, n int) []Spec { return fleetWith(t, n, nil) }

// fleetWith is fleet with probe called inside every settings.json Normalize
// (that is, inside each scan of that agent, under its locks).
func fleetWith(t *testing.T, n int, probe func(Spec)) []Spec {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	tmpl := filepath.Join(root, "template")
	write(t, filepath.Join(tmpl, "settings.json"), `{"hook":"`+tmpl+`/x.sh"}`)
	for i := range 30 {
		write(t, filepath.Join(tmpl, "commands", fmt.Sprintf("c%d.md", i)), fmt.Sprintf("c%d", i))
	}
	mutating := func(s Spec, b []byte) []byte {
		if probe != nil {
			probe(s)
		}
		out := bytes.ReplaceAll(b, []byte(s.Dir), []byte(s.Template))
		for i := range b { // scribble on the input, deliberately and cumulatively
			b[i]++
		}
		return out
	}
	var specs []Spec
	for i := range n {
		sess := filepath.Join(root, fmt.Sprintf("s%02d", i))
		must(t, os.MkdirAll(sess, 0o755))
		sp := Spec{
			Kind: "k", AgentID: fmt.Sprintf("a%02d", i), Env: "K_HOME",
			Template: tmpl, Root: sess, Dir: filepath.Join(sess, "home"),
			Entries: []Entry{
				{Rel: "settings.json", Normalize: mutating},
				{Rel: "commands"},
			},
		}
		if _, err := Seed(sp); err != nil {
			t.Fatal(err)
		}
		// The copy's settings point at its own dir, as Seed would rewrite them:
		// only a per-spec Normalize maps them back to match the template.
		write(t, filepath.Join(sp.Dir, "settings.json"), `{"hook":"`+sp.Dir+`/x.sh"}`)
		if i%4 == 0 {
			write(t, filepath.Join(sp.Dir, "commands", "c0.md"), "edited")
		}
		specs = append(specs, sp)
	}
	return specs
}

// ScanAll builds each template snapshot once however many workers race for
// it, gives each spec its own hook results, and matches sequential Scan.
func TestScanAllSharesOneTemplateSnapshot(t *testing.T) {
	specs := fleet(t, 24)
	builds := templateBuilds.Load()
	results := ScanAll(specs, ScanOptions{Workers: 8})
	if got := templateBuilds.Load() - builds; got != 1 {
		t.Fatalf("template snapshot built %d times, want 1", got)
	}
	for i, r := range results {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
		want := mustScan(t, specs[i])
		if fmt.Sprint(r.Changes) != fmt.Sprint(want) {
			t.Fatalf("%s: ScanAll = %v, Scan = %v", specs[i].AgentID, r.Changes, want)
		}
		if edited := i%4 == 0; (len(want) == 1) != edited {
			t.Fatalf("%s: changes = %v, edited = %v", specs[i].AgentID, want, edited)
		}
	}
	agents, edits, err := DriftSummary(specs, ScanOptions{Workers: 8})
	if err != nil || agents != 6 || edits != 6 {
		t.Fatalf("DriftSummary = %d agents, %d edits, %v; want 6, 6", agents, edits, err)
	}
}

// Scans and promote/reset writers interleave without deadlock or race, and the
// writer's result is what a scan after it sees.
func TestScanAllWithConcurrentWriters(t *testing.T) {
	specs := fleet(t, 16)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range 5 {
			ScanAll(specs, ScanOptions{Workers: 4})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < len(specs); i += 4 {
			if err := Reset(specs[i], "commands/c0.md"); err != nil {
				t.Error(err)
			}
			Summary(specs[i])
			Invalidate(specs[i])
		}
	}()
	wg.Wait()
	agents, edits, err := DriftSummary(specs, ScanOptions{})
	if err != nil || agents != 0 || edits != 0 {
		t.Fatalf("after resets: %d agents, %d edits, %v; want clean", agents, edits, err)
	}
}

// Progress is reported once per spec, in order, and a scan error fails the
// summary labelled with the agent instead of reading as clean.
func TestDriftSummaryProgressAndErrors(t *testing.T) {
	specs := fleet(t, 5)
	var seen []int
	if _, _, err := DriftSummary(specs, ScanOptions{Workers: 3, Progress: func(done, total int) {
		if total != 5 {
			t.Errorf("total = %d", total)
		}
		seen = append(seen, done)
	}}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(seen) != "[0 1 2 3 4 5]" {
		t.Fatalf("progress = %v", seen)
	}
	bad := specs[2]
	bad.Root = ""
	specs[2] = bad
	if _, _, err := DriftSummary(specs, ScanOptions{}); err == nil || !bytes.Contains([]byte(err.Error()), []byte("a02 (k)")) {
		t.Fatalf("err = %v, want one labelled a02 (k)", err)
	}
}
