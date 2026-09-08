package daemon

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/claudecfg"
	"amux/internal/core"
	"amux/internal/runtimeevents"
	"amux/internal/sessionreport"
	"amux/internal/sessionrpc"
	"amux/internal/store"
	"amux/internal/wsops"
)

func reportRequest(principal access.Principal, verb string, fields map[string]string) sessionrpc.DispatchRequest {
	return sessionrpc.DispatchRequest{
		Principal: principal, RequestID: "0123456789abcdef0123456789abcdef",
		Call: sessionrpc.Call{Kind: sessionrpc.CallOperation, Route: access.RouteAction, Verb: verb, Fields: fields},
	}
}

func TestSessionReportVocabularyRejectsAuthorityAndPathFields(t *testing.T) {
	options, _ := json.Marshal([]string{core.PermissionAllow, core.PermissionDeny})
	validPermission := map[string]string{
		sessionreport.FieldRuntimeGeneration: "generation",
		sessionreport.FieldRequestID:         "request",
		sessionreport.FieldTool:              "Bash",
		sessionreport.FieldOptions:           string(options),
	}
	tests := []struct {
		name string
		req  access.Request
		ok   bool
	}{
		{"valid activity", access.Request{Route: access.RouteAction, Verb: sessionreport.Activity, Fields: map[string]string{
			sessionreport.FieldRuntimeGeneration: "generation", sessionreport.FieldState: core.StateReady,
		}}, true},
		{"caller id", access.Request{Route: access.RouteAction, Verb: sessionreport.Activity, ID: "other", Fields: map[string]string{
			sessionreport.FieldRuntimeGeneration: "generation", sessionreport.FieldState: core.StateReady,
		}}, false},
		{"missing generation", access.Request{Route: access.RouteAction, Verb: sessionreport.Activity, Fields: map[string]string{
			sessionreport.FieldState: core.StateReady,
		}}, false},
		{"path smuggling", access.Request{Route: access.RouteAction, Verb: sessionreport.Capture, Fields: map[string]string{
			sessionreport.FieldRuntimeGeneration: "generation", sessionreport.FieldEvent: "Stop", "path": "/host/foreign",
		}}, false},
		{"occurrence smuggling", access.Request{Route: access.RouteAction, Verb: sessionreport.PermissionRequest, Fields: func() map[string]string {
			fields := make(map[string]string, len(validPermission)+1)
			for key, value := range validPermission {
				fields[key] = value
			}
			fields["item_id"] = "caller-occurrence"
			return fields
		}()}, false},
		{"oversized tool", access.Request{Route: access.RouteAction, Verb: sessionreport.PermissionRequest, Fields: func() map[string]string {
			fields := make(map[string]string, len(validPermission))
			for key, value := range validPermission {
				fields[key] = value
			}
			fields[sessionreport.FieldTool] = string(make([]byte, sessionreport.MaxToolBytes+1))
			return fields
		}()}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSessionReport(tc.req)
			if tc.ok && err != nil {
				t.Fatalf("valid report rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("invalid report accepted")
			}
		})
	}
}

