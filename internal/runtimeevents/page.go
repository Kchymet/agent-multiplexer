package runtimeevents

import (
	"encoding/json"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// PageSourceKind identifies a daemon-authorized source without exposing its
// path. It is deliberately separate from sourceSpec: provider streaming and
// bounded file-RPC paging have different cursor contracts.
type PageSourceKind string

const (
	PageSourceStructured PageSourceKind = "structured"
	PageSourceClaude     PageSourceKind = "claude"
	PageSourceCodex      PageSourceKind = "codex"
	PageSourcePermission PageSourceKind = "permission"
	PageSourceJournal    PageSourceKind = "journal"
)

// PageDecoder carries the minimum state needed to map bounded chunks over
// multiple file-RPC pages. Callers must enforce MemoryBytes after each record.
// Clone makes a request retryable: cancellation never mutates the cached state.
type PageDecoder struct {
	runtime     string
	sources     []pageSourceDecoder
	occurrences *permissionOccurrenceTracker
	binder      *permissionEventBinder
}

type pageSourceDecoder struct {
	kind   PageSourceKind
	claude *ClaudeState
	codex  *CodexState
}

// NewPageDecoder constructs one decoder for the fixed ordered source list.
func NewPageDecoder(runtime string, kinds []PageSourceKind) (*PageDecoder, bool) {
	d := &PageDecoder{
		runtime: runtime, occurrences: newPermissionOccurrenceTracker(), binder: newPermissionEventBinder(),
		sources: make([]pageSourceDecoder, len(kinds)),
	}
	for i, kind := range kinds {
		s := pageSourceDecoder{kind: kind}
		switch kind {
		case PageSourceStructured, PageSourcePermission, PageSourceJournal:
		case PageSourceClaude:
			s.claude = &ClaudeState{}
		case PageSourceCodex:
			s.codex = &CodexState{}
		default:
			return nil, false
		}
		d.sources[i] = s
	}
	return d, true
}

// Map maps one complete source record and applies the same permission
// occurrence/generation rules as provider streaming. Bindings must come only
// from the daemon's current runtime incarnation.
func (d *PageDecoder) Map(source int, line []byte, bindings map[string]string) []harnessproto.RuntimeEvent {
	if d == nil || source < 0 || source >= len(d.sources) {
		return nil
	}
	s := &d.sources[source]
	var events []harnessproto.RuntimeEvent
	switch s.kind {
	case PageSourceStructured:
		events = structuredLine(line)
	case PageSourceClaude:
		events = MapClaudeLine(line, s.claude)
	case PageSourceCodex:
		events = MapCodexLine(line, s.codex)
	case PageSourcePermission:
		events = MapPermissionLine(d.runtime, line)
	case PageSourceJournal:
		events = MapJournalLine(d.runtime, line)
	}
	events = d.occurrences.decorate(events)
	d.binder.seed(bindings)
	return d.binder.bind(events, bindings)
}

// Clone returns a deep copy suitable for an isolated page attempt.
func (d *PageDecoder) Clone() *PageDecoder {
	if d == nil {
		return nil
	}
	out := &PageDecoder{runtime: d.runtime, sources: make([]pageSourceDecoder, len(d.sources))}
	for i, s := range d.sources {
		out.sources[i].kind = s.kind
		if s.claude != nil {
			out.sources[i].claude = &ClaudeState{tools: cloneToolInfoMap(s.claude.tools)}
		}
		if s.codex != nil {
			out.sources[i].codex = &CodexState{
				calls: cloneToolInfoMap(s.codex.calls), approvals: append([]string(nil), s.codex.approvals...),
			}
		}
	}
	out.occurrences = &permissionOccurrenceTracker{
		next: cloneIntMap(d.occurrences.next), open: cloneStringSliceMap(d.occurrences.open),
	}
	out.binder = &permissionEventBinder{active: cloneStringMap(d.binder.active)}
	return out
}

// MemoryBytes is a conservative accounting estimate for cache admission. It
// includes retained raw tool inputs and map/string overhead, not Go allocator
// implementation details; the pager applies a lower hard cap to leave headroom.
func (d *PageDecoder) MemoryBytes() int {
	if d == nil {
		return 0
	}
	n := 256 + len(d.runtime) + len(d.sources)*64
	for _, s := range d.sources {
		n += len(s.kind)
		if s.claude != nil {
			n += toolInfoMapBytes(s.claude.tools)
		}
		if s.codex != nil {
			n += toolInfoMapBytes(s.codex.calls) + stringSliceBytes(s.codex.approvals)
		}
	}
	for key := range d.occurrences.next {
		n += 48 + len(key)
	}
	for key, values := range d.occurrences.open {
		n += 48 + len(key) + stringSliceBytes(values)
	}
	for key, value := range d.binder.active {
		n += 48 + len(key) + len(value)
	}
	return n
}

func cloneToolInfoMap(in map[string]toolInfo) map[string]toolInfo {
	if in == nil {
		return nil
	}
	out := make(map[string]toolInfo, len(in))
	for key, value := range in {
		out[key] = toolInfo{name: value.name, input: append(json.RawMessage(nil), value.input...)}
	}
	return out
}

func cloneIntMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneStringSliceMap(in map[string][]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for key, value := range in {
		out[key] = append([]string(nil), value...)
	}
	return out
}

func toolInfoMapBytes(values map[string]toolInfo) int {
	n := 0
	for key, value := range values {
		n += 64 + len(key) + len(value.name) + len(value.input)
	}
	return n
}

func stringSliceBytes(values []string) int {
	n := len(values) * 16
	for _, value := range values {
		n += len(value)
	}
	return n
}
