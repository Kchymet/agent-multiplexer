package direct

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"amux/internal/access"
	"amux/internal/agent"
	"amux/internal/core"
	amuxstore "amux/internal/store"
	"amux/internal/wsops"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// This file is the workflow action surface beyond S1's prompt and stop. Every
// verb needs its own additive permission (VerbPermissions), takes an exact
// field allowlist validated here before any backend call, and is admitted
// against a fresh inventory row (admissible) before it runs. The daemon is
// still authoritative for everything it checks itself (repo grant ceilings,
// open pull-request guards, permission request/runtime-generation binding,
// goal mode): these checks only ever narrow what reaches it.

// ── field validation (no backend) ─────────────────────────────────────────

// fieldText is a bounded, valid UTF-8, nonblank text field.
func fieldText(v string, max int) bool {
	return strings.TrimSpace(v) != "" && len(v) <= max && utf8.ValidString(v)
}

// fieldLine is fieldText without control characters (names, ids, models).
func fieldLine(v string, max int) bool {
	if !fieldText(v, max) {
		return false
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

var (
	repoNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	modelRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@\[\]-]{0,127}$`)
)

// onlyFields reports whether every key of f is in allowed.
func onlyFields(f map[string]string, allowed ...string) bool {
	for k := range f {
		if !slices.Contains(allowed, k) {
			return false
		}
	}
	return true
}

// creationModes are the session modes a creation verb may name.
var creationModes = []string{amuxstore.ModeTask, amuxstore.ModeInteractive}

// validRepoList checks a comma-joined repo name list's syntax (an empty list is
// allowed: it means no repositories).
func validRepoList(v string) bool {
	if len(v) > MaxJSONBody/4 {
		return false
	}
	for _, r := range amuxstore.SplitRepos(v) {
		if !repoNameRE.MatchString(r) {
			return false
		}
	}
	return true
}

// validCreationFields checks the optional fields every creation verb shares.
// identity_mode, when present, must be machine: direct clients never select a
// credential source.
func validCreationFields(f map[string]string) bool {
	if v, ok := f["identity_mode"]; ok && v != hp.IdentityMachine {
		return false
	}
	if v, ok := f["name"]; ok && !fieldLine(v, MaxNameBytes) {
		return false
	}
	if v, ok := f["prompt"]; ok && !fieldText(v, MaxPromptBytes) {
		return false
	}
	if v, ok := f["mode"]; ok && !slices.Contains(creationModes, v) {
		return false
	}
	for _, k := range []string{"model", wsops.FieldCoordinatorModel} {
		if v, ok := f[k]; ok && !modelRE.MatchString(v) {
			return false
		}
	}
	for _, k := range []string{"agent", wsops.FieldCoordinator} {
		if v, ok := f[k]; ok && !agent.Known(v) {
			return false
		}
	}
	if v, ok := f["repos"]; ok && !validRepoList(v) {
		return false
	}
	return true
}

// creationFields copies f without identity_mode (validated as machine, which is
// the daemon's only behaviour) so the daemon sees only its own vocabulary.
func creationFields(f map[string]string) map[string]string {
	out := make(map[string]string, len(f))
	for k, v := range f {
		if k != "identity_mode" {
			out[k] = v
		}
	}
	return out
}

func validGoalFields(f map[string]string) bool {
	if !onlyFields(f, hp.FieldGoalStatus, hp.FieldGoalObjective, hp.FieldGoalBudget) {
		return false
	}
	status, hasStatus := f[hp.FieldGoalStatus]
	objective, hasObjective := f[hp.FieldGoalObjective]
	budget, hasBudget := f[hp.FieldGoalBudget]
	if hasStatus && !hp.GoalStatuses[status] {
		return false
	}
	if hasObjective && !fieldText(objective, MaxPromptBytes) {
		return false
	}
	if hasBudget {
		n, err := strconv.ParseInt(budget, 10, 64)
		if err != nil || n <= 0 || strconv.FormatInt(n, 10) != budget {
			return false
		}
	}
	if status == hp.GoalClear && (hasObjective || hasBudget) {
		return false
	}
	return hasStatus || hasObjective || hasBudget
}

func steerAction(sid string, fields map[string]string) core.Action {
	return core.Action{Action: core.ActionSteer, ID: sid, Fields: fields}
}

// actionFor validates an action request's verb and fields against an exact
// allowlist and returns the daemon action. It does not consult the backend.
func actionFor(sid string, req ActionRequest) (core.Action, error) {
	f := req.Fields
	switch req.Verb {
	case VerbPrompt, VerbInterject:
		if !onlyFields(f, hp.FieldText) || !fieldText(f[hp.FieldText], MaxPromptBytes) {
			return core.Action{}, errBadRequest
		}
		verb := core.SteerPrompt
		if req.Verb == VerbInterject {
			verb = core.SteerInterject
		}
		return steerAction(sid, map[string]string{core.SteerVerb: verb, core.SteerText: f[hp.FieldText]}), nil
	case VerbStop:
		if len(f) != 0 {
			return core.Action{}, errBadRequest
		}
		return steerAction(sid, map[string]string{core.SteerVerb: core.SteerStop}), nil
	case VerbPermission:
		// Both correlation fields are mandatory: the daemon consumes the decision
		// only for the exact request it observed open on the exact live runtime
		// generation that published it.
		if !onlyFields(f, hp.FieldRequestID, hp.FieldRuntimeGeneration, hp.FieldDecision, hp.FieldReason) ||
			!fieldLine(f[hp.FieldRequestID], MaxFieldBytes) || !fieldLine(f[hp.FieldRuntimeGeneration], MaxFieldBytes) ||
			(f[hp.FieldDecision] != hp.DecisionAllow && f[hp.FieldDecision] != hp.DecisionDeny) {
			return core.Action{}, errBadRequest
		}
		out := map[string]string{
			core.SteerVerb: core.SteerPermission, core.SteerRequestID: f[hp.FieldRequestID],
			access.RuntimeGenerationField: f[hp.FieldRuntimeGeneration], core.SteerDecision: f[hp.FieldDecision],
		}
		if reason, ok := f[hp.FieldReason]; ok {
			if len(reason) > MaxReasonBytes || !utf8.ValidString(reason) {
				return core.Action{}, errBadRequest
			}
			out[core.SteerReason] = reason
		}
		return steerAction(sid, out), nil
	case VerbGoal:
		if !validGoalFields(f) {
			return core.Action{}, errBadRequest
		}
		out := map[string]string{core.SteerVerb: core.SteerGoal}
		for k, v := range f {
			out[k] = v
		}
		return steerAction(sid, out), nil
	case VerbStart:
		if len(f) != 0 {
			return core.Action{}, errBadRequest
		}
		return core.Action{Action: core.ActionStart, ID: sid}, nil
	case VerbRename:
		if !onlyFields(f, "name") || !fieldLine(f["name"], MaxNameBytes) {
			return core.Action{}, errBadRequest
		}
		return core.Action{Action: core.ActionRename, ID: sid, Fields: map[string]string{"name": f["name"]}}, nil
	case VerbArchive, VerbUnarchive:
		// Explicit, never a toggle, and never forced past the daemon's open
		// pull-request guard.
		if len(f) != 0 {
			return core.Action{}, errBadRequest
		}
		archived := "true"
		if req.Verb == VerbUnarchive {
			archived = "false"
		}
		return core.Action{Action: core.ActionSetArchived, ID: sid, Fields: map[string]string{"archived": archived}}, nil
	case VerbAddAgent:
		if !onlyFields(f, "agent", "prompt", "mode", "model", "repos", "identity_mode") || !validCreationFields(f) {
			return core.Action{}, errBadRequest
		}
		return core.Action{Action: core.ActionAddAgent, ID: sid, Fields: creationFields(f)}, nil
	}
	return core.Action{}, errUnsupported
}

// createActionFor validates a root creation request (PathWorkgroups). It does
// not consult the backend.
func createActionFor(req CreateRequest) (core.Action, error) {
	f := req.Fields
	switch req.Verb {
	case VerbNewWorkgroup:
		if !onlyFields(f, "name", "prompt", "repos", "agent", "mode", "model",
			wsops.FieldCoordinator, wsops.FieldCoordinatorModel, "identity_mode") || !validCreationFields(f) {
			return core.Action{}, errBadRequest
		}
		return core.Action{Action: core.ActionNewWorkgroup, Fields: creationFields(f)}, nil
	case VerbNewWorkgroupEmpty:
		// A coordinator with an explicit repo ceiling and zero members: repos is
		// required (possibly empty), and nothing describes a member.
		if _, ok := f["repos"]; !ok {
			return core.Action{}, errBadRequest
		}
		if !onlyFields(f, "name", "prompt", "repos", wsops.FieldCoordinator, wsops.FieldCoordinatorModel, "identity_mode") ||
			!validCreationFields(f) {
			return core.Action{}, errBadRequest
		}
		return core.Action{Action: core.ActionCreateWorkspace, Fields: creationFields(f)}, nil
	}
	return core.Action{}, errUnsupported
}

// requestedHarnesses lists the harnesses a creation action will launch, so the
// request is checked against what this machine verified it can run (mirrors
// the relay provider's rule).
func requestedHarnesses(a core.Action) []string {
	worker := agent.Canonical(a.Fields["agent"])
	switch a.Action {
	case core.ActionAddAgent:
		return []string{worker}
	case core.ActionCreateWorkspace:
		return []string{wsops.CoordinatorKind(a.Fields[wsops.FieldCoordinator])}
	}
	kinds := []string{wsops.CoordinatorKind(a.Fields[wsops.FieldCoordinator])}
	if len(amuxstore.SplitRepos(a.Fields["repos"])) > 0 && worker != kinds[0] {
		kinds = append(kinds, worker)
	}
	return kinds
}

// ── per-row admission (fresh inventory) ───────────────────────────────────

// archivedAncestor reports whether any ancestor of row (not row itself) is
// archived, from the fresh inventory. A broken chain counts as archived.
func archivedAncestor(row core.Session, byID map[string]core.Session) bool {
	if row.RootID == "" || row.RootID == row.ID {
		return false
	}
	parent, ok := byID[row.RootID]
	if !ok {
		return true
	}
	return effectivelyArchived(parent, byID)
}

// lifecycleRow reports a row amux manages as a workgroup session (a coordinator
// root or a member): not the console, not a repo home, not a detached session.
func lifecycleRow(row core.Session) bool {
	if isConsole(row) || row.Role == hp.RoleRepo || row.Source != "workspace" {
		return false
	}
	return row.Section == hp.SectionWorkgroups || row.Section == hp.SectionArchived
}

// workgroupRoot reports a coordinator workgroup root.
func workgroupRoot(row core.Session) bool {
	return lifecycleRow(row) && row.IsRoot && row.Role == hp.RoleCoordinator
}

// admissible checks the fresh target row for the verb: archive state first
// (every verb but unarchive refuses an effectively archived target; unarchive
// needs an archived target with no archived ancestor), then whether the daemon
// can serve the verb for this row.
func admissible(row core.Session, byID map[string]core.Session, verb string) error {
	archived := effectivelyArchived(row, byID)
	if verb == VerbUnarchive {
		if !row.Archived && row.Section != hp.SectionArchived || archivedAncestor(row, byID) {
			return errArchived
		}
	} else if archived {
		return errArchived
	}
	caps := row.Caps
	if caps == nil {
		caps = &hp.SessionCaps{}
	}
	ok := false
	switch verb {
	case VerbPrompt:
		ok = caps.Prompt
	case VerbStop:
		ok = caps.Cancel
	case VerbInterject:
		ok = caps.Interject
	case VerbGoal:
		ok = caps.Goal
	case VerbPermission:
		ok = caps.Permission
	case VerbStart, VerbRename, VerbArchive, VerbUnarchive:
		ok = lifecycleRow(row)
	case VerbAddAgent:
		ok = workgroupRoot(row)
	}
	if !ok {
		return errUnsupported
	}
	return nil
}

// rowCaps is the grant's per-row control surface for a fresh inventory row.
func rowCaps(g Grant, row core.Session, byID map[string]core.Session) SessionCaps {
	can := func(verb string) bool {
		return g.Can(VerbPermissions[verb]) && admissible(row, byID, verb) == nil
	}
	return SessionCaps{
		Prompt: can(VerbPrompt), Stop: can(VerbStop),
		Interject: can(VerbInterject), Goal: can(VerbGoal), Permission: can(VerbPermission),
		Start: can(VerbStart), Rename: can(VerbRename), Archive: can(VerbArchive), Unarchive: can(VerbUnarchive),
		AddAgent: can(VerbAddAgent),
	}
}

// sessionVerbs lists, in a fixed order, the session verbs a grant's
// permissions admit (rows still decide per session).
func sessionVerbs(g Grant) []string {
	out := []string{}
	for _, v := range []string{VerbPrompt, VerbStop, VerbInterject, VerbGoal, VerbPermission,
		VerbStart, VerbRename, VerbArchive, VerbUnarchive, VerbAddAgent} {
		if g.Can(VerbPermissions[v]) {
			out = append(out, v)
		}
	}
	return out
}

// mayCreateRoots reports whether the grant may create new workgroup roots. A
// workgroup-scoped grant never can: a new root is outside every named
// workgroup, so creating one would mint authority outside the grant.
func mayCreateRoots(g Grant) bool {
	return g.Can(PermCreate) && g.Resource.Kind == ResourceAllSessions
}

// projectCatalog is the grant's view of the machine catalog: tracked repo names
// only for a grant that may create roots, and repo ceilings only for in-scope,
// non-archived workgroup roots of the fresh inventory.
func projectCatalog(g Grant, cat MachineCatalog, rows []core.Session) *CreationCatalog {
	out := &CreationCatalog{
		Harnesses: slices.Clone(cat.Harnesses), IdentityModes: []string{hp.IdentityMachine},
		Modes: slices.Clone(creationModes),
	}
	if out.Harnesses == nil {
		out.Harnesses = []CatalogHarness{}
	}
	if mayCreateRoots(g) {
		out.Repos = slices.Clone(cat.Repos)
		if out.Repos == nil {
			out.Repos = []string{}
		}
	}
	byID := indexRows(rows)
	for _, row := range rows {
		if !validSessionID(row.ID) || !inScope(g, row, byID) || admissible(row, byID, VerbAddAgent) != nil {
			continue
		}
		repos := slices.Clone(cat.Ceilings[row.ID])
		if repos == nil {
			repos = []string{}
		}
		out.Workgroups = append(out.Workgroups, CatalogWorkgroup{ID: row.ID, Repos: repos})
	}
	return out
}

// archiveAffected lists the sessions whose effective archive state an archive
// or unarchive of row changes (see ActionResponse.Affected). Other verbs
// affect nothing listed.
func archiveAffected(row core.Session, rows []core.Session, byID map[string]core.Session, verb string) []string {
	if verb != VerbArchive && verb != VerbUnarchive {
		return nil
	}
	out := []string{row.ID}
	if !row.IsRoot {
		return out
	}
	for _, r := range rows {
		if r.ID == row.ID || r.Archived || !validSessionID(r.ID) {
			continue
		}
		// A member of this root, not archived in its own right.
		if r.RootID != "" && rootOf(r, byID) == row.ID {
			out = append(out, r.ID)
		}
	}
	return out
}
