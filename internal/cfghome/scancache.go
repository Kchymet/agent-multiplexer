package cfghome

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"amux/internal/hostprep"
)

// ---- locking ------------------------------------------------------------------
//
// Lock order, outermost first: templateMu → one agent's lock → leaf locks
// (hashMu, a ScanCache's map, summaryMu). summaryMu is never held across a
// scan. Scan and Seed read the template, so they share templateMu; Promote and
// Reset write it, so they hold it exclusively — a writer never interleaves with
// a scan that might advance some agent's baseline from a half-written template.
// Everything that reads-modifies-writes one agent's manifest or stat sidecar
// holds that agent's lock, so scans of different agents run in parallel.
//
// These are in-process locks. Separate amux processes (the daemon's poll and a
// CLI promote) were never serialized by the old global mutex either; manifest
// and sidecar writes are atomic renames, so a cross-process race leaves a
// complete file from one side, and the next scan reconciles.

var (
	templateMu sync.RWMutex

	agentLocksMu sync.Mutex
	agentLocks   = map[string]*sync.Mutex{}
)

// lockAgent takes the lock for sp's manifest and sidecar. Callers that also
// need templateMu take it first.
func lockAgent(sp Spec) (unlock func()) {
	key := manifestPath(sp)
	agentLocksMu.Lock()
	l := agentLocks[key]
	if l == nil {
		l = &sync.Mutex{}
		agentLocks[key] = l
	}
	agentLocksMu.Unlock()
	l.Lock()
	return l.Unlock
}

// ---- file stamps ----------------------------------------------------------------

// fileStamp is what a cached hash must still match before it may stand in for
// reading the file: identity (device, inode), link count, size, mode, and both
// mtime and ctime. ctime matters because userspace can restore mtime (touch -r,
// utimes) after a same-size edit, but cannot set ctime — any write, chmod,
// rename-over, or utimes advances it.
type fileStamp struct {
	Dev   uint64 `json:"dev"`
	Ino   uint64 `json:"ino"`
	Nlink uint64 `json:"nlink"`
	Size  int64  `json:"size"`
	Mode  uint32 `json:"mode"`
	Mtime int64  `json:"mtime"` // unix nanoseconds
	Ctime int64  `json:"ctime"` // unix nanoseconds
}

// reusable reports whether a hash taken at hashed (unix nanos) of a file then
// stamped s may be reused for a file now stamped now. Beyond an exact match,
// both timestamps must predate the hash by hashSettle: a file written in the
// same timestamp tick as (or just before) the hash could change again without
// any visible stamp difference on a coarse-grained filesystem ("racy clean").
// A multiply linked file is never trusted: a write through another name would
// not be attributed to this path.
func (s fileStamp) reusable(now fileStamp, hashed int64) bool {
	if s != now || now.Nlink != 1 {
		return false
	}
	limit := hashed - int64(hashSettle)
	return now.Mtime < limit && now.Ctime < limit
}

// hashSettle is how long before a hash a file's mtime and ctime must lie for
// the hash to be reused (see fileStamp.reusable).
var hashSettle = 2 * time.Second // a var only so tests need not sleep

