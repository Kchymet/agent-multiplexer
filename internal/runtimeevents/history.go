package runtimeevents

import (
	"context"
	"errors"
	"os"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// history.go answers runtime-events-history (docs/remote-provider-sessions.md
// §4.7): one bounded page of a session's past events, newest first by request
// order, in the same ordinal space a fresh runtime-events subscription assigns.
// A consumer opens a transcript with the newest page, subscribes with afterSeq =
// the page's head to follow it live, and asks for older pages only as the reader
// scrolls back — instead of replaying the whole record before showing anything.
//
// A page is served from the per-file indexes (index.go): finding its first event
// restores the nearest checkpoint, so a page costs at most one checkpoint
// interval of decoding beyond its own events, wherever it sits in the record.

const (
	// DefaultHistoryLimit is the page size when a request names none.
	DefaultHistoryLimit = 200
	// MaxHistoryLimit caps a requested page size.
	MaxHistoryLimit = 1000
	// maxHistoryPayloadBytes bounds a page's payload; the oldest events are left
	// for the next page when it would be exceeded (a page always has one event).
	maxHistoryPayloadBytes = 4 << 20
)

// HistoryContext builds the provider's runtime-events-history source: given a
// resolver from session id to its runtime record, it returns the page of at most
// limit events whose ordinals end just before beforeSeq (beforeSeq <= 0 means the
// newest page). ok=false when the session has no resolvable record or names a
// runtime this package has no reader for — the same cases Stream refuses.
func HistoryContext(resolve ContextResolver) func(ctx context.Context, sessionID string, beforeSeq int64, limit int) (harnessproto.RuntimeEventHistory, bool) {
	return func(ctx context.Context, sessionID string, beforeSeq int64, limit int) (harnessproto.RuntimeEventHistory, bool) {
		rec, ok := resolve(ctx, sessionID)
		if !ok || (rec.Path == "" && rec.Journal == "") {
			return harnessproto.RuntimeEventHistory{}, false
		}
		page, err := History(rec, beforeSeq, limit)
		return page, err == nil
	}
}

// History reads one page of rec's events; see HistoryContext. Permission events
// are bound with rec.PermissionBindings exactly as the stream binds them, so the
// newest page of a session waiting on an approval carries its answerable tuple.
func History(rec Record, beforeSeq int64, limit int) (harnessproto.RuntimeEventHistory, error) {
	specs, ok := sourcesFor(rec)
	if !ok {
		return harnessproto.RuntimeEventHistory{}, errors.New("runtime has no reader")
	}
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	limit = min(limit, MaxHistoryLimit)
	var page harnessproto.RuntimeEventHistory
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if page, err = historyPage(specs, beforeSeq, limit); !errors.Is(err, errIndexStale) {
			break
		}
	}
	if err != nil {
		return harnessproto.RuntimeEventHistory{}, err
	}
	page.Runtime = rec.Runtime
	if len(page.Events) > 0 {
		binder := newPermissionEventBinder()
		page.Events = binder.bind(page.Events, rec.PermissionBindings)
	}
	return page, nil
}

type openSource struct {
	f    *os.File
	ix   *sourceIndex
	base int64 // session ordinal before this source's first event
	n    int64
}

func historyPage(specs []sourceSpec, beforeSeq int64, limit int) (harnessproto.RuntimeEventHistory, error) {
	var opened []openSource
	defer func() {
		for _, o := range opened {
			_ = o.f.Close()
		}
	}()
	var head int64
	for _, sp := range specs {
		if sp.path == "" {
			continue
		}
		f, ix, snap, err := openIndexed(sp)
		if err != nil {
			return harnessproto.RuntimeEventHistory{}, err
		}
		if f == nil {
			continue
		}
		opened = append(opened, openSource{f: f, ix: ix, base: head, n: snap.events})
		head += snap.events
	}
	hi := head
	if beforeSeq > 0 && beforeSeq-1 < hi {
		hi = beforeSeq - 1
	}
	page := harnessproto.RuntimeEventHistory{Head: head}
	if hi < 1 {
		return page, nil
	}
	lo := max(1, hi-int64(limit)+1)
	events := make([]harnessproto.RuntimeEvent, 0, hi-lo+1)
	for _, o := range opened {
		from, to := max(lo, o.base+1), min(hi, o.base+o.n)
		if from > to {
			continue
		}
		got, err := o.ix.readRange(o.f, from-o.base-1, to-o.base)
		if err != nil {
			if errors.Is(err, errIndexStale) {
				o.ix.invalidate()
			}
			return harnessproto.RuntimeEventHistory{}, err
		}
		events = append(events, got...)
	}
	// Keep the newest events within the payload budget; the dropped older ones
	// are the next page's.
	total, start := 0, len(events)
	for start > 0 {
		size := eventSize(events[start-1])
		if start < len(events) && total+size > maxHistoryPayloadBytes {
			break
		}
		total += size
		start--
	}
	page.Events = events[start:]
	page.Seq = hi
	page.HasMore = hi-int64(len(page.Events)) >= 1
	return page, nil
}

func eventSize(ev harnessproto.RuntimeEvent) int {
	return len(ev.Payload) + len(ev.Type) + len(ev.ItemID) + 48
}
