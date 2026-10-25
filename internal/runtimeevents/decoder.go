package runtimeevents

import (
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// sourceDecoder is the complete decode state of one record file: the runtime
// reader's own state (open tool calls, open Codex approvals) plus the permission
// occurrence tracker that stamps each request with its stable occurrence ItemID.
// Unlike a LineMapper closure it can be cloned, which is what lets the index
// (index.go) checkpoint it at line boundaries and resume decoding from the
// middle of a file with exactly the state a from-the-top replay would have.
//
// The occurrence tracker is per source. The stream used to share one across a
// session's sources, but at most one source of any record can carry permission
// events (sourcesFor: the structured log, Codex's rollout, or Claude's
// permission journal — never two), so a per-source tracker assigns the same
// occurrences while keeping each file's state independent of its siblings'.
type sourceDecoder struct {
	kind    PageSourceKind
	runtime string
	claude  *ClaudeState
	codex   *CodexState
	occ     *permissionOccurrenceTracker
}

func newSourceDecoder(kind PageSourceKind, runtime string) *sourceDecoder {
	d := &sourceDecoder{kind: kind, runtime: runtime, occ: newPermissionOccurrenceTracker()}
	switch kind {
	case PageSourceClaude:
		d.claude = &ClaudeState{}
	case PageSourceCodex:
		d.codex = &CodexState{}
	}
	return d
}

// mapLine maps one complete record line and stamps permission occurrences.
func (d *sourceDecoder) mapLine(line []byte) []harnessproto.RuntimeEvent {
	var events []harnessproto.RuntimeEvent
	switch d.kind {
	case PageSourceStructured:
		events = structuredLine(line)
	case PageSourceClaude:
		events = MapClaudeLine(line, d.claude)
	case PageSourceCodex:
		events = MapCodexLine(line, d.codex)
	case PageSourcePermission:
		events = MapPermissionLine(d.runtime, line)
	case PageSourceJournal:
		events = MapJournalLine(d.runtime, line)
	}
	return d.occ.decorate(events)
}

func (d *sourceDecoder) clone() *sourceDecoder {
	out := &sourceDecoder{kind: d.kind, runtime: d.runtime, occ: &permissionOccurrenceTracker{
		next: cloneIntMap(d.occ.next), open: cloneStringSliceMap(d.occ.open),
	}}
	if d.claude != nil {
		out.claude = &ClaudeState{tools: cloneToolInfoMap(d.claude.tools)}
	}
	if d.codex != nil {
		out.codex = &CodexState{
			calls: cloneToolInfoMap(d.codex.calls), approvals: append([]string(nil), d.codex.approvals...),
		}
	}
	return out
}