// hooked reports whether rel is an exact entry with a Seed or Normalize hook.
// Hooks receive the Spec, so their output may depend on the agent (paths, ids):
// such a file's hash is never shared between agents nor cached across runs —
// it is recomputed from raw bytes with this spec's own hooks. Files no hook
// touches (everything under a directory entry, and plain file entries) hash to
// sha256 of their raw bytes regardless of spec.
func hooked(sp Spec, rel string) bool {
	e := entryFor(sp, rel)
	return e.Rel == rel && (e.Seed != nil || e.Normalize != nil)
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ---- copy side: persistent stat sidecar -------------------------------------------

// statCacheVersion invalidates every sidecar when the recorded semantics change.
const statCacheVersion = 1

// statRecord is one copy file's last hash and the stamp it was taken against.
type statRecord struct {
	fileStamp
	Hashed int64  `json:"hashed"` // unix nanoseconds, taken after the read
	Sum    string `json:"sum"`
}

// statCache is the per-agent sidecar beside the manifest (under the state dir,
// never inside the sandbox, so the agent cannot plant hashes). It only covers
// unhooked files, whose hash is a pure function of their bytes, and is only
// valid for the copy dir it was written for.
type statCache struct {
	Version int                   `json:"version"`
	Dir     string                `json:"dir"`
	Files   map[string]statRecord `json:"files"`

	next  map[string]statRecord // records observed by this scan
	dirty bool
}

func statCachePath(sp Spec) string {
	return strings.TrimSuffix(manifestPath(sp), ".json") + ".stat.json"
}

// loadStatCache reads sp's sidecar; any problem (missing, corrupt, other
// version or dir) yields an empty cache, which only costs a full read.
func loadStatCache(sp Spec) *statCache {
	sc := &statCache{next: map[string]statRecord{}}
	if b, err := os.ReadFile(statCachePath(sp)); err == nil {
		var disk statCache
		if json.Unmarshal(b, &disk) == nil && disk.Version == statCacheVersion && disk.Dir == sp.Dir {
			sc.Files = disk.Files
		}
	}
	if sc.Files == nil {
		sc.Files = map[string]statRecord{}
	}
	return sc
}

// hash returns the compared hash of the copy's rel, already opened as f with
// fstat info. An unhooked file whose stamp still matches its record is not
// read. A read error (including hostprep.ErrFileTooLarge) is returned for the
// caller to handle as before: such a file is left out of the snapshot. A nil
// *statCache reads and hashes every file.
func (sc *statCache) hash(sp Spec, rel string, info fs.FileInfo, f *os.File) (string, error) {
	plain := !hooked(sp, rel)
	st, stamped := stampOf(info)
	if sc != nil && plain && stamped {
		if rec, ok := sc.Files[rel]; ok && rec.fileStamp.reusable(st, rec.Hashed) {
			sc.next[rel] = rec
			return rec.Sum, nil
		}
	}
	start := time.Now().UnixNano()
	copyReads.Add(1)
	b, err := readLimited(f)
	if err != nil {
		return "", err
	}
	h := hashDestinationBytes(sp, rel, b)
	if sc != nil && plain && stamped && stableDuringRead(f, st) {
		sc.next[rel] = statRecord{fileStamp: st, Hashed: start, Sum: h}
		sc.dirty = true
	}
	return h, nil
}

func readLimited(f *os.File) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(f, hostprep.MaxReadFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > hostprep.MaxReadFileSize {
		return nil, hostprep.ErrFileTooLarge
	}
	return b, nil
}

// stableDuringRead reports whether f's descriptor stamp after a read is
// identical to before stamp, so the bytes read are the content before
// describes. Together with a hash time taken BEFORE the read (reusable
// demands both timestamps predate it by hashSettle), a write landing during
// a long read — even within one coarse timestamp tick — can never be recorded
// as reusable.
func stableDuringRead(f *os.File, before fileStamp) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	after, ok := stampOf(fi)
	return ok && after == before
}

// save writes the records this scan observed when they differ from what was
// loaded. Caller holds sp's agent lock.
func (sc *statCache) save(sp Spec) error {
	if sc == nil || (!sc.dirty && len(sc.next) == len(sc.Files)) {
		return nil
	}
	if err := os.MkdirAll(StateDir(), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(statCache{Version: statCacheVersion, Dir: sp.Dir, Files: sc.next})
	if err != nil {
		return err
	}
	return writeFile(statCachePath(sp), b, 0o644)
}

// ---- template side: per-run snapshot ------------------------------------------------

// templateSnap is one point-in-time read of a template. Unhooked files are
// stored as hashes, shared by every agent of the template; hooked exact
// entries are kept as raw bytes and run through each spec's own hooks.
type templateSnap struct {
	hashes map[string]string
	raw    map[string][]byte
}

// templateBuilds counts snapshot constructions (tests assert single init).
var templateBuilds atomic.Int64

// copyReads counts copy-side file reads that went through the stat cache (tests
// assert which scans reuse hashes).
var copyReads atomic.Int64

func buildTemplateSnap(sp Spec) *templateSnap {
	templateBuilds.Add(1)
	ts := &templateSnap{hashes: map[string]string{}, raw: map[string][]byte{}}
	add := func(p, rel string) {
		if hooked(sp, rel) {
			if b, ok := readTemplateRegular(p); ok {
				ts.raw[rel] = b
			}
			return
		}
		if h, ok := hashTemplateFile(p); ok {
			ts.hashes[rel] = h
		}
	}
	for _, e := range sp.Entries {
		base := e.src(sp)
		fi, err := os.Stat(base)
		if err != nil {
			continue
		}
		if !fi.IsDir() {
			add(base, e.Rel)
			continue
		}
		_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if skipDir(d) {
				return fs.SkipDir
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			sub, _ := filepath.Rel(base, p)
			add(p, filepath.Join(e.Rel, sub))
			return nil
		})
	}
	return ts
}

