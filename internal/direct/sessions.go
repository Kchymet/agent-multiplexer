package direct

import (
	"context"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"amux/internal/core"
	"amux/internal/hostbackend"
	"amux/internal/runtimeevents"
	hp "github.com/kchymet/agent-multiplexer/harnessproto"
)

// Backend is the elevated local surface the direct server calls only after a
// grant admits an operation. Production wraps hostbackend.HostDaemon and
// runtimeevents; tests count calls.
type Backend interface {
	// Snapshot is the fresh authoritative inventory used for authorization.
	Snapshot(ctx context.Context) ([]core.Session, error)
	// Apply runs one already-admitted action.
	Apply(ctx context.Context, a core.Action) (string, error)
	// History and Stream read one already-authorized session's transcript.
	History(ctx context.Context, sessionID string, beforeSeq int64, limit int) (hp.RuntimeEventHistory, bool)
	Stream(ctx context.Context, sessionID string, afterSeq int64) (<-chan hp.RuntimeEventBatch, bool)
}

// HostBackend is the production Backend. Transcript reads resolve the runtime
// record internally through the checked resolver; the record (host paths) is
// never returned to a client.
type HostBackend struct {
	H hostbackend.HostDaemon
}

func (b HostBackend) Snapshot(ctx context.Context) ([]core.Session, error) { return b.H.Snapshot(ctx) }

func (b HostBackend) Apply(ctx context.Context, a core.Action) (string, error) {
	return b.H.Apply(ctx, a)
}

func (b HostBackend) resolver() runtimeevents.ContextResolver {
	return func(ctx context.Context, sessionID string) (runtimeevents.Record, bool) {
		if !validSessionID(sessionID) || ctx.Err() != nil {
			return runtimeevents.Record{}, false
		}
		rec, err := b.H.RuntimeRecord(ctx, sessionID)
		if err != nil || (rec.Path == "" && rec.Journal == "") {
			return runtimeevents.Record{}, false
		}
		return runtimeevents.Record{
			Runtime: rec.Runtime, Path: rec.Path, Permissions: rec.Permissions, Journal: rec.Journal,
			Structured: rec.Structured, PermissionBindings: rec.PermissionBindings,
		}, true
	}
}

func (b HostBackend) History(ctx context.Context, sid string, before int64, limit int) (hp.RuntimeEventHistory, bool) {
	return runtimeevents.HistoryContext(b.resolver())(ctx, sid, before, limit)
}

func (b HostBackend) Stream(ctx context.Context, sid string, after int64) (<-chan hp.RuntimeEventBatch, bool) {
	return runtimeevents.StreamContext(b.resolver(), 0)(ctx, sid, after)
}

// validSessionID rejects path- or URI-shaped ids before any lookup, as the relay
// provider does.
func validSessionID(id string) bool {
	return id != "" && len(id) <= 256 && id != "." && id != ".." && utf8.ValidString(id) &&
		!strings.ContainsAny(id, "/\\\x00") && !strings.Contains(id, "://")
}

// ConsoleID is the machine-wide console session. It carries operator and
// account powers, so S1 direct access never exposes it: not in inventory,
// history, streams or actions, under any grant, including all_sessions.
const ConsoleID = "console"

func isConsole(row core.Session) bool { return row.ID == ConsoleID || row.Role == hp.RoleConsole }

// inScope reports whether row is inside the grant's resource scope, judged from
// the fresh inventory rows only. For a workgroup grant that is the workgroup's
// root row and every row whose root (followed through the authoritative rows)
// is that root.
func inScope(g Grant, row core.Session, byID map[string]core.Session) bool {
	if isConsole(row) {
		return false
	}
	switch g.Resource.Kind {
	case ResourceAllSessions:
		return true
	case ResourceWorkgroups:
		root := rootOf(row, byID)
		return root != "" && slices.Contains(g.Resource.Workgroups, root)
	}
	return false
}

// rootOf follows RootID links through the snapshot to the top-level root id.
// A row whose chain is broken or cyclic has no root.
func rootOf(row core.Session, byID map[string]core.Session) string {
	cur := row
	for range 16 {
		if cur.RootID == "" || cur.RootID == cur.ID {
			if cur.IsRoot || cur.Role == hp.RoleCoordinator {
				return cur.ID
			}
			return ""
		}
		parent, ok := byID[cur.RootID]
		if !ok {
			return ""
		}
		cur = parent
	}
	return ""
}

