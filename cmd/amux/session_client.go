package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"amux/internal/core"
	"amux/internal/sessionreport"
	"amux/internal/sessionrpc"
)

type restrictedSessionRPC interface {
	Action(context.Context, sessionrpc.Action) (sessionrpc.Result, error)
	Query(context.Context, sessionrpc.Query) (sessionrpc.Result, error)
	Close() error
}

var openRestrictedSessionRPC = func() (restrictedSessionRPC, error) {
	return sessionrpc.OpenClient()
}

func restrictedAction(action core.Action) (string, error) {
	if action.Kind != "" || action.Cwd != "" || action.Query != "" || action.PaneID != "" ||
		action.Tab != 0 || action.Cols != 0 || action.Rows != 0 || len(action.Data) != 0 {
		return "", fmt.Errorf("action contains fields unavailable to session RPC")
	}
	client, err := openRestrictedSessionRPC()
	if err != nil {
		return "", fmt.Errorf("open fixed session control context: %w", err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	result, callErr := client.Action(ctx, sessionrpc.Action{
		Verb: action.Action, ID: action.ID, Target: action.Target, Fields: cloneCLIFields(action.Fields),
	})
	var response core.Result
	if len(result.Body) != 0 {
		if err := json.Unmarshal(result.Body, &response); err != nil {
			return "", fmt.Errorf("decode session action response: %w", err)
		}
	}
	if callErr != nil {
		// Publication/response uncertainty is returned verbatim. The caller must
		// never turn it into an automatic retry of a claimed mutation.
		if response.Error != "" {
			return "", fmt.Errorf("%s: %w", response.Error, callErr)
		}
		return "", callErr
	}
	if len(result.Body) == 0 || !response.OK {
		if response.Error == "" {
			response.Error = "session action returned no successful result"
		}
		return "", fmt.Errorf("%s", response.Error)
	}
	return response.NewID, nil
}

func restrictedQuery(name string, dst any) error {
	return restrictedQueryRequest(sessionrpc.Query{Verb: name}, dst)
}

func restrictedQueryRequest(query sessionrpc.Query, dst any) error {
	client, err := openRestrictedSessionRPC()
	if err != nil {
		return fmt.Errorf("open fixed session control context: %w", err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	result, err := client.Query(ctx, query)
	if err != nil {
		return err
	}
	if len(result.Body) == 0 {
		return fmt.Errorf("session query returned no result")
	}
	return json.Unmarshal(result.Body, dst)
}

// restrictedReport publishes one self-only observation through the fixed
// authenticated session context, in one call. The daemon's final effect gate
// decides whether the runtime incarnation is still current.
func restrictedReport(verb string, fields map[string]string) error {
	return restrictedReportWithin(35*time.Second, verb, fields)
}

// hookReportTimeout bounds a generated hook's report. Hooks are async and
// best-effort, and Claude kills them shortly after this anyway: a report the
// daemon has not served by then is stale, and waiting longer only stacks
// processes and queued requests behind a busy daemon.
const hookReportTimeout = 10 * time.Second

// reportServiceRetry spaces retries while a new session's mailbox is unserved.
const reportServiceRetry = 100 * time.Millisecond

// reportWithin is restrictedReport bounded for a generated hook (hook=true) or
// with the explicit command's full wait.
func reportWithin(hook bool, verb string, fields map[string]string) error {
	if hook {
		return restrictedReportWithin(hookReportTimeout, verb, fields)
	}
	return restrictedReport(verb, fields)
}

func restrictedReportWithin(timeout time.Duration, verb string, fields map[string]string) error {
	if !sessionreport.IsVerb(verb) {
		return fmt.Errorf("unknown self report %q", verb)
	}
	client, err := openRestrictedSessionRPC()
	if err != nil {
		return fmt.Errorf("open fixed session report context: %w", err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	reportFields := cloneCLIFields(fields)
	if reportFields == nil {
		reportFields = make(map[string]string)
	}
	// One call: the daemon applies a report that names no runtime generation
	// to the current runtime. It used to take a report-context query first,
	// which doubled every hook's and status line's mailbox traffic for a value
	// the daemon's effect gate re-checks anyway (2026-11-22 saturation).
	result, callErr := untilServed(ctx, "deliver self report", func() (sessionrpc.Result, error) {
		return client.Action(ctx, sessionrpc.Action{Verb: verb, Fields: reportFields})
	})
	if legacyReportDaemon(callErr) {
		// A daemon that predates the optional field refuses the report as
		// invalid without applying it: fetch the generation the old way.
		generation, err := reportContextGeneration(ctx, client)
		if err != nil {
			return err
		}
		reportFields[sessionreport.FieldRuntimeGeneration] = generation
		result, callErr = client.Action(ctx, sessionrpc.Action{Verb: verb, Fields: reportFields})
	}
	var response core.Result
	if len(result.Body) != 0 {
		if err := json.Unmarshal(result.Body, &response); err != nil {
			return fmt.Errorf("decode self report response: %w", err)
		}
	}
	if callErr != nil {
		if response.Error != "" {
			return fmt.Errorf("%s: %w", response.Error, callErr)
		}
		return callErr
	}
	if len(result.Body) == 0 || !response.OK {
		if response.Error == "" {
			response.Error = "self report returned no successful result"
		}
		return fmt.Errorf("%s", response.Error)
	}
	return nil
}

// untilServed runs call, retrying while the session's mailbox has no service
// record yet: a freshly created session's runtime starts before the daemon
// serves its mailbox, and its first reports (session start, the initial
// prompt) would otherwise be lost, leaving the member "unknown" for its whole
// first turn. Nothing was published, so retrying within ctx is safe.
func untilServed(ctx context.Context, what string, call func() (sessionrpc.Result, error)) (sessionrpc.Result, error) {
	result, err := call()
	for errors.Is(err, os.ErrNotExist) {
		select {
		case <-ctx.Done():
			return result, fmt.Errorf("%s: session mailbox is not served yet: %w", what, err)
		case <-time.After(reportServiceRetry):
		}
		result, err = call()
	}
	return result, err
}

// legacyReportDaemon reports whether err is an older daemon refusing a self
// report for lacking runtime_generation. Such a refusal applies nothing.
func legacyReportDaemon(err error) bool {
	var remote *sessionrpc.RemoteError
	return errors.As(err, &remote) && remote.Status == sessionrpc.StatusInvalid && remote.Code == "report_invalid"
}

// reportContextGeneration asks the daemon for the current runtime generation:
// the legacy report preamble, needed only by a daemon older than this client.
func reportContextGeneration(ctx context.Context, client restrictedSessionRPC) (string, error) {
	contextResult, err := untilServed(ctx, "read current report context", func() (sessionrpc.Result, error) {
		return client.Query(ctx, sessionrpc.Query{Verb: sessionreport.Context})
	})
	if err != nil {
		return "", fmt.Errorf("read current report context: %w", err)
	}
	var reportContext struct {
		RuntimeGeneration string `json:"runtime_generation"`
	}
	if len(contextResult.Body) == 0 || json.Unmarshal(contextResult.Body, &reportContext) != nil || reportContext.RuntimeGeneration == "" {
		return "", fmt.Errorf("read current report context: invalid response")
	}
	return reportContext.RuntimeGeneration, nil
}

func cloneCLIFields(fields map[string]string) map[string]string {
	if fields == nil {
		return nil
	}
	out := make(map[string]string, len(fields))
	for key, value := range fields {
		out[key] = value
	}
	return out
}