// hashesFor is the template's compared hashes for one spec.
func (ts *templateSnap) hashesFor(sp Spec) map[string]string {
	out := maps.Clone(ts.hashes)
	for rel, b := range ts.raw {
		// Hooks may rewrite their input in place; the raw bytes are shared by
		// every agent of this run, so each spec transforms its own copy.
		out[rel] = sum(transformTemplate(sp, rel, bytes.Clone(b)))
	}
	return out
}

// transformTemplate puts a template file's bytes through the spec's Seed (so
// they hash as a fresh copy would) and then Normalize.
func transformTemplate(sp Spec, rel string, b []byte) []byte {
	if e := entryFor(sp, rel); e.Rel == rel {
		if e.Seed != nil {
			b = e.Seed(sp, b)
		}
		if e.Normalize != nil {
			b = e.Normalize(sp, b)
		}
	}
	return b
}

// readTemplateRegular reads a template file that is a regular file (symlinks
// are not config we compare, matching what Scan always did).
func readTemplateRegular(path string) ([]byte, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	b, err := os.ReadFile(path)
	return b, err == nil
}

// liveTemplateHash re-reads the template's rel now, bypassing every cache.
// Scan uses it before advancing a baseline on agreement, so a run-scoped
// snapshot that the user's edits have overtaken never gets persisted.
func liveTemplateHash(sp Spec, rel string) (string, bool) {
	b, ok := readTemplateRegular(templatePath(sp, rel))
	if !ok {
		return "", false
	}
	return sum(transformTemplate(sp, rel, b)), true
}

// hashTemplateFile is sha256 of an unhooked template file, memoized in-process
// by path and stamp so the daemon's repeated polls cost a stat per file. The
// key is the path alone: unhooked hashes do not depend on the spec.
func hashTemplateFile(path string) (string, bool) {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	st, stamped := stampOf(fi)
	if stamped {
		hashMu.Lock()
		c, ok := hashCache[path]
		hashMu.Unlock()
		if ok && c.stamp.reusable(st, c.hashed) {
			return c.sum, true
		}
	}
	start := time.Now().UnixNano()
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return "", false
	}
	h := sum(b)
	// Memoize only when the opened file is the one Lstat described (no swap
	// between the two) and nothing changed during the read; the hash time is
	// the read's start, as for the copy side (see stableDuringRead).
	if stamped && stableDuringRead(f, st) {
		hashMu.Lock()
		if len(hashCache) > hashCacheMax {
			hashCache = map[string]hashEntry{} // crude bound; refills on the next scan
		}
		hashCache[path] = hashEntry{stamp: st, hashed: start, sum: h}
		hashMu.Unlock()
	}
	return h, true
}

type hashEntry struct {
	stamp  fileStamp
	hashed int64
	sum    string
}

// hashCacheMax bounds the memo so a machine with many agents and large plugin
// trees can't grow it without limit.
const hashCacheMax = 50000

var (
	hashMu    sync.Mutex
	hashCache = map[string]hashEntry{}
)

// ScanCache shares template snapshots across the scans of one run (one
// `amux sandbox drift` or doctor check), keyed by the template's path, harness
// kind and entry layout. Concurrent scans of the same template wait on a
// single construction. It is a point-in-time view: create one per run, never
// keep it across a Promote or Reset.
type ScanCache struct {
	mu    sync.Mutex
	cells map[string]*templateCell
}

