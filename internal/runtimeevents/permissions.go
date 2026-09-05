package runtimeevents

import (
	"encoding/json"
	"strings"

	"amux/internal/core"
	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// permissions.go reads amux's own permission journal — the third record a
// session's event stream is derived from, alongside the runtime's transcript.
//
// It exists because Claude Code's transcript records no permission prompts: the
// prompt opens in the TUI, the human answers, and nothing reaches disk. amux's
// hooks write what the transcript cannot (core/permissions.go), and this mapper
// turns those lines into the same vocabulary the transcript readers emit, so a
// consumer sees `permission_request` / `permission_resolved` on one stream with
// one ordinal space rather than having to correlate two.
//
// The journal is amux's own format, so unlike the runtime transcripts there is
// no upstream shape to drift — but the never-drop rule still holds: a line this
// mapper cannot read becomes `raw`.

// permissionMapper builds the journal's line mapper. runtime labels the `raw`
// passthrough with the runtime whose session the journal belongs to, so a
// consumer never has to guess which agent a stray line came from.
func permissionMapper(runtime string) func() LineMapper {
	return func() LineMapper {
		return func(line []byte) []harnessproto.RuntimeEvent { return MapPermissionLine(runtime, line) }
	}
}

// MapPermissionLine decodes one line of a session's permission journal into the
// event it stands for: a line that opens a request is a `permission_request`
// carrying the id a `permission` verb quotes back, and one that closes it is a
// `permission_resolved` retiring that id. Both coalesce on the request id.
func MapPermissionLine(runtime string, line []byte) []harnessproto.RuntimeEvent {
	if strings.TrimSpace(string(line)) == "" {
		return nil
	}
	var r core.PermissionRecord
	if err := json.Unmarshal(line, &r); err != nil || r.RequestID == "" {
		return []harnessproto.RuntimeEvent{rawEventFor(runtime, "permission/unparsable", json.RawMessage(line))}
	}
	if !r.Open() {
		return []harnessproto.RuntimeEvent{permissionResolved(r.RequestID, r.Decision)}
	}
	return []harnessproto.RuntimeEvent{{
		Type: TypePermissionRequest, ItemID: r.RequestID, Direction: dirOut,
		Payload: mustMarshal(map[string]any{
			"request_id": r.RequestID,
			"tool":       r.Tool,
			"action":     r.Action,
			"options":    permissionOptions(r.Options),
		}),
	}}
}

// permissionResolved retires a request id: the prompt it named is gone, so a
// `permission` verb quoting it must be refused rather than applied to whatever
// the runtime has open now. Shared by every reader that can observe a prompt
// closing (the journal here, Codex's rollout).
func permissionResolved(requestID, decision string) harnessproto.RuntimeEvent {
	return harnessproto.RuntimeEvent{
		Type: TypePermissionResolved, ItemID: requestID, Direction: dirOut,
		Payload: mustMarshal(map[string]any{
			"request_id": requestID,
			"decision":   decision,
		}),
	}
}

// permissionOptions keeps the options list a non-null array even for a record
// that carried none, so a consumer rendering an approve card never has to guard
// a nil.
func permissionOptions(opts []string) []string {
	if opts == nil {
		return []string{}
	}
	return opts
}

// Pending is the permission request a session currently has open: what the
// runtime is blocked on, and the id that answers it.
type Pending struct {
	RequestID string
	Tool      string
	Action    string
	Options   []string
}

// PendingPermission replays a session's record and returns the permission
// request still open — the one a `permission` verb may answer — or ok=false when
// nothing is waiting. When several are open (parallel tool calls), the most
// recent is returned; a caller matching a specific id should use OpenPermissions.
//
// It replays through the very same mappers the live stream uses, so the daemon
// can only ever match an id the stream actually published: correlation cannot
// drift from what the orchestrator was told.
func PendingPermission(rec Record) (Pending, bool) {
	open := OpenPermissions(rec)
	if len(open) == 0 {
		return Pending{}, false
	}
	return open[len(open)-1], true
}

// OpenPermissions returns every permission request a session still has open, in
// the order they were opened. Only the sources that can carry a permission event
// are replayed — Claude's transcript records no prompt at all, so scanning it
// would be pure cost, leaving only its short journal. Codex has no journal and is
// replayed from the rollout; that is the expensive case, and it is paid at most
// once per `permission` verb, which is human-paced.
func OpenPermissions(rec Record) []Pending {
	specs, ok := sourcesFor(rec)
	if !ok {
		return nil
	}
	var open []Pending
	for _, sp := range specs {
		if !sp.permission || sp.path == "" {
			continue
		}
		mapper := sp.newMapper()
		for _, ev := range mapEachLine(sp.path, mapper) {
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
					RequestID: body.RequestID, Tool: body.Tool,
					Action: body.Action, Options: body.Options,
				})
			case TypePermissionResolved:
				var body struct {
					RequestID string `json:"request_id"`
				}
				if json.Unmarshal(ev.Payload, &body) != nil {
					continue
				}
				for i, o := range open {
					if o.RequestID == body.RequestID {
						open = append(open[:i], open[i+1:]...)
						break
					}
				}
			}
		}
	}
	return open
}
