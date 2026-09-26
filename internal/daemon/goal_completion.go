package daemon

import (
	"context"
	"log"

	"amux/internal/agent"
	"amux/internal/core"
	"amux/internal/store"
	"amux/internal/wsops"
)

// Reconcile outside the App Server notification goroutine: closing a
// supervisor from its own read loop would deadlock. The same admission locks
// as host/mailbox actions prevent a stale completion from archiving a restored
// workgroup or killing a replacement runtime.
func (d *Daemon) completeWorkgroupGoals(ctx context.Context) {
	if d.codex == nil || ctx.Err() != nil {
		return
	}
	if d.sessionRPC != nil {
		d.sessionRPC.dispatchMu.Lock()
		defer d.sessionRPC.dispatchMu.Unlock()
	}
	d.effectMu.Lock()
	defer d.effectMu.Unlock()
	db, err := store.Open()
	if err != nil {
		log.Printf("goal completion: %v", err)
		return
	}
	defer db.Close()
	sessions, err := db.AllSessions()
	if err != nil {
		log.Printf("goal completion: %v", err)
		return
	}
	for _, session := range sessions {
		if !agent.NativeGoals(session) || session.Archived {
			continue
		}
		if sup, ok := d.codex.Get(session.ID); ok {
			d.completeWorkgroupGoal(ctx, session, sup)
		}
	}
}

type workgroupCompleter interface {
	CompleteWorkgroup(func() error) (bool, error)
}

// Caller holds effect admission and supplies the current live supervisor.
func (d *Daemon) completeWorkgroupGoal(ctx context.Context, session store.Session, sup workgroupCompleter) {
	if session.Archived || !agent.NativeGoals(session) {
		return
	}
	done, err := sup.CompleteWorkgroup(func() error {
		return wsops.SetArchived(ctx, session.ID, true)
	})
	if err != nil {
		log.Printf("complete workgroup %s: %v", session.ID, err)
		return
	}
	if done {
		structuredJournal(session.ID, core.JournalInfo, "goal complete; workgroup marked done")
		d.killEngineFor(session.ID)
		d.triggerPoll()
	}
}
