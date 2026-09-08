// Package sessionreport defines the closed self-report vocabulary carried over
// authenticated sessionrpc calls. It contains no authority or filesystem logic.
package sessionreport

const (
	Context            = "report-context"
	Activity           = "report-activity"
	Model              = "report-model"
	PermissionRequest  = "report-permission-request"
	PermissionResolved = "report-permission-resolved"
	PermissionClear    = "report-permission-clear"
	Capture            = "report-capture"
)

const (
	FieldState             = "state"
	FieldModel             = "model"
	FieldRequestID         = "request_id"
	FieldTool              = "tool"
	FieldAction            = "action"
	FieldOptions           = "options"
	FieldDecision          = "decision"
	FieldEvent             = "event"
	FieldRuntimeGeneration = "runtime_generation"
)

const (
	MaxStateBytes     = 16
	MaxModelBytes     = 256
	MaxRequestIDBytes = 256
	MaxToolBytes      = 256
	MaxActionBytes    = 8 << 10
	MaxOptionsBytes   = 1 << 10
	MaxOptionBytes    = 64
	MaxEventBytes     = 64
	MaxHookInputBytes = 64 << 10
)

// IsVerb reports whether verb belongs to the self-report namespace. Report
// verbs deliberately remain outside core.KnownAction: they are not management
// operations and must never reach the general wsops action dispatcher.
func IsVerb(verb string) bool {
	switch verb {
	case Activity, Model, PermissionRequest, PermissionResolved, PermissionClear, Capture:
		return true
	default:
		return false
	}
}

// IsContextQuery is the only self-report query. It returns correlation for the
// authenticated subject's current runtime, never identity supplied by a caller.
func IsContextQuery(verb string) bool { return verb == Context }
