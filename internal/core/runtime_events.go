package core

import "github.com/kchymet/agent-multiplexer/harnessproto"

const (
	RuntimeEventsCursorField        = "cursor"
	RuntimeEventsAfterSequenceField = "after_sequence"
)

// SequencedRuntimeEvent gives file-RPC pages an explicit stable ordinal. These
// ordinals belong to one page-cursor chain. They are not interchangeable with a
// provider stream's afterSeq: independently started readers can observe
// multi-source appends in a different order.
type SequencedRuntimeEvent struct {
	Sequence int64                     `json:"sequence"`
	Event    harnessproto.RuntimeEvent `json:"event"`
}

// OversizedRuntimeEvent explicitly accounts for content that cannot be carried
// by one bounded response. Kind "event" describes a normalized event whose JSON
// size was measured. Kind "raw_record" describes a source record discarded in
// bounded chunks without parsing; only its raw byte count and reason are known.
type OversizedRuntimeEvent struct {
	Kind         string `json:"kind"`
	Sequence     int64  `json:"sequence"`
	Type         string `json:"type,omitempty"`
	EncodedBytes int    `json:"encoded_bytes,omitempty"`
	RawBytes     int64  `json:"raw_bytes,omitempty"`
	Reason       string `json:"reason"`
}

// RuntimeEventPage is the complete QueryRuntimeEvents response body. Its JSON
// encoding, including this wrapper and NextCursor, is capped by the session RPC
// response limit.
type RuntimeEventPage struct {
	Target          string                  `json:"target"`
	Runtime         string                  `json:"runtime,omitempty"`
	CursorSequence  int64                   `json:"cursor_sequence"`
	ScannedSequence int64                   `json:"scanned_sequence"`
	Events          []SequencedRuntimeEvent `json:"events"`
	NextCursor      string                  `json:"next_cursor"`
	HasMore         bool                    `json:"has_more"`
	Truncated       bool                    `json:"truncated"`
	Oversized       *OversizedRuntimeEvent  `json:"oversized,omitempty"`
}
