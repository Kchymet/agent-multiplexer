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

// tailSource is one open file of a session's stream: the spec that named it plus
// the read cursor and decode state the tail carries across polls.
type tailSource struct {
	sourceSpec
	mapper LineMapper
	offset int64
	info   os.FileInfo
}

func (s *tailSource) reset() {
	s.offset, s.mapper, s.info = 0, s.newMapper(), nil
}

// tail streams batches of structured events derived from a session's growing
// record files into out, until ctx is cancelled. Events are assigned a
// per-session monotonic ordinal (the seq); only events whose ordinal exceeds
// afterSeq are emitted, so a resuming orchestrator skips what it already has.
// Each poll's new complete lines are emitted as one batch (Seq = the last
// ordinal in the batch).
//
// A session normally has one file — the runtime's transcript — but Claude Code
// records no permission prompt in its transcript, so amux's own permission
// journal is read as a second source (permissions.go). Sources are polled in a
// fixed order and share one ordinal space, so the two kinds of event arrive on
// one stream with one resume cursor rather than two the consumer would have to
// merge.
//
// Tolerance:
//   - A file may not exist yet (a session with no transcript on disk, a session
//     never prompted for permission): tail polls until it appears or ctx ends —
//     honest degradation, never an error.
//   - Growth (append) is the common case: only bytes past the last complete line
//     are read; a partial trailing line (no newline yet) is left for the next poll.
//   - Truncation / rotation (size shrinks below our offset): the tail resets to
//     the file start and re-reads. Because the ordinal space is shared, a resync
//     of any source restarts them all and ordinals recount from 1; the
//     orchestrator dedups by ordinal, so a stable prefix re-sends idempotently.
func tail(ctx context.Context, rec Record, specs []sourceSpec, afterSeq int64, poll time.Duration, out chan<- harnessproto.RuntimeEventBatch) {
	defer close(out)
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	sources := make([]*tailSource, 0, len(specs))
	for _, sp := range specs {
		if sp.path == "" {
			continue
		}
		s := &tailSource{sourceSpec: sp}
		s.reset()
		sources = append(sources, s)
	}
	var ordinal int64
	resyncAll := func() {
		ordinal = 0
		for _, s := range sources {
			s.reset()
		}
	}

	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		// A sweep that hit a rotated or truncated source has restarted the shared
		// ordinal space, so everything it collected is renumbered: drop it and sweep
		// again from the first source, which reassigns ordinals in source order. A
		// second failure just waits for the next tick.
		var batch harnessproto.RuntimeEventBatch
		for attempt := 0; attempt < 2; attempt++ {
			batch = harnessproto.RuntimeEventBatch{}
			if sweep(sources, &ordinal, afterSeq, &batch, resyncAll) {
				break
			}
			batch = harnessproto.RuntimeEventBatch{}
		}
		if len(batch.Events) > 0 {
			batch.Runtime = rec.Runtime
			select {
			case out <- batch:
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// sweep reads each source once, in order, accumulating new events into batch
// under the shared ordinal. It returns false when a source turned out to have
// rotated or been truncated — it calls resyncAll and stops, leaving the caller to
// sweep again from the first source rather than emit a half-renumbered batch.
func sweep(sources []*tailSource, ordinal *int64, afterSeq int64, batch *harnessproto.RuntimeEventBatch, resyncAll func()) bool {
	for _, s := range sources {
		fi, err := os.Stat(s.path)
		if err != nil {
			continue
		}
		// Rotation (the path now names a different file — a fresh --resume rewrite)
		// or in-place truncation (size shrank below our offset): restart from the
		// top with fresh state.
		if (s.info != nil && !os.SameFile(s.info, fi)) || fi.Size() < s.offset {
			resyncAll()
			return false
		}
		s.info = fi
		if fi.Size() <= s.offset {
			continue
		}
		// A --resume rewrite replaces the record with a different file. If the OS
		// reuses the freed inode, os.SameFile above can't see the rotation and
		// fi.Size() may exceed our stale offset, so we would read from the middle of
		// a fresh line. Guard by integrity: our offset always points just past a
		// newline in the file we were reading; if the byte there is no longer a
		// newline, the file underneath us changed — resync from the top.
		if s.offset > 0 && !offsetOnLineBoundary(s.path, s.offset) {
			resyncAll()
			return false
		}
		s.offset = readFrom(s.path, s.offset, s.mapper, ordinal, afterSeq, batch)
	}
	return true
}

// readFrom reads complete newline-terminated lines starting at offset, maps each,
// advances *ordinal per emitted event, appends the events whose ordinal exceeds
// afterSeq to batch, and returns the new offset (past the last complete line). A
// partial trailing line is not consumed. batch is passed in rather than returned
// so one poll's sources accumulate into a single batch under one ordinal space.
func readFrom(path string, offset int64, mapper LineMapper, ordinal *int64, afterSeq int64, batch *harnessproto.RuntimeEventBatch) int64 {
	f, err := os.Open(path)
	if err != nil {
		return offset
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset
	}

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
	return consumed
}

// mapEachLine maps every complete line of a file through mapper and returns the
// events, for a caller replaying a record rather than tailing it (see
// PendingPermission). A missing or unreadable file yields nothing.
func mapEachLine(path string, mapper LineMapper) []harnessproto.RuntimeEvent {
	var ordinal int64
	var batch harnessproto.RuntimeEventBatch
	readFrom(path, 0, mapper, &ordinal, 0, &batch)
	return batch.Events
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
	// Permissions is amux's own permission journal for the session, read as a
	// second source alongside Path. It exists because a runtime may resolve
	// permission prompts entirely in its TUI without recording them (Claude Code
	// does), leaving amux's hooks as the only producer of the permission_request
	// events the `permission` verb correlates against. Empty when the runtime
	// records its prompts itself (Codex) or none is written.
	Permissions string
}

// Resolver resolves a published session id to its runtime record. ok=false ⇒ the
// session has no structured record (the provider advertises the feature but emits
// nothing for it — honest degradation, §4).
type Resolver func(sessionID string) (Record, bool)

// sourceSpec describes one file a session's events are read from: where it is,
// the factory for the mapper that decodes its lines, and whether that file can
// carry permission events (so a replay looking for the open request reads only
// the files that could hold one).
type sourceSpec struct {
	path       string
	newMapper  func() LineMapper
	permission bool
}

// sourcesFor returns the files a session's stream is read from, in the order
// they are polled, and whether the runtime has a reader at all. A runtime with no
// reader yields ok=false so the provider emits nothing for it rather than tailing
// a record it would only turn into noise.
//
// Claude Code's transcript records no permission prompt, so its prompts come from
// amux's journal alone; Codex writes its approval prompts into the rollout, so
// the rollout is itself a permission source.
func sourcesFor(rec Record) ([]sourceSpec, bool) {
	var out []sourceSpec
	switch rec.Runtime {
	case harnessproto.RuntimeClaude:
		out = append(out, sourceSpec{path: rec.Path, newMapper: func() LineMapper {
			st := &ClaudeState{}
			return func(line []byte) []harnessproto.RuntimeEvent { return MapClaudeLine(line, st) }
		}})
	case harnessproto.RuntimeCodex:
		out = append(out, sourceSpec{path: rec.Path, permission: true, newMapper: func() LineMapper {
			st := &CodexState{}
			return func(line []byte) []harnessproto.RuntimeEvent { return MapCodexLine(line, st) }
		}})
	default:
		return nil, false
	}
	if rec.Permissions != "" {
		out = append(out, sourceSpec{
			path: rec.Permissions, permission: true, newMapper: permissionMapper(rec.Runtime),
		})
	}
	return out, true
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
		specs, ok := sourcesFor(rec)
		if !ok {
			return nil, false
		}
		ch := make(chan harnessproto.RuntimeEventBatch, 8)
		go tail(ctx, rec, specs, afterSeq, poll, ch)
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
