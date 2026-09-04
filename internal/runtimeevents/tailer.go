package runtimeevents

import (
	"bufio"
	"context"
	"io"
	"os"
	"time"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// DefaultPollInterval is how often a tail re-stats its record file for growth.
const DefaultPollInterval = time.Second

// LineMapper maps one record line to zero or more structured events. It carries
// per-session decode state opaque to the tailer (e.g. *ClaudeState).
type LineMapper func(line []byte) []harnessproto.RuntimeEvent

// tail streams batches of structured events derived from a growing record file
// into out, until ctx is cancelled. Events are assigned a per-session monotonic
// ordinal (the seq); only events whose ordinal exceeds afterSeq are emitted, so a
// resuming orchestrator skips what it already has. Each poll's new complete lines
// are emitted as one batch (Seq = the last ordinal in the batch).
//
// Tolerance:
//   - The file may not exist yet (a session with no transcript on disk): tail
//     polls until it appears or ctx ends — honest degradation, never an error.
//   - Growth (append) is the common case: only bytes past the last complete line
//     are read; a partial trailing line (no newline yet) is left for the next poll.
//   - Truncation / rotation (size shrinks below our offset): the tail resets to
//     the file start and re-reads. Ordinals recount from 1; the orchestrator
//     dedups by ordinal, so a stable prefix re-sends idempotently.
func tail(ctx context.Context, rec Record, newMapper func() LineMapper, afterSeq int64, poll time.Duration, out chan<- harnessproto.RuntimeEventBatch) {
	defer close(out)
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	path := rec.Path
	var (
		offset   int64
		ordinal  int64
		mapper   = newMapper()
		lastInfo os.FileInfo
	)
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		fi, err := os.Stat(path)
		if err == nil {
			// Rotation (the path now names a different file — a fresh --resume
			// rewrite) or in-place truncation (size shrank below our offset): restart
			// from the top with fresh state. Ordinals recount; the orchestrator dedups
			// by ordinal, so a stable prefix re-sends idempotently.
			rotated := lastInfo != nil && !os.SameFile(lastInfo, fi)
			if rotated || fi.Size() < offset {
				offset, ordinal, mapper = 0, 0, newMapper()
			}
			lastInfo = fi
			if fi.Size() > offset {
				// A --resume rewrite replaces the record with a different file. If the
				// OS reuses the freed inode, os.SameFile above can't see the rotation
				// and fi.Size() may exceed our stale offset, so we'd read from the
				// middle of a fresh line. Guard by integrity: our offset always points
				// just past a newline in the file we were reading; if the byte there is
				// no longer a newline, the file underneath us changed — resync from the
				// top (ordinals recount; the orchestrator dedups by ordinal).
				if offset > 0 && !offsetOnLineBoundary(path, offset) {
					offset, ordinal, mapper = 0, 0, newMapper()
				}
				newOffset, batch := readFrom(path, offset, mapper, &ordinal, afterSeq)
				batch.Runtime = rec.Runtime
				offset = newOffset
				if len(batch.Events) > 0 {
					select {
					case out <- batch:
					case <-ctx.Done():
						return
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// readFrom reads complete newline-terminated lines starting at offset, maps each,
// advances *ordinal per emitted event, and returns the new offset (past the last
// complete line) plus a batch of events whose ordinal exceeds afterSeq. A partial
// trailing line is not consumed.
func readFrom(path string, offset int64, mapper LineMapper, ordinal *int64, afterSeq int64) (int64, harnessproto.RuntimeEventBatch) {
	f, err := os.Open(path)
	if err != nil {
		return offset, harnessproto.RuntimeEventBatch{}
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, harnessproto.RuntimeEventBatch{}
	}

	var batch harnessproto.RuntimeEventBatch
	consumed := offset
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			// No trailing newline: this is a partial line still being written.
			// Leave it unconsumed so the next poll re-reads it whole.
			break
		}
		consumed += int64(len(line))
		for _, ev := range mapper(line) {
			*ordinal++
			if *ordinal > afterSeq {
				batch.Events = append(batch.Events, ev)
				batch.Seq = *ordinal
			}
		}
	}
	return consumed, batch
}

// offsetOnLineBoundary reports whether the byte immediately before offset is a
// newline — i.e. offset still points just past a complete line in the current
// file. readFrom only ever advances offset past a '\n', so a false here means the
// file was replaced out from under us (a --resume rewrite that reused the inode).
// Any read error is treated as "not a boundary" so the caller resyncs safely.
func offsetOnLineBoundary(path string, offset int64) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], offset-1); err != nil {
		return false
	}
	return b[0] == '\n'
}

// PathResolver resolves a published session id to its on-disk runtime record
// path. ok=false ⇒ the session has no structured record (the provider advertises
// the feature but emits nothing for it — honest degradation, §4).
type PathResolver func(sessionID string) (path string, ok bool)

// Record is one session's on-disk runtime record: which runtime wrote it and
// where. Runtime is a harnessproto.Runtime* name; it selects the reader and is
// stamped on every batch so the consumer never has to assume a runtime.
type Record struct {
	Runtime string
	Path    string
}

// Resolver resolves a published session id to its runtime record. ok=false ⇒ the
// session has no structured record (the provider advertises the feature but emits
// nothing for it — honest degradation, §4).
type Resolver func(sessionID string) (Record, bool)

// mapperFor returns the per-session line mapper factory for a runtime, and
// whether the runtime has a reader at all. A runtime with no reader yields
// ok=false so the provider emits nothing for it rather than tailing a record it
// would only turn into noise.
func mapperFor(runtime string) (func() LineMapper, bool) {
	switch runtime {
	case harnessproto.RuntimeClaude:
		return func() LineMapper {
			st := &ClaudeState{}
			return func(line []byte) []harnessproto.RuntimeEvent { return MapClaudeLine(line, st) }
		}, true
	case harnessproto.RuntimeCodex:
		return func() LineMapper {
			st := &CodexState{}
			return func(line []byte) []harnessproto.RuntimeEvent { return MapCodexLine(line, st) }
		}, true
	default:
		return nil, false
	}
}

// Stream builds the runtime-events source the provider's RuntimeEventStream hook
// expects: given a resolver from session id to its runtime record, it picks the
// reader for that record's runtime and tails it from afterSeq, streaming mapped
// event batches until ctx is cancelled. ok=false when the session has no
// resolvable record, or names a runtime this package has no reader for.
func Stream(resolve Resolver, poll time.Duration) func(ctx context.Context, sessionID string, afterSeq int64) (<-chan harnessproto.RuntimeEventBatch, bool) {
	return func(ctx context.Context, sessionID string, afterSeq int64) (<-chan harnessproto.RuntimeEventBatch, bool) {
		rec, ok := resolve(sessionID)
		if !ok || rec.Path == "" {
			return nil, false
		}
		newMapper, ok := mapperFor(rec.Runtime)
		if !ok {
			return nil, false
		}
		ch := make(chan harnessproto.RuntimeEventBatch, 8)
		go tail(ctx, rec, newMapper, afterSeq, poll, ch)
		return ch, true
	}
}

// ClaudeStream is Stream pinned to the Claude reader, for a caller that resolves
// only Claude transcripts.
func ClaudeStream(resolve PathResolver, poll time.Duration) func(ctx context.Context, sessionID string, afterSeq int64) (<-chan harnessproto.RuntimeEventBatch, bool) {
	return Stream(func(sessionID string) (Record, bool) {
		path, ok := resolve(sessionID)
		return Record{Runtime: harnessproto.RuntimeClaude, Path: path}, ok
	}, poll)
}
