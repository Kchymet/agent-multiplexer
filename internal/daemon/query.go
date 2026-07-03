package daemon

import (
	"encoding/json"
	"fmt"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/store"
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
	case core.QueryRuntimePath:
		// Resolve a session id to its runtime transcript path via its harness, so
		// the provider need not open the store to tail transcripts. Empty string
		// (marshalled as "") means no supported record — honest degradation.
		s, ok, err := db.GetSession(a.ID)
		if err != nil {
			return nil, err
		}
		if ok {
			path, _ := agent.HarnessFor(s.Agent).RuntimeTranscriptPath(s)
			return path, nil
		}
		// Untracked/console: the id is itself a conversation id with no amux-store
		// row — resolve it from the default harness's own transcript listing.
		for _, info := range agent.HarnessFor(agent.DefaultKind()).ListSessions() {
			if info.ID == a.ID {
				return info.Path, nil
			}
		}
		return "", nil
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
			wg := core.WorkgroupRow{ID: r.ID, Scope: scope, Display: r.Display()}
			subs, _ := db.Children(r.ID)
			for _, s := range subs {
				wg.Agents = append(wg.Agents, core.AgentRow{
					ID: s.ID, Agent: agent.Canonical(s.Agent), Mode: s.Mode, Repos: s.Repo,
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
