package runtimeevents

import (
	"encoding/json"
	"strings"

	"amux/internal/core"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// journal.go reads amux's own session journal (core/journal.go) — the third
// record a session's event stream is derived from, alongside the runtime's
// transcript and the permission journal.
//
// It carries what amux did *to* a session, which the runtime by definition never
// records: today that is the progress of a cold start kicked off by a relayed
// `prompt` (docs/remote-provider-sessions.md §4.6). Those lines become `notice`
// events, so an orchestrator watching one stream sees "starting agent" and then
// the runtime's own events once the transcript exists, rather than silence
// followed by a turn appearing from nowhere.
//
// The journal is amux's own format, so there is no upstream shape to drift —
// but the never-drop rule still holds: a line this mapper cannot read becomes
// `raw`.

// journalMapper builds the journal's line mapper. runtime labels the `raw`
// passthrough with the runtime whose session the journal belongs to, matching
// permissionMapper.
func journalMapper(runtime string) func() LineMapper {
	return func() LineMapper {
		return func(line []byte) []harnessproto.RuntimeEvent { return MapJournalLine(runtime, line) }
	}
}

// MapJournalLine decodes one line of a session's amux journal into the `notice`
// it stands for. The record's level rides through as the notice's level, so an
// amux-side failure arrives as a notice at `error` rather than as prose a
// consumer would have to parse.
func MapJournalLine(runtime string, line []byte) []harnessproto.RuntimeEvent {
	if strings.TrimSpace(string(line)) == "" {
		return nil
	}
	var r core.JournalRecord
	if err := json.Unmarshal(line, &r); err != nil || r.Text == "" {
		return []harnessproto.RuntimeEvent{rawEventFor(runtime, "amux/journal-unparsable", json.RawMessage(line))}
	}
	level := r.Level
	if level == "" {
		level = core.JournalInfo
	}
	return []harnessproto.RuntimeEvent{notice(level, r.Text)}
}
