package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"amux/internal/access"
	"amux/internal/amuxcfg"
	"amux/internal/core"
	"amux/internal/sessionrpc"
	"amux/internal/store"
)

// Host-scope coordinator archive (coordinator.archive_scope=host).
//
// access.Policy decides the shape (a coordinator archiving a foreign, active
// ordinary agent or workgroup, never a restore, the console or a repo home)
// and consults the host setting. The daemon adds what only it knows — live
// activity — at the same admission boundaries, and records an audit entry for
// every archive that lands.

// maxHostSessionRows bounds the staleness projection well inside the 64 KiB
// session-RPC response budget.
const maxHostSessionRows = 200

// errHostArchiveBusy is the guard error when a target became active between
// the admission check and the effect.
var errHostArchiveBusy = errors.New("host-scope archive target is active")

func hostArchiveEnabled(context.Context) bool { return amuxcfg.HostArchiveScope() }

// hostScopeArchive reports whether req relies on host-scope archive authority,
// resolving the caller and target from daemon-owned state.
func (r *sessionRuntime) hostScopeArchive(ctx context.Context, principal access.Principal, req access.Request) (subject, target access.Resource, ok bool) {
	if principal.Kind != access.SubjectSession || req.Route != access.RouteAction || req.Verb != core.ActionSetArchived {
		return subject, target, false
	}
	subject, found, err := r.resolver.Lookup(ctx, principal.SubjectID)
	if err != nil || !found || subject.Archived {
		return subject, target, false
	}
	target, found, err = r.resolver.Lookup(ctx, req.ID)
	if err != nil || !found {
		return subject, target, false
	}
	return subject, target, access.HostScopeArchive(subject, target, req)
}

// hostArchiveRefusal names why target cannot be archived now ("" when it can):
// it, or for a workgroup any active member, is running, waiting on a
// permission or input prompt, or of unknown activity. The daemon's published
// rail is the activity source: a workgroup row already carries the most
// demanding of its coordinator's and members' states.
func hostArchiveRefusal(snapshot core.Snapshot, target access.Resource) string {
	seen := false
	for _, s := range snapshot.Sessions {
		if s.Archived || (s.ID != target.ID && (target.Role != store.RoleCoordinator || s.RootID != target.ID)) {
			continue
		}
		if s.ID == target.ID {
			seen = true
		}
		if code := activityRefusal(s.State); code != "" {
			return code
		}
	}
	if !seen {
		return "archive_target_state_unknown"
	}
	return ""
}

func activityRefusal(state string) string {
	switch state {
	case core.StateRunning:
		return "archive_target_running"
	case core.StateWaiting:
		// Waiting is a pending permission request or an input prompt.
		return "archive_target_waiting"
	case core.StateIdle, core.StateReady, "":
		return ""
	default:
		return "archive_target_state_unknown"
	}
}

// dispatchHostArchive runs a host-scope archive on the split archive path
// (the target's and its members' locks, see archiveScopedAction), with the
// activity check both at admission, for a clear refusal code, and in the
// effect guard, so a target that starts a turn in between is not archived.
func (r *sessionRuntime) dispatchHostArchive(ctx context.Context, request sessionrpc.DispatchRequest, req access.Request, action core.Action, subject, target access.Resource) (sessionrpc.DispatchResult, error) {
	release := r.d.admitShared()
	denied, ok := r.admissionCheck(ctx, request.Principal, req)
	refusal := ""
	if ok {
		refusal = hostArchiveRefusal(r.d.snapshot(), target)
	}
	release()
	if !ok {
		return denied, nil
	}
	if refusal != "" {
		return rpcDenied(refusal), nil
	}
	guarded := withGuardSubject(withAccessGuard(ctx, func(checkCtx context.Context) error {
		if err := r.finalAuthorize(checkCtx, request.Principal, req); err != nil {
			return err
		}
		if code := hostArchiveRefusal(r.d.snapshot(), target); code != "" {
			return fmt.Errorf("%w: %s", errHostArchiveBusy, code)
		}
		return nil
	}), request.Principal.SubjectID)
	result := r.d.handle(guarded, action)
	if result.OK {
		recordHostArchive(subject, target)
	}
	return r.actionResult(result)
}

// hostArchiveAudit is one line of the coordinator archive audit log.
type hostArchiveAudit struct {
	At          string `json:"at"`
	Coordinator string `json:"coordinator"`
	Target      string `json:"target"`
	TargetRole  string `json:"targetRole,omitempty"`
	TargetRoot  string `json:"targetRoot,omitempty"`
	Scope       string `json:"scope"`
}

