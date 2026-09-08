package daemon

import (
	"fmt"
	"strings"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/store"
)

// canonicalSessionOperation validates the complete restricted RPC vocabulary
// and constructs the one core.Action that will execute. Its input is also the
// exact access.Request passed to access.Policy: callers must not authorize one
// value and then decode or dispatch another representation.
func canonicalSessionOperation(req access.Request) (core.Action, error) {
	if req.Tab != 0 {
		return core.Action{}, fmt.Errorf("session RPC does not support streaming tabs")
	}
	switch req.Route {
	case access.RouteQuery:
		if err := validateSessionQuery(req); err != nil {
			return core.Action{}, err
		}
		return core.Action{Action: core.ActionQuery, Query: req.Verb, ID: req.ID, Fields: cloneFields(req.Fields)}, nil
	case access.RouteAction:
		if err := validateSessionAction(req); err != nil {
			return core.Action{}, err
		}
		return core.Action{
			Action: req.Verb, ID: req.ID, Target: req.Target,
			Fields: cloneFields(req.Fields),
		}, nil
	default:
		return core.Action{}, fmt.Errorf("unknown session RPC route %q", req.Route)
	}
}

func validateSessionQuery(req access.Request) error {
	if req.Target != "" {
		return fmt.Errorf("query %q has unsupported target", req.Verb)
	}
	switch req.Verb {
	case core.QueryVersion, core.QueryCodexControl, core.QueryRepos,
		core.QuerySessions, core.QuerySnapshot:
		if req.ID != "" || len(req.Fields) != 0 {
			return fmt.Errorf("query %q does not accept an id", req.Verb)
		}
		return nil
	case core.QueryRuntimeEvents:
		if strings.TrimSpace(req.ID) == "" {
			return fmt.Errorf("query %q requires an id", req.Verb)
		}
		if err := validateFields(req.Fields, core.RuntimeEventsCursorField, core.RuntimeEventsAfterSequenceField); err != nil {
			return err
		}
		_, _, err := parseSessionEventFields(req.Fields)
		return err
	case core.QueryRuntimePath, core.QueryRuntimeRecord:
		// These names are recognized so policy can return access denied instead
		// of pretending they are an extensible route. Restricted principals are
		// never dispatched to the host-path readModel implementation.
		if strings.TrimSpace(req.ID) == "" || len(req.Fields) != 0 {
			return fmt.Errorf("query %q requires an id", req.Verb)
		}
		return nil
	default:
		return fmt.Errorf("unknown session RPC query %q", req.Verb)
	}
}

func validateSessionAction(req access.Request) error {
	if !core.KnownAction(req.Verb) {
		return fmt.Errorf("unknown session RPC action %q", req.Verb)
	}
	noTarget := func() error {
		if req.Target != "" {
			return fmt.Errorf("action %q does not accept target", req.Verb)
		}
		return nil
	}
	requireID := func() error {
		if strings.TrimSpace(req.ID) == "" {
			return fmt.Errorf("action %q requires an id", req.Verb)
		}
		return nil
	}
	noID := func() error {
		if req.ID != "" {
			return fmt.Errorf("action %q does not accept an id", req.Verb)
		}
		return nil
	}
	fields := func(allowed ...string) error { return validateFields(req.Fields, allowed...) }

	switch req.Verb {
	case core.ActionRefresh:
		return joinValidation(noID(), noTarget(), fields())
	case core.ActionStart:
		return joinValidation(requireID(), noTarget(), fields())
	case core.ActionAuthReload:
		if err := joinValidation(noID(), noTarget(), fields("force")); err != nil {
			return err
		}
		return optionalBool(req.Fields, "force")
	case core.ActionSteer:
		if err := joinValidation(requireID(), noTarget()); err != nil {
			return err
		}
		return validateSessionSteer(req.Fields)
	case core.ActionRename:
		return joinValidation(requireID(), noTarget(), fields("name"), requireField(req.Fields, "name"))
	case core.ActionMove:
		return joinValidation(requireID(), fields())
	case core.ActionDelete, core.ActionKill, core.ActionRmRepo:
		return joinValidation(requireID(), noTarget(), fields())
	case core.ActionArchive:
		return fmt.Errorf("toggle archive is host-only; restricted callers must request an explicit archive")
	case core.ActionSetArchived:
		if err := joinValidation(requireID(), noTarget(), fields("archived"), requireField(req.Fields, "archived")); err != nil {
			return err
		}
		if err := requiredBool(req.Fields, "archived"); err != nil {
			return err
		}
		if req.Fields["archived"] != "true" {
			return fmt.Errorf("restoring an archived session requires authenticated host regrant")
		}
		return nil
	case core.ActionAgentSetRepos:
		return joinValidation(requireID(), noTarget(), fields("repos"), requireField(req.Fields, "repos"))
	case core.ActionCoordinatorSetRepos:
		return fmt.Errorf("coordinator grant changes require the authenticated host")
	case core.ActionAddRepo:
		return joinValidation(noID(), noTarget(), fields("source"), requireNonemptyField(req.Fields, "source"))
	case core.ActionAddAgent:
		return joinValidation(requireID(), noTarget(), fields("agent", "prompt", "mode", "model", "repos"), validateCreationMode(req.Fields))
	case core.ActionNewRepoAgent:
		return joinValidation(requireID(), noTarget(), fields("agent", "prompt", "mode", "model"), validateCreationMode(req.Fields))
	case core.ActionNewWorkgroup:
		return joinValidation(noID(), noTarget(), fields("name", "prompt", "mode", "model", "agent", "repos", "linear"), validateCreationMode(req.Fields))
	case core.ActionCreateWorkspace:
		if err := joinValidation(noID(), noTarget(), fields("name", "prompt", "mode", "model", "agent", "repos", "defaultAgent"), validateCreationMode(req.Fields)); err != nil {
			return err
		}
		if value, ok := req.Fields["defaultAgent"]; ok && value != "1" {
			return fmt.Errorf("field %q must be 1", "defaultAgent")
		}
		return nil
	default:
		return fmt.Errorf("unknown session RPC action %q", req.Verb)
	}
}

