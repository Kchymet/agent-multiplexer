package runtimeevents

import (
	"bufio"
	"context"
	"io"
	"os"
	"time"

	"amux/internal/harnessproto"
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
func tail(ctx context.Context, path string, newMapper func() LineMapper, afterSeq int64, poll time.Duration, out chan<- harnessproto.RuntimeEventBatch) {
	defer close(out)
	if poll <= 0 {
		poll = DefaultPollInterval
	}
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
				newOffset, batch := readFrom(path, offset, mapper, &ordinal, afterSeq)
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

// PathResolver resolves a published session id to its on-disk runtime record
// path. ok=false ⇒ the session has no structured record (the provider advertises
// the feature but emits nothing for it — honest degradation, §4).
type PathResolver func(sessionID string) (path string, ok bool)

// ClaudeStream builds a runtime-events source for Claude Code sessions: given a
// resolver from session id to its JSONL path, it returns a function matching the
// provider's RuntimeEventStream hook — for each subscribed session it tails the
// JSONL from afterSeq and streams mapped event batches until ctx is cancelled.
// ok=false when the session has no resolvable record.
func ClaudeStream(resolve PathResolver, poll time.Duration) func(ctx context.Context, sessionID string, afterSeq int64) (<-chan harnessproto.RuntimeEventBatch, bool) {
	return func(ctx context.Context, sessionID string, afterSeq int64) (<-chan harnessproto.RuntimeEventBatch, bool) {
		path, ok := resolve(sessionID)
		if !ok || path == "" {
			return nil, false
		}
		ch := make(chan harnessproto.RuntimeEventBatch, 8)
		go tail(ctx, path, func() LineMapper {
			st := &ClaudeState{}
			return func(line []byte) []harnessproto.RuntimeEvent { return MapClaudeLine(line, st) }
		}, afterSeq, poll, ch)
		return ch, true
	}
}