// effectivelyArchived reports a row that is archived itself or whose ancestors
// are, from the fresh inventory.
func effectivelyArchived(row core.Session, byID map[string]core.Session) bool {
	cur := row
	for range 16 {
		if cur.Archived || cur.Section == hp.SectionArchived {
			return true
		}
		if cur.RootID == "" || cur.RootID == cur.ID {
			return false
		}
		parent, ok := byID[cur.RootID]
		if !ok {
			return false
		}
		cur = parent
	}
	return true
}

// project is the allowlisted inventory row a client sees.
func project(g Grant, row core.Session, byID map[string]core.Session) Session {
	steer := g.Can(PermSteer) && !effectivelyArchived(row, byID)
	caps := SessionCaps{}
	if row.Caps != nil && steer {
		caps = SessionCaps{Prompt: row.Caps.Prompt, Stop: row.Caps.Cancel}
	}
	out := Session{
		ID: row.ID, Title: row.Title, Kind: row.Kind, Mode: row.Mode, Model: row.Model, RootID: row.RootID,
		IsRoot: row.IsRoot, Repos: row.Repos, Section: row.Section, State: row.State, Status: row.Status,
		Archived: row.Archived, StartedAt: row.StartedAt, Runtime: row.Runtime, ControlMode: row.ControlMode,
		Role: row.Role, Caps: caps,
	}
	if row.PRs != nil {
		out.PRs = slices.Clone([]hp.SessionPR(*row.PRs))
	}
	return out
}

// filtered returns the grant's view of a fresh snapshot.
func filtered(g Grant, rows []core.Session) []Session {
	byID := indexRows(rows)
	out := []Session{}
	for _, row := range rows {
		if validSessionID(row.ID) && inScope(g, row, byID) {
			out = append(out, project(g, row, byID))
		}
	}
	return out
}

func indexRows(rows []core.Session) map[string]core.Session {
	byID := make(map[string]core.Session, len(rows))
	for _, r := range rows {
		if r.ID != "" {
			byID[r.ID] = r
		}
	}
	return byID
}

var (
	errNotFound    = errors.New(CodeNotFound)
	errArchived    = errors.New(CodeArchived)
	errUnsupported = errors.New(CodeUnsupportedVerb)
	errBadRequest  = errors.New(CodeBadRequest)
)

// authorizeTarget reads the fresh inventory and returns the target row when it
// is inside the grant's scope. Not-in-scope and nonexistent are indistinguishable.
func authorizeTarget(ctx context.Context, b Backend, g Grant, sid string) (core.Session, map[string]core.Session, error) {
	rows, err := b.Snapshot(ctx)
	if err != nil {
		return core.Session{}, nil, err
	}
	byID := indexRows(rows)
	row, ok := byID[sid]
	if !ok || !inScope(g, row, byID) {
		return core.Session{}, nil, errNotFound
	}
	return row, byID, nil
}

// actionFor validates an S1 action request's verb and fields against an exact
// allowlist and returns the daemon action. It does not consult the backend.
func actionFor(sid string, req ActionRequest) (core.Action, error) {
	switch req.Verb {
	case VerbPrompt:
		for k := range req.Fields {
			if k != hp.FieldText {
				return core.Action{}, errBadRequest
			}
		}
		text := req.Fields[hp.FieldText]
		if strings.TrimSpace(text) == "" || len(text) > MaxPromptBytes || !utf8.ValidString(text) {
			return core.Action{}, errBadRequest
		}
		return core.Action{Action: core.ActionSteer, ID: sid, Fields: map[string]string{core.SteerVerb: core.SteerPrompt, core.SteerText: text}}, nil
	case VerbStop:
		if len(req.Fields) != 0 {
			return core.Action{}, errBadRequest
		}
		return core.Action{Action: core.ActionSteer, ID: sid, Fields: map[string]string{core.SteerVerb: core.SteerStop}}, nil
	}
	return core.Action{}, errUnsupported
}

// steerable checks the fresh target row for the verb's capability and archive
// state.
func steerable(row core.Session, byID map[string]core.Session, verb string) error {
	if effectivelyArchived(row, byID) {
		return errArchived
	}
	if row.Caps == nil {
		return errUnsupported
	}
	if verb == VerbPrompt && !row.Caps.Prompt || verb == VerbStop && !row.Caps.Cancel {
		return errUnsupported
	}
	return nil
}