func validateCreationMode(fields map[string]string) error {
	mode, ok := fields["mode"]
	if !ok {
		return nil
	}
	switch mode {
	case store.ModeTask, store.ModeInteractive:
		return nil
	default:
		return fmt.Errorf("field %q must be %q or %q", "mode", store.ModeTask, store.ModeInteractive)
	}
}

func validateSessionSteer(fields map[string]string) error {
	verb := fields[core.SteerVerb]
	switch verb {
	case core.SteerPrompt, core.SteerInterject:
		return joinValidation(
			validateFields(fields, core.SteerVerb, core.SteerText),
			requireNonemptyField(fields, core.SteerText),
		)
	case core.SteerStop:
		return validateFields(fields, core.SteerVerb)
	case core.SteerPermission:
		if err := joinValidation(
			validateFields(fields, core.SteerVerb, core.SteerDecision, core.SteerRequestID,
				core.SteerReason, access.RuntimeGenerationField),
			requireNonemptyField(fields, core.SteerRequestID),
			requireNonemptyField(fields, access.RuntimeGenerationField),
		); err != nil {
			return err
		}
		decision := fields[core.SteerDecision]
		if decision != core.SteerAllow && decision != core.SteerDeny {
			return fmt.Errorf("field %q must be %q or %q", core.SteerDecision, core.SteerAllow, core.SteerDeny)
		}
		return nil
	default:
		return fmt.Errorf("unknown steer verb %q", verb)
	}
}

func validateFields(fields map[string]string, allowed ...string) error {
	want := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		want[key] = struct{}{}
	}
	for key := range fields {
		if _, ok := want[key]; !ok {
			return fmt.Errorf("unknown field %q", key)
		}
	}
	return nil
}

func requireField(fields map[string]string, key string) error {
	if _, ok := fields[key]; !ok {
		return fmt.Errorf("missing field %q", key)
	}
	return nil
}

func requireNonemptyField(fields map[string]string, key string) error {
	if strings.TrimSpace(fields[key]) == "" {
		return fmt.Errorf("field %q is required", key)
	}
	return nil
}

func optionalBool(fields map[string]string, key string) error {
	if _, ok := fields[key]; !ok {
		return nil
	}
	return requiredBool(fields, key)
}

func requiredBool(fields map[string]string, key string) error {
	if fields[key] != "true" && fields[key] != "false" {
		return fmt.Errorf("field %q must be true or false", key)
	}
	return nil
}

func joinValidation(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func cloneFields(fields map[string]string) map[string]string {
	if fields == nil {
		return nil
	}
	out := make(map[string]string, len(fields))
	for key, value := range fields {
		out[key] = value
	}
	return out
}