type templateCell struct {
	once sync.Once
	snap *templateSnap
}

// NewScanCache returns an empty per-run cache.
func NewScanCache() *ScanCache { return &ScanCache{cells: map[string]*templateCell{}} }

// templateKey identifies everything a templateSnap's contents depend on: which
// files are read (template, kind, entry Rel/Src) and how they are partitioned
// (which exact entries are hooked).
func templateKey(sp Spec) string {
	var b strings.Builder
	b.WriteString(sp.Kind + "\x00" + sp.Template)
	for _, e := range sp.Entries {
		fmt.Fprintf(&b, "\x00%s\x01%s\x01%t", e.Rel, e.Src, e.Seed != nil || e.Normalize != nil)
	}
	return b.String()
}

func (c *ScanCache) template(sp Spec) *templateSnap {
	if c == nil {
		return buildTemplateSnap(sp)
	}
	key := templateKey(sp)
	c.mu.Lock()
	cell := c.cells[key]
	if cell == nil {
		cell = &templateCell{}
		c.cells[key] = cell
	}
	c.mu.Unlock()
	cell.once.Do(func() { cell.snap = buildTemplateSnap(sp) })
	return cell.snap
}

// Scan is the package Scan, sharing this run's template snapshots.
func (c *ScanCache) Scan(sp Spec) ([]Change, error) { return scan(c, sp) }

// ---- many agents --------------------------------------------------------------------

// ScanOptions tunes ScanAll.
type ScanOptions struct {
	// Workers bounds concurrent scans; <= 0 means DefaultWorkers().
	Workers int
	// Progress, when set, is called once with done=0 before any scan starts
	// (so a caller can show a hint at once) and then after each spec finishes
	// with the count done so far. Calls are serialized and done increases
	// monotonically.
	Progress func(done, total int)
}

// DefaultWorkers is the scan pool size: the CPU count, capped at 8 because
// scans are mostly I/O on one disk and more workers only add seek contention.
func DefaultWorkers() int { return max(1, min(runtime.GOMAXPROCS(0), 8)) }

// ScanResult is one spec's outcome, at the spec's index in ScanAll's input.
type ScanResult struct {
	Changes []Change
	Err     error
}

// ScanAll scans every spec with a bounded worker pool sharing one ScanCache.
// Results are in input order. It returns once every worker has exited.
func ScanAll(specs []Spec, opt ScanOptions) []ScanResult {
	out := make([]ScanResult, len(specs))
	workers := opt.Workers
	if workers <= 0 {
		workers = DefaultWorkers()
	}
	workers = min(workers, len(specs))
	cache := NewScanCache()
	if opt.Progress != nil && len(specs) > 0 {
		opt.Progress(0, len(specs))
	}
	var (
		next     atomic.Int64
		progress sync.Mutex
		done     int
		wg       sync.WaitGroup
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= len(specs) {
					return
				}
				out[i].Changes, out[i].Err = cache.Scan(specs[i])
				if opt.Progress != nil {
					progress.Lock()
					done++
					opt.Progress(done, len(specs))
					progress.Unlock()
				}
			}
		}()
	}
	wg.Wait()
	return out
}

// DriftSummary counts differences awaiting the user's decision across specs:
// how many agents (by AgentID) have any, and how many there are. A scan error
// fails the summary — never a clean count — reporting the first failing spec
// in input order.
func DriftSummary(specs []Spec, opt ScanOptions) (agents, edits int, err error) {
	results := ScanAll(specs, opt)
	perAgent := map[string]int{}
	var order []string
	for i, r := range results {
		if r.Err != nil {
			return 0, 0, fmt.Errorf("%s (%s): %w", specs[i].AgentID, specs[i].Kind, r.Err)
		}
		id := specs[i].AgentID
		if _, seen := perAgent[id]; !seen {
			order = append(order, id)
		}
		perAgent[id] += Pending(r.Changes)
	}
	for _, id := range order {
		if n := perAgent[id]; n > 0 {
			agents++
			edits += n
		}
	}
	return agents, edits, nil
}