// HostArchiveAuditPath is the append-only JSONL audit of coordinator archives
// performed under coordinator.archive_scope=host.
func HostArchiveAuditPath() string {
	return filepath.Join(core.StateDir(), "audit", "coordinator-archive.jsonl")
}

var hostArchiveAuditMu sync.Mutex

// recordHostArchive logs the archive and appends it to the audit file. The
// archive already happened; an audit write failure is logged, never undone.
func recordHostArchive(subject, target access.Resource) {
	role := target.Role
	if role == "" {
		role = "agent"
	}
	log.Printf("audit: coordinator %s archived %s %s (root %q) under %s=%s",
		subject.ID, role, target.ID, target.RootID, amuxcfg.CoordinatorArchiveScopeKey, amuxcfg.ArchiveScopeHost)
	entry := hostArchiveAudit{
		At: time.Now().UTC().Format(time.RFC3339Nano), Coordinator: subject.ID,
		Target: target.ID, TargetRole: role, TargetRoot: target.RootID, Scope: amuxcfg.ArchiveScopeHost,
	}
	if err := appendHostArchiveAudit(entry); err != nil {
		log.Printf("audit: write %s: %v", HostArchiveAuditPath(), err)
	}
}

func appendHostArchiveAudit(entry hostArchiveAudit) error {
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	hostArchiveAuditMu.Lock()
	defer hostArchiveAuditMu.Unlock()
	path := HostArchiveAuditPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// hostSessionsView is the QueryHostSessions projection: empty unless the
// caller is an active coordinator and the host enabled host scope. It names
// only ids, titles, roles, activity and timestamps — no paths, branches,
// prompts or transcripts — and grants nothing: every effect still goes through
// Policy.Authorize.
func (r *sessionRuntime) hostSessionsView(ctx context.Context, principal access.Principal) (core.HostSessionsView, error) {
	var view core.HostSessionsView
	if err := ctx.Err(); err != nil {
		return view, err
	}
	subject, ok, err := r.resolver.Lookup(ctx, principal.SubjectID)
	if err != nil {
		return view, err
	}
	if principal.Kind != access.SubjectSession || !ok || subject.Archived {
		return view, access.ErrDenied
	}
	if subject.Role != store.RoleCoordinator || !hostArchiveEnabled(ctx) {
		return view, nil
	}
	view.Enabled = true
	states := map[string]string{}
	snapshot := r.d.snapshot()
	for _, s := range snapshot.Sessions {
		if !s.Archived {
			states[s.ID] = s.State
		}
	}
	err = r.resolver.withStore(func(db policyStore) error {
		roots, err := db.Roots()
		if err != nil {
			return err
		}
		for _, root := range roots {
			if root.Archived {
				continue
			}
			children, err := db.Children(root.ID)
			if err != nil {
				return err
			}
			active := 0
			for _, child := range children {
				if child.Archived {
					continue
				}
				active++
				view.Sessions = appendHostSessionRow(view.Sessions, subject, child, states, snapshot, 0)
			}
			view.Sessions = appendHostSessionRow(view.Sessions, subject, root, states, snapshot, active)
		}
		return nil
	})
	if err != nil {
		return core.HostSessionsView{}, err
	}
	sort.SliceStable(view.Sessions, func(i, j int) bool {
		return view.Sessions[i].LastActivity < view.Sessions[j].LastActivity
	})
	if len(view.Sessions) > maxHostSessionRows {
		view.Sessions, view.Truncated = view.Sessions[:maxHostSessionRows], true
	}
	return view, nil
}

func appendHostSessionRow(rows []core.HostSessionRow, subject access.Resource, s store.Session, states map[string]string, snapshot core.Snapshot, members int) []core.HostSessionRow {
	target := policyResource(s)
	if access.InWorkgroupScope(subject, target) {
		return rows // the caller's own workgroup is already in its snapshot
	}
	state, ok := states[s.ID]
	if !ok {
		state = core.StateUnknown
	} else if state == "" {
		state = core.StateIdle
	}
	last := s.Created
	if rec, found := core.SessionHookState(s.ID, s.ClaudeID); found && rec.Updated > last {
		last = rec.Updated
	}
	row := core.HostSessionRow{
		ID: s.ID, RootID: s.RootID, Title: s.Display(), Role: s.Role(), Agent: s.Agent,
		State: state, Archived: s.Archived, Members: members, Created: s.Created, LastActivity: last,
	}
	switch {
	case !access.HostArchivableTarget(target):
		row.Reason = "role_not_archivable"
	default:
		row.Reason = hostArchiveRefusal(snapshot, target)
	}
	row.Archivable = row.Reason == ""
	return append(rows, row)
}
