package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"amux/internal/access"
	"amux/internal/agent"
	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/hostprep"
	"amux/internal/sessionreport"
	"amux/internal/sessionrpc"
	"amux/internal/store"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

func isSessionReport(req access.Request) bool {
	return (req.Route == access.RouteQuery && sessionreport.IsContextQuery(req.Verb)) ||
		(req.Route == access.RouteAction && sessionreport.IsVerb(req.Verb))
}

func (r *sessionRuntime) authorizeSessionReport(ctx context.Context, principal access.Principal, req access.Request) error {
	if principal.Kind != access.SubjectSession {
		return access.ErrDenied
	}
	if err := validateSessionReport(req); err != nil {
		return err
	}
	resource, ok, err := r.resolver.Lookup(ctx, principal.SubjectID)
	if err != nil {
		return err
	}
	if !ok || resource.Archived || resource.ID != principal.SubjectID {
		return access.ErrDenied
	}
	return nil
}

func validateSessionReport(req access.Request) error {
	if req.ID != "" || req.Target != "" || req.Tab != 0 {
		return errors.New("self report does not accept id, target, or tab")
	}
	if req.Route == access.RouteQuery {
		if !sessionreport.IsContextQuery(req.Verb) || len(req.Fields) != 0 {
			return errors.New("invalid self report query")
		}
		return nil
	}
	if req.Route != access.RouteAction || !sessionreport.IsVerb(req.Verb) {
		return errors.New("unknown self report operation")
	}
	required := []string{sessionreport.FieldRuntimeGeneration}
	allowed := append([]string(nil), required...)
	switch req.Verb {
	case sessionreport.Activity:
		allowed = append(allowed, sessionreport.FieldState)
		required = append(required, sessionreport.FieldState)
	case sessionreport.Model:
		allowed = append(allowed, sessionreport.FieldModel)
		required = append(required, sessionreport.FieldModel)
	case sessionreport.PermissionRequest:
		allowed = append(allowed, sessionreport.FieldRequestID, sessionreport.FieldTool,
			sessionreport.FieldAction, sessionreport.FieldOptions)
		required = append(required, sessionreport.FieldRequestID, sessionreport.FieldTool,
			sessionreport.FieldOptions)
	case sessionreport.PermissionResolved:
		allowed = append(allowed, sessionreport.FieldRequestID, sessionreport.FieldTool, sessionreport.FieldDecision)
		required = append(required, sessionreport.FieldDecision)
	case sessionreport.PermissionClear:
		allowed = append(allowed, sessionreport.FieldRequestID, sessionreport.FieldDecision)
		required = append(required, sessionreport.FieldDecision)
	case sessionreport.Capture:
		allowed = append(allowed, sessionreport.FieldEvent)
		required = append(required, sessionreport.FieldEvent)
	}
	if err := exactReportFields(req.Fields, allowed, required); err != nil {
		return err
	}
	if err := boundedReportField(req.Fields, sessionreport.FieldRuntimeGeneration, 256, true); err != nil {
		return err
	}
	switch req.Verb {
	case sessionreport.Activity:
		if err := boundedReportField(req.Fields, sessionreport.FieldState, sessionreport.MaxStateBytes, true); err != nil {
			return err
		}
		switch req.Fields[sessionreport.FieldState] {
		case core.StateIdle, core.StateReady, core.StateWaiting, core.StateRunning:
			return nil
		default:
			return errors.New("invalid activity state")
		}
	case sessionreport.Model:
		return boundedReportField(req.Fields, sessionreport.FieldModel, sessionreport.MaxModelBytes, true)
	case sessionreport.PermissionRequest:
		if err := boundedPermissionFields(req.Fields); err != nil {
			return err
		}
		if err := boundedReportField(req.Fields, sessionreport.FieldRequestID, sessionreport.MaxRequestIDBytes, true); err != nil {
			return err
		}
		if err := boundedReportField(req.Fields, sessionreport.FieldTool, sessionreport.MaxToolBytes, true); err != nil {
			return err
		}
		options, err := decodeReportOptions(req.Fields[sessionreport.FieldOptions])
		if err != nil {
			return err
		}
		if len(options) != 2 || options[0] != core.PermissionAllow || options[1] != core.PermissionDeny {
			return errors.New("permission report options must be allow and deny")
		}
		return nil
	case sessionreport.PermissionResolved:
		if err := boundedPermissionFields(req.Fields); err != nil {
			return err
		}
		if strings.TrimSpace(req.Fields[sessionreport.FieldRequestID]) == "" && strings.TrimSpace(req.Fields[sessionreport.FieldTool]) == "" {
			return errors.New("permission resolution requires request_id or tool")
		}
		decision := req.Fields[sessionreport.FieldDecision]
		if decision != core.PermissionAllow && decision != core.PermissionDeny {
			return errors.New("permission resolution decision must be allow or deny")
		}
		return nil
	case sessionreport.PermissionClear:
		if err := boundedPermissionFields(req.Fields); err != nil {
			return err
		}
		if req.Fields[sessionreport.FieldDecision] != core.PermissionCleared {
			return errors.New("permission clear decision must be cleared")
		}
		return nil
	case sessionreport.Capture:
		if err := boundedReportField(req.Fields, sessionreport.FieldEvent, sessionreport.MaxEventBytes, true); err != nil {
			return err
		}
		if !claudecfg.CaptureHookEvent(req.Fields[sessionreport.FieldEvent]) {
			return errors.New("unknown capture hook event")
		}
		return nil
	}
	return errors.New("unknown self report operation")
}