func publishTestRuntime(t *testing.T, d *Daemon, subject string) string {
	t.Helper()
	generation, err := d.permissions.observe(subject, &fakeInstance{})
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

func TestSessionReportsScopeManagedRecordsAndKeepPermissionsDiagnostic(t *testing.T) {
	const runtimeID = "33333333-3333-4333-8333-333333333333"
	a := store.Session{ID: "a1", RootID: "root", Agent: "claude", Dir: t.TempDir(), ClaudeID: runtimeID}
	b := store.Session{ID: "a2", RootID: "root", Agent: "claude", Dir: t.TempDir(), ClaudeID: runtimeID}
	d, runtime, principals := sessionRuntimeFixture(t, a, b)
	genA := publishTestRuntime(t, d, a.ID)
	publishTestRuntime(t, d, b.ID)

	activity := reportRequest(principals[a.ID], sessionreport.Activity, map[string]string{
		sessionreport.FieldRuntimeGeneration: genA,
		sessionreport.FieldState:             core.StateRunning,
	})
	result, err := runtime.dispatch(context.Background(), activity)
	if err != nil || result.Status != sessionrpc.StatusOK {
		t.Fatalf("activity report = %+v, %v", result, err)
	}
	if rec, ok := core.SessionHookState(a.ID, runtimeID); !ok || rec.State != core.StateRunning || rec.Cwd != a.Dir {
		t.Fatalf("subject activity = %+v, %v", rec, ok)
	}
	if _, ok := core.SessionHookState(b.ID, runtimeID); ok {
		t.Fatal("equal runtime UUID selected another subject's activity")
	}

	options, _ := json.Marshal([]string{core.PermissionAllow, core.PermissionDeny})
	permission := reportRequest(principals[a.ID], sessionreport.PermissionRequest, map[string]string{
		sessionreport.FieldRuntimeGeneration: genA,
		sessionreport.FieldRequestID:         "self-claimed-id",
		sessionreport.FieldTool:              "Bash",
		sessionreport.FieldAction:            "echo diagnostic",
		sessionreport.FieldOptions:           string(options),
	})
	result, err = runtime.dispatch(context.Background(), permission)
	if err != nil || result.Status != sessionrpc.StatusOK {
		t.Fatalf("permission observation = %+v, %v", result, err)
	}
	open, err := core.PendingPermissionObservations(a.ID, runtimeID, genA)
	if err != nil || len(open) != 1 || open[0].RequestID != "self-claimed-id" {
		t.Fatalf("diagnostic observations = %+v, %v", open, err)
	}
	mismatchedResolution := reportRequest(principals[a.ID], sessionreport.PermissionResolved, map[string]string{
		sessionreport.FieldRuntimeGeneration: genA,
		sessionreport.FieldRequestID:         "self-claimed-id",
		sessionreport.FieldTool:              "Read",
		sessionreport.FieldDecision:          core.PermissionAllow,
	})
	result, err = runtime.dispatch(context.Background(), mismatchedResolution)
	if err != nil || result.Status != sessionrpc.StatusFailed || result.Code != "report_permission_lifecycle" {
		t.Fatalf("mismatched permission lifecycle = %+v, %v", result, err)
	}
	open, err = core.PendingPermissionObservations(a.ID, runtimeID, genA)
	if err != nil || len(open) != 1 {
		t.Fatalf("invalid resolution closed observation: %+v, %v", open, err)
	}
	db, err := store.Open()
	if err != nil {
		t.Fatal(err)
	}
	record, err := d.runtimeRecord(db, a.ID)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if record.Permissions != "" || len(runtimeevents.OpenPermissions(runtimeEventRecord(record))) != 0 {
		t.Fatalf("self observation became answerable: %+v", record)
	}
}

func TestOldGenerationReportCannotClearOrMutateReplacement(t *testing.T) {
	const runtimeID = "44444444-4444-4444-8444-444444444444"
	session := store.Session{ID: "a1", RootID: "root", Agent: "claude", Dir: t.TempDir(), ClaudeID: runtimeID}
	d, runtime, principals := sessionRuntimeFixture(t, session)
	oldGeneration := publishTestRuntime(t, d, session.ID)
	if err := core.AppendPermissionObservation(session.ID, runtimeID, oldGeneration, core.PermissionObservation{RequestID: "old", Tool: "Bash"}); err != nil {
		t.Fatal(err)
	}
	newGeneration, err := d.permissions.observe(session.ID, &fakeInstance{})
	if err != nil || newGeneration == oldGeneration {
		t.Fatalf("replacement generation = %q, %v", newGeneration, err)
	}
	clear := reportRequest(principals[session.ID], sessionreport.PermissionClear, map[string]string{
		sessionreport.FieldRuntimeGeneration: oldGeneration,
		sessionreport.FieldDecision:          core.PermissionCleared,
	})
	result, err := runtime.dispatch(context.Background(), clear)
	if err != nil || result.Status != sessionrpc.StatusDenied || result.Code != "report_runtime_replaced" {
		t.Fatalf("old clear = %+v, %v", result, err)
	}
	open, err := core.PendingPermissionObservations(session.ID, runtimeID, oldGeneration)
	if err != nil || len(open) != 1 {
		t.Fatalf("old observation was mutated: %+v, %v", open, err)
	}
	if _, ok := core.SessionHookState(session.ID, runtimeID); ok {
		t.Fatal("replacement inherited an old report effect")
	}
}

func TestSessionCaptureUsesAuthoritativePrivateTranscript(t *testing.T) {
	const runtimeID = "66666666-6666-4666-8666-666666666666"
	session := store.Session{ID: "a1", RootID: "root", Agent: "claude", Dir: t.TempDir(), ClaudeID: runtimeID}
	home := claudecfg.At(claudecfg.AgentHome(session.Dir))
	source := home.TranscriptPath(session.Dir, runtimeID)
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	const content = "{\"role\":\"assistant\",\"content\":\"owned\"}\n"
	if err := os.WriteFile(source, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	d, runtime, principals := sessionRuntimeFixture(t, session)
	generation := publishTestRuntime(t, d, session.ID)
	result, err := runtime.dispatch(context.Background(), reportRequest(principals[session.ID], sessionreport.Capture, map[string]string{
		sessionreport.FieldRuntimeGeneration: generation,
		sessionreport.FieldEvent:             "Stop",
	}))
	if err != nil || result.Status != sessionrpc.StatusOK {
		t.Fatalf("capture = %+v, %v", result, err)
	}
	path, _, ok := core.SessionCapturedTranscript(session.ID, runtimeID)
	if !ok {
		t.Fatal("daemon-owned capture was not published")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != content {
		t.Fatalf("captured transcript = %q, %v", got, err)
	}
}

func TestReviewerCaptureFindsHistoricalAcceptedLaunchCWD(t *testing.T) {
	const runtimeID = "77777777-7777-4777-8777-777777777777"
	session := store.Session{
		ID: "a1", RootID: "root", Agent: "claude", Repo: "acme/api",
		Dir: t.TempDir(), ClaudeID: runtimeID,
	}
	historicalCwd := wsops.AgentWorkdir(session)
	if historicalCwd == session.Dir {
		t.Fatal("single-repo fixture did not produce its historical launch cwd")
	}
	home := claudecfg.At(claudecfg.AgentHome(session.Dir))
	source := home.TranscriptPath(historicalCwd, runtimeID)
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	const content = "{\"role\":\"assistant\",\"content\":\"historical cwd\"}\n"
	if err := os.WriteFile(source, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := openSessionTranscriptSource(session)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil || string(got) != content {
		t.Fatalf("historical accepted launch cwd capture = %q, %v", got, err)
	}
}

func TestCaptureStreamsOutsideEffectGateAndRejectsReplacement(t *testing.T) {
	const runtimeID = "55555555-5555-4555-8555-555555555555"
	session := store.Session{ID: "a1", RootID: "root", Agent: "claude", Dir: t.TempDir(), ClaudeID: runtimeID}
	home := claudecfg.At(claudecfg.AgentHome(session.Dir))
	source := home.TranscriptPath(session.Dir, runtimeID)
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("{\"role\":\"user\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, runtime, principals := sessionRuntimeFixture(t, session)
	oldGeneration := publishTestRuntime(t, d, session.ID)

	originalStage := stageSessionTranscript
	started := make(chan struct{})
	release := make(chan struct{})
	stageSessionTranscript = func(ctx context.Context, subject, runtimeID, event string, src io.Reader) (*core.SessionTranscriptStage, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return core.StageSessionTranscript(ctx, subject, runtimeID, event, src)
	}
	t.Cleanup(func() { stageSessionTranscript = originalStage })

	request := reportRequest(principals[session.ID], sessionreport.Capture, map[string]string{
		sessionreport.FieldRuntimeGeneration: oldGeneration,
		sessionreport.FieldEvent:             "Stop",
	})
	done := make(chan sessionrpc.DispatchResult, 1)
	go func() {
		result, _ := runtime.dispatch(context.Background(), request)
		done <- result
	}()
	<-started
	gateAvailable := make(chan struct{})
	go func() {
		d.effectMu.Lock()
		close(gateAvailable)
		d.effectMu.Unlock()
	}()
	select {
	case <-gateAvailable:
	case <-time.After(time.Second):
		t.Fatal("capture copy held daemon-wide effectMu")
	}
	newGeneration, err := d.permissions.observe(session.ID, &fakeInstance{})
	if err != nil || newGeneration == oldGeneration {
		t.Fatalf("replace during capture = %q, %v", newGeneration, err)
	}
	close(release)
	result := <-done
	if result.Status != sessionrpc.StatusDenied || result.Code != "report_runtime_replaced" {
		t.Fatalf("capture after replacement = %+v", result)
	}
	if _, _, ok := core.SessionCapturedTranscript(session.ID, runtimeID); ok {
		t.Fatal("replacement published an old runtime's staged capture")
	}
}
