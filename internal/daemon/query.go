package daemon

import (
	"encoding/json"
	"fmt"

	"amux/internal/agent"
	"amux/internal/amuxcfg"
	"amux/internal/codexapp"
	"amux/internal/core"
	"amux/internal/store"

	"github.com/kchymet/agent-multiplexer/harnessproto"
)

// query answers a client's ActionQuery by building the requested read model from
// the store and sending it back as a Data frame. The daemon owns store access;
// this is what lets the CLI list repos and workgroups without opening the DB
// itself. Unknown queries and store errors come back as a failed Data frame.
func (d *Daemon) query(cl *connState, a core.Action) {
	rows, err := d.readModel(a)
	if err != nil {
		cl.send(core.Data{Type: core.FrameData, Query: a.Query, OK: false, Error: err.Error()})
		return
	}
	blob, err := json.Marshal(rows)
	if err != nil {
		cl.send(core.Data{Type: core.FrameData, Query: a.Query, OK: false, Error: err.Error()})
		return
	}
	cl.send(core.Data{Type: core.FrameData, Query: a.Query, OK: true, Rows: blob})
}

// readModel returns the read model named by a.Query. It's split out so the
// marshalling and framing in query stay uniform across query names. The daemon is
// the sole store owner, so this — and the poll loop — are the only readers.
func (d *Daemon) readModel(a core.Action) (any, error) {
	if a.Query == core.QueryCodexControl {
		return d.codexControl, d.configErr
	}
	// QuerySnapshot serves the already-computed rail (no store read): it's the same
	// inventory the poll loop caches and broadcasts, handed to a peer that would
	// otherwise open the store itself.
	if a.Query == core.QuerySnapshot {
		return d.snapshot().Sessions, nil
	}

	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	switch a.Query {
	case core.QueryRuntimePath, core.QueryRuntimeRecord:
		// Resolve a session id to its runtime transcript — and the runtime that
		// wrote it — via its harness, so the provider need not open the store to
		// tail transcripts. An empty path means no supported record; the provider
		// then emits nothing for that session (honest degradation). The older
		// QueryRuntimePath answers with just the path, so a provider built before
		// QueryRuntimeRecord keeps working against a newer daemon.
		rec, err := d.runtimeRecord(db, a.ID)
		if err != nil {
			return nil, err
		}
		if a.Query == core.QueryRuntimePath {
			return rec.Path, nil
		}
		return rec, nil
	case core.QueryRepos:
		repos, err := db.Repos()
		if err != nil {
			return nil, err
		}
		rows := make([]core.RepoRow, 0, len(repos))
		for _, r := range repos {
			rows = append(rows, core.RepoRow{Name: r.Name, Source: r.Source})
		}
		return rows, nil
	case core.QuerySessions:
		roots, err := db.Roots()
		if err != nil {
			return nil, err
		}
		rows := make([]core.WorkgroupRow, 0, len(roots))
		for _, r := range roots {
			scope := r.Scope
			if scope == "" {
				scope = store.ScopeWork
			}
			wg := core.WorkgroupRow{ID: r.ID, Scope: scope, Display: r.Display(), Role: r.Role()}
			if wg.Role != "" {
				wg.Agent, wg.Dir = agent.Canonical(r.Agent), r.Dir
				if wg.Dir == "" {
					wg.Dir = store.RootDir(r.ID) // a root predating default sessions
				}
			}
			subs, _ := db.Children(r.ID)
			for _, s := range subs {
				wg.Agents = append(wg.Agents, core.AgentRow{
					Dir: s.Dir,
					ID:  s.ID, Agent: agent.Canonical(s.Agent), Mode: s.Mode, Repos: s.Repo,
					Branch: s.Branch, Archived: s.Archived,
				})
			}
			rows = append(rows, wg)
		}
		return rows, nil
	default:
		return nil, fmt.Errorf("unknown query %q", a.Query)
	}
}