func exactReportFields(fields map[string]string, allowed, required []string) error {
	want := make(map[string]bool, len(allowed))
	for _, field := range allowed {
		want[field] = true
	}
	for field := range fields {
		if !want[field] {
			return fmt.Errorf("unknown self report field %q", field)
		}
	}
	for _, field := range required {
		if _, ok := fields[field]; !ok {
			return fmt.Errorf("missing self report field %q", field)
		}
	}
	return nil
}

func boundedReportField(fields map[string]string, name string, limit int, required bool) error {
	value, ok := fields[name]
	if !ok {
		return nil
	}
	if !utf8.ValidString(value) || len(value) > limit {
		return fmt.Errorf("self report field %q exceeds its bound", name)
	}
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("self report field %q is required", name)
	}
	return nil
}

func boundedPermissionFields(fields map[string]string) error {
	for _, field := range []struct {
		name  string
		limit int
	}{
		{sessionreport.FieldRequestID, sessionreport.MaxRequestIDBytes},
		{sessionreport.FieldTool, sessionreport.MaxToolBytes},
		{sessionreport.FieldAction, sessionreport.MaxActionBytes},
		{sessionreport.FieldOptions, sessionreport.MaxOptionsBytes},
		{sessionreport.FieldDecision, sessionreport.MaxOptionBytes},
	} {
		if err := boundedReportField(fields, field.name, field.limit, false); err != nil {
			return err
		}
	}
	return nil
}

func decodeReportOptions(value string) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(value)))
	var options []string
	if err := dec.Decode(&options); err != nil {
		return nil, errors.New("invalid permission report options")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("invalid permission report options")
	}
	if len(options) > 8 {
		return nil, errors.New("too many permission report options")
	}
	for _, option := range options {
		if !utf8.ValidString(option) || len(option) > sessionreport.MaxOptionBytes {
			return nil, errors.New("permission report option exceeds its bound")
		}
	}
	return options, nil
}

type selfReportContext struct {
	RuntimeGeneration string `json:"runtime_generation"`
}

func (r *sessionRuntime) dispatchSessionReport(ctx context.Context, principal access.Principal, req access.Request) sessionrpc.DispatchResult {
	if err := r.authorizeSessionReport(ctx, principal, req); err != nil {
		if errors.Is(err, access.ErrDenied) {
			return rpcDenied("report_access_denied")
		}
		return rpcInvalid("report_invalid")
	}
	session, ok, err := lookupSession(principal.SubjectID)
	if err != nil || !ok || session.Archived || session.ID != principal.SubjectID {
		return rpcDenied("report_session_unavailable")
	}
	if strings.TrimSpace(session.ClaudeID) == "" {
		return rpcFailed("report_runtime_identity_unavailable")
	}
	generation, live := r.d.permissions.generation(session.ID)
	if !live || generation == "" {
		return rpcFailed("report_runtime_unavailable")
	}
	if req.Route == access.RouteQuery {
		body, err := json.Marshal(selfReportContext{RuntimeGeneration: generation})
		if err != nil {
			return rpcFailed("report_encode_failed")
		}
		return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body}
	}
	if req.Fields[sessionreport.FieldRuntimeGeneration] != generation {
		return rpcDenied("report_runtime_replaced")
	}
	if err := r.applySessionReport(session, generation, req); err != nil {
		return rpcFailed(reportErrorCode(err))
	}
	body, err := r.encodeResult(core.Result{Type: "result", OK: true})
	if err != nil {
		return rpcFailed("report_encode_failed")
	}
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body}
}

