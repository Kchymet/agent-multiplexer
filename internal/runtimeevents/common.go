package runtimeevents

import (
	"encoding/json"
	"strings"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// common.go holds the pieces every runtime reader shares: the event constructors
// for the generic vocabulary and the small JSON helpers. Each reader supplies its
// own record→vocabulary mapping (claude.go, codex.go) on top of these.

// notice builds a meta-direction notice event.
func notice(level, text string) harnessproto.RuntimeEvent {
	return harnessproto.RuntimeEvent{Type: TypeNotice, Direction: dirMeta,
		Payload: mustMarshal(map[string]any{"level": level, "text": text})}
}

// rawEventFor builds the passthrough event for a record a reader has no mapping
// for: the producing runtime, the record's own type name, and its body verbatim.
// body must be valid JSON to embed as-is; an unparsable line is preserved as a
// JSON string so the raw event never loses the original bytes.
func rawEventFor(runtime, nativeType string, body json.RawMessage) harnessproto.RuntimeEvent {
	var bodyVal any
	switch {
	case len(body) == 0:
		bodyVal = json.RawMessage(`{}`)
	case json.Valid(body):
		bodyVal = body
	default:
		bodyVal = string(body)
	}
	return harnessproto.RuntimeEvent{Type: TypeRaw, Direction: dirOut,
		Payload: mustMarshal(map[string]any{
			"runtime":     runtime,
			"native_type": nativeType,
			"body":        bodyVal,
		})}
}

// extractText flattens a content value that may be a bare string or an array of
// {type,text} blocks into one string. Anything else yields "".
func extractText(v json.RawMessage) string {
	if len(v) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) == nil {
		return s
	}
	var arr []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(v, &arr) == nil {
		parts := make([]string, 0, len(arr))
		for _, b := range arr {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// rawOrNull passes JSON through for a raw_input/raw_output payload field,
// collapsing absent and null to nil.
func rawOrNull(v json.RawMessage) any {
	if len(v) == 0 || string(v) == "null" {
		return nil
	}
	return v
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// permissionEventBinder stamps only occurrence-specific bindings proved by the
// daemon. It retains a bound occurrence until its matching resolution arrives,
// so that resolution carries the originally bound generation even though the
// request is no longer open at publication time. Unbound history remains
// readable but generation-free and therefore nonanswerable.
type permissionEventBinder struct {
	active map[string]string // occurrence ItemID -> runtime generation
}

func newPermissionEventBinder() *permissionEventBinder {
	return &permissionEventBinder{active: make(map[string]string)}
}

func (b *permissionEventBinder) seed(bindings map[string]string) {
	// Seed occurrences skipped by afterSeq that remain authoritatively open, so
	// a later resolution still carries the same exact tuple.
	for occurrence, generation := range bindings {
		if occurrence != "" && generation != "" {
			b.active[occurrence] = generation
		}
	}
}

func (b *permissionEventBinder) bind(events []harnessproto.RuntimeEvent, bindings map[string]string) []harnessproto.RuntimeEvent {
	b.seed(bindings)
	for i := range events {
		event := &events[i]
		if event.Type != harnessproto.TypePermissionRequest && event.Type != harnessproto.TypePermissionResolved {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		// A structured record may itself contain this field. It is history, not
		// authority: discard it and use only the daemon's live occurrence binding.
		delete(payload, harnessproto.FieldRuntimeGeneration)
		generation := ""
		switch event.Type {
		case harnessproto.TypePermissionRequest:
			generation = bindings[event.ItemID]
			if generation != "" {
				b.active[event.ItemID] = generation
			}
		case harnessproto.TypePermissionResolved:
			generation = b.active[event.ItemID]
			delete(b.active, event.ItemID)
		}
		if generation != "" {
			payload[harnessproto.FieldRuntimeGeneration] = generation
		}
		event.Payload = mustMarshal(payload)
	}
	return events
}
