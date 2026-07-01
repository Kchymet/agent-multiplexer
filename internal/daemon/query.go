package daemon

import (
	"encoding/json"
	"fmt"

	"amux/internal/core"
	"amux/internal/store"
)

// query answers a client's ActionQuery by building the requested read model from
// the store and sending it back as a Data frame. The daemon owns store access;
// this is what lets the CLI list repos and workgroups without opening the DB
// itself. Unknown queries and store errors come back as a failed Data frame.
func (d *Daemon) query(cl *connState, a core.Action) {
	rows, err := d.readModel(a.Query)
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

// readModel returns the store-backed slice named by q. It's split out so the
// marshalling and framing in query stay uniform across query names.
func (d *Daemon) readModel(q string) (any, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()

	switch q {
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
				agent := s.Agent
				if agent == "" {
					agent = "claude"
				}
				wg.Agents = append(wg.Agents, core.AgentRow{
					ID: s.ID, Agent: agent, Mode: s.Mode, Repos: s.Repo, Archived: s.Archived,
				})
			}
			rows = append(rows, wg)
		}
		return rows, nil
	default:
		return nil, fmt.Errorf("unknown query %q", q)
	}
}