// runtimeRecord resolves a session id to its on-disk runtime transcript and the
// runtime (agent kind) that wrote it. A tracked session answers through its own
// harness, as does the console (published like an agent but synthetic rather than
// a store row); an untracked id is itself a conversation id, so it resolves from
// the default harness's transcript listing. An id that is none of those yields a
// zero record rather than an error.
//
// A tracked session always names amux's own journal, transcript or not: the
// journal is keyed by the amux session id, and a session that has never run —
// exactly the one an accepted `prompt` is cold-starting — has nothing else to
// stream. So a zero Path no longer means "nothing to read here".
// structuredResolvable reports whether a Codex session should resolve to its
// single structured event-log source. True when the opt-in gate is on (the session
// will run — or is running — structured, so the canonical source is stable from cold)
// OR a structured identity was persisted (history stays readable after the App Server
// exits, and even after the gate is later turned off).
func (d *Daemon) structuredResolvable(id string) bool {
	if d.codexControl.Effective == amuxcfg.AppServer {
		return true
	}
	_, ok := codexapp.LoadIdentity(id)
	return ok
}

func (d *Daemon) runtimeRecord(db *store.DB, id string) (core.RuntimeRecord, error) {
	s, ok, err := db.GetSession(id)
	if err != nil {
		return core.RuntimeRecord{}, err
	}
	if !ok || s.Role() == store.RoleCoordinator {
		// The console is published in the same inventory as every agent but is a
		// synthetic session rather than a store row; a repo home may not exist yet
		// for a repo tracked before default sessions; a coordinator root may still
		// lack its pinned conversation. Resolve them the way every other
		// daemon-side lookup does (lookupSession) instead of answering "nothing to
		// read" for a row the daemon itself advertises.
		s, ok, err = lookupSession(id)
		if err != nil {
			return core.RuntimeRecord{}, err
		}
	}
	if ok {
		// A structured-control session (AGE-181) records its normalized events in a
		// supervisor-written log, not the runtime's own transcript, so resolve it to
		// that log with Structured set — and to that log ALONE. It is the single
		// canonical source: amux's own cold-start/failure notices are appended into the
		// same file (codexapp.AppendNotice), not carried as a separate journal source,
		// so replay ordinals stay stable (one append-only file ⇒ ordinal == line number,
		// identical live or on reconnect; a second journal file merged at read time
		// could not preserve that when a late notice interleaves with turn output —
		// ROOT interleaved-replay audit).
		//
		// Resolve structured from COLD, before the supervisor persists identity: the
		// gate (startup selection + Codex agent) already determines the session will run
		// structured, so the canonical source is stable from the first subscription and
		// a provider that subscribes while Codex is still starting simply tails the
		// (not-yet-written) log and follows it — no reconnect, no source switch. A
		// persisted identity still resolves it too, so history stays readable after the
		// App Server exits (and after the gate is later turned off).
		if agent.Canonical(s.Agent) == harnessproto.RuntimeCodex && d.structuredResolvable(id) {
			return core.RuntimeRecord{
				Runtime:    harnessproto.RuntimeCodex,
				Path:       codexapp.EventLogPathFor(id),
				Structured: true,
			}, nil
		}
		h := agent.HarnessFor(s.Agent)
		path, _ := h.RuntimeTranscriptPath(s)
		perms, _ := h.RuntimePermissionPath(s)
		return core.RuntimeRecord{
			Runtime: s.Agent, Path: path, Permissions: perms, Journal: core.JournalPath(id),
		}, nil
	}
	kind := agent.DefaultKind()
	h := agent.HarnessFor(kind)
	for _, info := range h.ListSessions() {
		if info.ID == id {
			// An untracked id IS the conversation id, which is what the permission
			// hooks key their journal on — so the harness can resolve one from a
			// session carrying nothing else.
			perms, _ := h.RuntimePermissionPath(store.Session{ClaudeID: id})
			return core.RuntimeRecord{
				Runtime: kind, Path: info.Path, Permissions: perms, Journal: core.JournalPath(id),
			}, nil
		}
	}
	return core.RuntimeRecord{}, nil
}