func (r *sessionRuntime) applySessionReport(session store.Session, generation string, req access.Request) error {
	switch req.Verb {
	case sessionreport.Activity:
		return core.WriteSessionHookState(session.ID, session.ClaudeID,
			req.Fields[sessionreport.FieldState], session.Dir)
	case sessionreport.Model:
		return core.WriteSessionRuntimeModel(session.ID, session.ClaudeID,
			req.Fields[sessionreport.FieldModel])
	case sessionreport.PermissionRequest:
		return appendPermissionRequestObservation(session, generation, req.Fields)
	case sessionreport.PermissionResolved:
		return appendPermissionResolutionObservation(session, generation, req.Fields, false)
	case sessionreport.PermissionClear:
		return appendPermissionResolutionObservation(session, generation, req.Fields, true)
	case sessionreport.Capture:
		return errors.New("capture requires two-phase dispatch")
	default:
		return errors.New("invalid report operation")
	}
}

var (
	errReportLifecycle     = errors.New("permission report lifecycle invalid")
	errReportCapture       = errors.New("capture source unavailable")
	stageSessionTranscript = core.StageSessionTranscript
	// One unpublished capture globally bounds staging disk to 256 MiB without
	// blocking management dispatch/effect admission while the copy runs.
	sessionCaptureSlot = make(chan struct{}, 1)
)

func reportErrorCode(err error) string {
	switch {
	case errors.Is(err, errReportLifecycle):
		return "report_permission_lifecycle"
	case errors.Is(err, errReportCapture), errors.Is(err, core.ErrSessionTranscriptTooLarge),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), hostprep.IsUnsafe(err):
		return "report_capture_unavailable"
	default:
		return "report_write_failed"
	}
}

func appendPermissionRequestObservation(session store.Session, generation string, fields map[string]string) error {
	open, err := core.PendingPermissionObservations(session.ID, session.ClaudeID, generation)
	if err != nil {
		return err
	}
	requestID := fields[sessionreport.FieldRequestID]
	for _, current := range open {
		if current.RequestID == requestID {
			return fmt.Errorf("%w: duplicate open request", errReportLifecycle)
		}
	}
	options, err := decodeReportOptions(fields[sessionreport.FieldOptions])
	if err != nil {
		return err
	}
	return core.AppendPermissionObservation(session.ID, session.ClaudeID, generation, core.PermissionObservation{
		RequestID: requestID, Tool: fields[sessionreport.FieldTool],
		Action: fields[sessionreport.FieldAction], Options: options,
	})
}

func appendPermissionResolutionObservation(session store.Session, generation string, fields map[string]string, clear bool) error {
	open, err := core.PendingPermissionObservations(session.ID, session.ClaudeID, generation)
	if err != nil {
		return err
	}
	requestID := fields[sessionreport.FieldRequestID]
	if clear && requestID == "" {
		if len(open) == 0 {
			return fmt.Errorf("%w: no open request", errReportLifecycle)
		}
		for _, current := range open {
			if err := core.AppendPermissionObservation(session.ID, session.ClaudeID, generation, core.PermissionObservation{
				RequestID: current.RequestID, Tool: current.Tool, Decision: core.PermissionCleared,
			}); err != nil {
				return err
			}
		}
		return nil
	}
	var matches []core.PermissionObservation
	foundRequestID := false
	for _, current := range open {
		if requestID != "" && current.RequestID == requestID {
			foundRequestID = true
		}
		switch {
		case requestID != "" && current.RequestID == requestID &&
			(fields[sessionreport.FieldTool] == "" || fields[sessionreport.FieldTool] == current.Tool):
			matches = append(matches, current)
		case requestID == "" && fields[sessionreport.FieldTool] != "" && current.Tool == fields[sessionreport.FieldTool]:
			matches = append(matches, current)
		}
	}
	if len(matches) == 0 {
		if foundRequestID {
			return fmt.Errorf("%w: request tool mismatch", errReportLifecycle)
		}
		return fmt.Errorf("%w: no matching open request", errReportLifecycle)
	}
	if len(matches) != 1 {
		return fmt.Errorf("%w: ambiguous open request", errReportLifecycle)
	}
	decision := fields[sessionreport.FieldDecision]
	return core.AppendPermissionObservation(session.ID, session.ClaudeID, generation, core.PermissionObservation{
		RequestID: matches[0].RequestID, Tool: matches[0].Tool, Decision: decision,
	})
}

