package runtimeevents

import (
	"context"
	"errors"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// Per-source reads. History and Stream number a session's events in ONE joined
// ordinal space by concatenating its sources (transcript, permission journal,
// amux journal) at cumulative offsets, so an append to an earlier source
// shifts every later source's ordinals. A consumer that needs a durable cursor
// or a stable event identity (direct wake-ups) reads each source by its own
// index instead: within one append-only source an event's 1-based index never
// changes.

// SourceEvents is one source's slice of a session's events.
type SourceEvents struct {
	// Kind names the source (a PageSourceKind); it is unique within a record.
	Kind string
	// Head is the number of events the source holds now.
	Head int64
	// First is the 1-based index of Events[0] within this source.
	First  int64
	Events []harnessproto.RuntimeEvent
}

// SourcesPage is one read across a session's sources, in source order.
type SourcesPage struct {
	Runtime string
	Sources []SourceEvents
}

// ReadSourcesContext resolves a session's record and reads up to limit events
// from each source after the given per-source index (absent = 0).
func ReadSourcesContext(resolve ContextResolver) func(ctx context.Context, sessionID string, after map[string]int64, limit int) (SourcesPage, bool) {
	return func(ctx context.Context, sessionID string, after map[string]int64, limit int) (SourcesPage, bool) {
		rec, ok := resolve(ctx, sessionID)
		if !ok || (rec.Path == "" && rec.Journal == "") {
			return SourcesPage{}, false
		}
		page, err := ReadSources(rec, after, limit)
		return page, err == nil
	}
}

// ReadSources reads rec's sources independently. A source whose Head is below
// the caller's cursor was replaced or truncated; the caller decides what that
// means for its cursor. Permission events are bound exactly as History binds
// them.
func ReadSources(rec Record, after map[string]int64, limit int) (SourcesPage, error) {
	specs, ok := sourcesFor(rec)
	if !ok {
		return SourcesPage{}, errors.New("runtime has no reader")
	}
	if limit <= 0 {
		limit = DefaultHistoryLimit
	}
	limit = min(limit, MaxHistoryLimit)
	var page SourcesPage
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if page, err = readSources(specs, after, limit); !errors.Is(err, errIndexStale) {
			break
		}
	}
	if err != nil {
		return SourcesPage{}, err
	}
	page.Runtime = rec.Runtime
	for i := range page.Sources {
		if len(page.Sources[i].Events) > 0 {
			binder := newPermissionEventBinder()
			page.Sources[i].Events = binder.bind(page.Sources[i].Events, rec.PermissionBindings)
		}
	}
	return page, nil
}

func readSources(specs []sourceSpec, after map[string]int64, limit int) (SourcesPage, error) {
	var page SourcesPage
	for _, sp := range specs {
		if sp.path == "" {
			continue
		}
		f, ix, snap, err := openIndexed(sp)
		if err != nil {
			return SourcesPage{}, err
		}
		kind := string(sp.kind)
		if f == nil {
			page.Sources = append(page.Sources, SourceEvents{Kind: kind})
			continue
		}
		src := SourceEvents{Kind: kind, Head: snap.events}
		from := max(after[kind], 0)
		if from < snap.events {
			to := min(snap.events, from+int64(limit))
			got, err := ix.readRange(f, from, to)
			if err != nil {
				_ = f.Close()
				if errors.Is(err, errIndexStale) {
					ix.invalidate()
				}
				return SourcesPage{}, err
			}
			src.First, src.Events = from+1, got
		}
		_ = f.Close()
		page.Sources = append(page.Sources, src)
	}
	return page, nil
}
