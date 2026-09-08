package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	client, err := openRestrictedSessionRPC()
	if err != nil {
		return fmt.Errorf("open fixed session control context: %w", err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	result, err := client.Query(ctx, sessionrpc.Query{Verb: name})
	if err != nil {
		return err
	}
	if len(result.Body) == 0 {
		return nil
	}
	return json.Unmarshal(result.Body, dst)
}

// restrictedReport publishes one self-only observation through the fixed
// authenticated session context. The context query and action deliberately use
// one client: the returned generation is only correlation, while the daemon's
// final effect gate decides whether that incarnation is still current.
func restrictedReport(verb string, fields map[string]string) error {
	if !sessionreport.IsVerb(verb) {
		return fmt.Errorf("unknown self report %q", verb)
	}
	client, err := openRestrictedSessionRPC()
	if err != nil {
		return fmt.Errorf("open fixed session report context: %w", err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	contextResult, err := client.Query(ctx, sessionrpc.Query{Verb: sessionreport.Context})
	if err != nil {
		return fmt.Errorf("read current report context: %w", err)
	}
	var reportContext struct {
		RuntimeGeneration string `json:"runtime_generation"`
	}
	if len(contextResult.Body) == 0 || json.Unmarshal(contextResult.Body, &reportContext) != nil || reportContext.RuntimeGeneration == "" {
		return fmt.Errorf("read current report context: invalid response")
	}
	reportFields := cloneCLIFields(fields)
	if reportFields == nil {
		reportFields = make(map[string]string)
	}
	reportFields[sessionreport.FieldRuntimeGeneration] = reportContext.RuntimeGeneration
	result, callErr := client.Action(ctx, sessionrpc.Action{Verb: verb, Fields: reportFields})
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