func openSessionTranscriptSource(session store.Session) (*os.File, error) {
	if agent.Canonical(session.Agent) != harnessproto.RuntimeClaude {
		return nil, fmt.Errorf("%w: runtime does not expose Claude capture hooks", errReportCapture)
	}
	root, err := hostprep.OpenSession(session.Dir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errReportCapture, err)
	}
	defer root.Close()
	home := claudecfg.At(claudecfg.AgentHome(session.Dir))
	cwd, found, err := home.FindSessionRooted(root, session.ClaudeID, session.Dir)
	if err != nil || !found {
		return nil, fmt.Errorf("%w: managed transcript not found", errReportCapture)
	}
	source := home.TranscriptPath(cwd, session.ClaudeID)
	rel, err := root.Rel(source)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errReportCapture, err)
	}
	f, err := root.OpenFile(rel)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errReportCapture, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: source stat failed", errReportCapture)
	}
	if err := core.ValidateSessionTranscriptSize(info.Size()); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// dispatchSessionCapture keeps the daemon-wide dispatch/effect locks away from
// the potentially 256 MiB copy. Both short gates authenticate and bind the exact
// subject/runtime incarnation; only the final gate publishes the private stage.
func (r *sessionRuntime) dispatchSessionCapture(ctx context.Context, principal access.Principal, req access.Request) sessionrpc.DispatchResult {
	select {
	case sessionCaptureSlot <- struct{}{}:
		defer func() { <-sessionCaptureSlot }()
	case <-ctx.Done():
		return rpcFailed("report_capture_unavailable")
	}
	r.dispatchMu.Lock()
	r.d.effectMu.Lock()
	ctx = withEffectAdmission(ctx)
	if err := r.d.authority.Valid(ctx, principal); err != nil {
		r.d.effectMu.Unlock()
		r.dispatchMu.Unlock()
		return rpcDenied("credential_invalid")
	}
	if err := r.authorizeSessionReport(ctx, principal, req); err != nil {
		r.d.effectMu.Unlock()
		r.dispatchMu.Unlock()
		if errors.Is(err, access.ErrDenied) {
			return rpcDenied("report_access_denied")
		}
		return rpcInvalid("report_invalid")
	}
	session, ok, err := lookupSession(principal.SubjectID)
	generation, live := r.d.permissions.generation(principal.SubjectID)
	if err != nil || !ok || session.Archived || session.ID != principal.SubjectID ||
		session.ClaudeID == "" || !live || generation == "" {
		r.d.effectMu.Unlock()
		r.dispatchMu.Unlock()
		return rpcFailed("report_runtime_unavailable")
	}
	if req.Fields[sessionreport.FieldRuntimeGeneration] != generation {
		r.d.effectMu.Unlock()
		r.dispatchMu.Unlock()
		return rpcDenied("report_runtime_replaced")
	}
	source, err := openSessionTranscriptSource(session)
	r.d.effectMu.Unlock()
	r.dispatchMu.Unlock()
	if err != nil {
		return rpcFailed(reportErrorCode(err))
	}
	defer source.Close()

	stage, err := stageSessionTranscript(ctx, session.ID, session.ClaudeID,
		req.Fields[sessionreport.FieldEvent], source)
	if err != nil {
		return rpcFailed(reportErrorCode(err))
	}
	defer stage.Abort()

	r.dispatchMu.Lock()
	defer r.dispatchMu.Unlock()
	r.d.effectMu.Lock()
	defer r.d.effectMu.Unlock()
	if err := ctx.Err(); err != nil {
		return rpcFailed("report_capture_unavailable")
	}
	if err := r.d.authority.Valid(ctx, principal); err != nil {
		return rpcDenied("credential_invalid")
	}
	if err := r.authorizeSessionReport(ctx, principal, req); err != nil {
		if errors.Is(err, access.ErrDenied) {
			return rpcDenied("report_access_denied")
		}
		return rpcInvalid("report_invalid")
	}
	current, ok, err := lookupSession(principal.SubjectID)
	currentGeneration, live := r.d.permissions.generation(principal.SubjectID)
	if err != nil || !ok || current.Archived || current.ID != session.ID ||
		current.ClaudeID != session.ClaudeID || current.Dir != session.Dir ||
		!live || currentGeneration != generation ||
		req.Fields[sessionreport.FieldRuntimeGeneration] != currentGeneration {
		return rpcDenied("report_runtime_replaced")
	}
	if err := stage.Publish(); err != nil {
		return rpcFailed(reportErrorCode(err))
	}
	body, err := r.encodeResult(core.Result{Type: "result", OK: true})
	if err != nil {
		return rpcFailed("report_encode_failed")
	}
	return sessionrpc.DispatchResult{Status: sessionrpc.StatusOK, Body: body}
}
