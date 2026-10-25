package runtimeevents

import (
	"bufio"
	"bytes"
	"container/list"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// index.go keeps a per-record-file index so a reader never re-parses a whole
// transcript to answer one request. Runtime records are append-only JSONL that
// reach tens of megabytes, and the readers are stateful (a tool_result needs the
// input of its earlier tool_use), so a line in the middle cannot be decoded on
// its own. The index therefore stores sparse checkpoints — a line-boundary byte
// offset, the number of normalized events before it, and a clone of the decoder
// state at that point — every indexCheckpointBytes of source or
// indexCheckpointEvents events. Any event ordinal is then reachable by
// restoring the nearest earlier checkpoint and decoding forward at most one
// interval.
//
// The index is keyed by path and decoder, and validated against the file's
// identity, size, mtime and the bytes just before its indexed end:
//   - unchanged identity, size and mtime: no read at all;
//   - growth (append): only the new tail is scanned, from the indexed end;
//   - a partial trailing line (no newline yet) is not indexed until it completes;
//   - a different file (rotation, a --resume rewrite), a size below the indexed
//     end (truncation), or a changed anchor (in-place rewrite, even one that
//     reused the inode) discards the index and rebuilds it from the top.
//
// The index also carries the permission requests still open at its end, which is
// what OpenPermissions answers — previously a full replay per call.

const (
	indexCheckpointBytes  = 256 << 10
	indexCheckpointEvents = 512
	indexAnchorBytes      = 32
	indexCacheEntries     = 64
)

var errIndexStale = errors.New("runtime record changed while reading")

type indexCheckpoint struct {
	offset int64 // just past a newline (or 0)
	events int64 // events produced by every line before offset
	dec    *sourceDecoder
}

// sourceIndex is the index of one record file. Its methods are safe for
// concurrent use; reads hold the lock so checkpoints never move under them.
type sourceIndex struct {
	mu      sync.Mutex
	kind    PageSourceKind
	runtime string

	identity os.FileInfo
	size     int64
	modTime  time.Time
	end      int64 // offset just past the last complete, indexed line
	anchor   []byte
	events   int64
	dec      *sourceDecoder // state at end
	open     []Pending      // permission requests open at end
	cps      []indexCheckpoint
	sinceCP  int64 // bytes since the last checkpoint
	eventsCP int64 // events since the last checkpoint
	scanned  int64 // source bytes decoded over this index's lifetime (tests/bench)
}

func newSourceIndex(kind PageSourceKind, runtime string) *sourceIndex {
	ix := &sourceIndex{kind: kind, runtime: runtime}
	ix.resetLocked()
	return ix
}

func (ix *sourceIndex) resetLocked() {
	ix.identity, ix.size, ix.modTime, ix.end, ix.anchor, ix.events = nil, 0, time.Time{}, 0, nil, 0
	ix.dec = newSourceDecoder(ix.kind, ix.runtime)
	ix.open = nil
	ix.cps = []indexCheckpoint{{offset: 0, events: 0, dec: ix.dec.clone()}}
	ix.sinceCP, ix.eventsCP = 0, 0
}

// indexSnapshot is what a caller needs from one update without holding the lock.
type indexSnapshot struct {
	identity os.FileInfo
	end      int64
	events   int64
}

// update brings the index up to date with f (an open handle on the indexed
// path) and returns its extent. Only bytes past the indexed end are decoded.
func (ix *sourceIndex) update(f *os.File) (indexSnapshot, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.updateLocked(f)
}

func (ix *sourceIndex) updateLocked(f *os.File) (indexSnapshot, error) {
	fi, err := f.Stat()
	if err != nil {
		return indexSnapshot{}, err
	}
	if ix.identity != nil {
		unchanged := os.SameFile(ix.identity, fi) && fi.Size() == ix.size && fi.ModTime().Equal(ix.modTime)
		if !unchanged && !ix.stillPrefixLocked(f, fi) {
			ix.resetLocked()
		}
	}
	ix.identity, ix.size, ix.modTime = fi, fi.Size(), fi.ModTime()
	if fi.Size() > ix.end {
		if err := ix.scanLocked(f, fi.Size()); err != nil {
			ix.resetLocked()
			return indexSnapshot{}, err
		}
	}
	return indexSnapshot{identity: ix.identity, end: ix.end, events: ix.events}, nil
}

// stillPrefixLocked reports whether the indexed bytes are still the start of fi:
// the same file, not shorter than what was indexed, and with the same bytes just
// before the indexed end.
func (ix *sourceIndex) stillPrefixLocked(f *os.File, fi os.FileInfo) bool {
	if !os.SameFile(ix.identity, fi) || fi.Size() < ix.end {
		return false
	}
	if len(ix.anchor) == 0 {
		return ix.end == 0
	}
	got := make([]byte, len(ix.anchor))
	if _, err := f.ReadAt(got, ix.end-int64(len(got))); err != nil {
		return false
	}
	return bytes.Equal(got, ix.anchor)
}

// scanLocked decodes complete lines from the indexed end up to size.
func (ix *sourceIndex) scanLocked(f *os.File, size int64) error {
	r := bufio.NewReaderSize(io.NewSectionReader(f, ix.end, size-ix.end), 64<<10)
	var last []byte
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break // a partial trailing line waits for its newline
			}
			return err
		}
		n := int64(len(line))
		events := ix.dec.mapLine(line)
		ix.open = applyPending(ix.open, events)
		ix.end += n
		ix.scanned += n
		ix.events += int64(len(events))
		ix.sinceCP += n
		ix.eventsCP += int64(len(events))
		last = line
		if ix.sinceCP >= indexCheckpointBytes || ix.eventsCP >= indexCheckpointEvents {
			ix.cps = append(ix.cps, indexCheckpoint{offset: ix.end, events: ix.events, dec: ix.dec.clone()})
			ix.sinceCP, ix.eventsCP = 0, 0
		}
	}
	if last != nil {
		if len(last) >= indexAnchorBytes {
			ix.anchor = append([]byte(nil), last[len(last)-indexAnchorBytes:]...)
		} else {
			anchor := make([]byte, min(int64(indexAnchorBytes), ix.end))
			if _, err := f.ReadAt(anchor, ix.end-int64(len(anchor))); err != nil {
				return err
			}
			ix.anchor = anchor
		}
	}
	return nil
}

// checkpointFor returns the latest checkpoint with at most events before it.
// The returned decoder is the index's own; clone it before use.
func (ix *sourceIndex) checkpointForLocked(events int64) indexCheckpoint {
	i := sort.Search(len(ix.cps), func(i int) bool { return ix.cps[i].events > events })
	return ix.cps[i-1] // cps[0].events == 0, so i >= 1
}

// position returns where a reader should start to emit events after the first
// skip events of this file: the checkpoint offset, the events before it, and a
// private copy of the decoder state there.
func (ix *sourceIndex) position(skip int64) (offset, events int64, dec *sourceDecoder) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if skip >= ix.events {
		return ix.end, ix.events, ix.dec.clone()
	}
	cp := ix.checkpointForLocked(skip)
	return cp.offset, cp.events, cp.dec.clone()
}

// readRange decodes the events with file-local zero-based ordinals in [from,
// to), reading from the nearest checkpoint at or before from and stopping at the
// line that produces event to-1. It errors with errIndexStale when the file no
// longer matches the index; the caller rebuilds and retries.
func (ix *sourceIndex) readRange(f *os.File, from, to int64) ([]harnessproto.RuntimeEvent, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if from < 0 || to > ix.events || from >= to {
		return nil, nil
	}
	cp := ix.checkpointForLocked(from)
	dec := cp.dec.clone()
	r := bufio.NewReaderSize(io.NewSectionReader(f, cp.offset, ix.end-cp.offset), 64<<10)
	ordinal := cp.events
	out := make([]harnessproto.RuntimeEvent, 0, to-from)
	for ordinal < to {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, errIndexStale
		}
		ix.scanned += int64(len(line))
		for _, ev := range dec.mapLine(line) {
			if ordinal >= from && ordinal < to {
				out = append(out, ev)
			}
			ordinal++
		}
	}
	if int64(len(out)) != to-from {
		return nil, errIndexStale
	}
	return out, nil
}

// invalidate discards the index; the next update rebuilds it from the top.
func (ix *sourceIndex) invalidate() {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.resetLocked()
}

// openPending returns a copy of the permission requests open at the indexed end.
func (ix *sourceIndex) openPending() []Pending {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return append([]Pending(nil), ix.open...)
}

// applyPending folds decorated events into the list of open requests: a request
// opens a Pending keyed by its occurrence, a resolution closes that occurrence.
func applyPending(open []Pending, events []harnessproto.RuntimeEvent) []Pending {
	for _, ev := range events {
		switch ev.Type {
		case TypePermissionRequest:
			var body struct {
				RequestID string   `json:"request_id"`
				Tool      string   `json:"tool"`
				Action    string   `json:"action"`
				Options   []string `json:"options"`
			}
			if json.Unmarshal(ev.Payload, &body) != nil || body.RequestID == "" {
				continue
			}
			open = append(open, Pending{
				RequestID: body.RequestID, Occurrence: ev.ItemID, Tool: body.Tool,
				Action: body.Action, Options: body.Options,
			})
		case TypePermissionResolved:
			for i, o := range open {
				if o.Occurrence == ev.ItemID {
					open = append(open[:i], open[i+1:]...)
					break
				}
			}
		}
	}
	return open
}

// ── cache ────────────────────────────────────────────────────────────────────

type indexKey struct {
	path    string
	kind    PageSourceKind
	runtime string
}

// indexCache is a small LRU of source indexes shared by every reader in the
// process (stream subscriptions, history pages, permission lookups).
type indexCache struct {
	mu      sync.Mutex
	max     int
	order   *list.List // front = most recently used; values are indexKey
	entries map[indexKey]*indexCacheEntry
}

type indexCacheEntry struct {
	ix   *sourceIndex
	elem *list.Element
}

func newIndexCache(max int) *indexCache {
	return &indexCache{max: max, order: list.New(), entries: make(map[indexKey]*indexCacheEntry)}
}

var defaultIndexCache = newIndexCache(indexCacheEntries)

func (c *indexCache) get(key indexKey) *sourceIndex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e := c.entries[key]; e != nil {
		c.order.MoveToFront(e.elem)
		return e.ix
	}
	e := &indexCacheEntry{ix: newSourceIndex(key.kind, key.runtime)}
	e.elem = c.order.PushFront(key)
	c.entries[key] = e
	for c.order.Len() > c.max {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(indexKey))
	}
	return e.ix
}

// indexFor returns the cached index for one source spec.
func indexFor(sp sourceSpec) *sourceIndex {
	return defaultIndexCache.get(indexKey{path: sp.path, kind: sp.kind, runtime: sp.runtime})
}

// openIndexed opens a source and brings its index up to date. A missing file is
// reported as (nil, nil, zero, nil): it simply has no events yet.
func openIndexed(sp sourceSpec) (*os.File, *sourceIndex, indexSnapshot, error) {
	f, err := os.Open(sp.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, indexSnapshot{}, nil
		}
		return nil, nil, indexSnapshot{}, err
	}
	ix := indexFor(sp)
	snap, err := ix.update(f)
	if err != nil {
		_ = f.Close()
		return nil, nil, indexSnapshot{}, err
	}
	return f, ix, snap, nil
}
