package daemon

import (
	"amux/internal/core"
	"amux/internal/runtimeevents"
	"amux/internal/store"
)

// loadPermissionBaseline snapshots unresolved durable requests before a new
// engine handle is published. A replayed request from a prior process must not
// acquire the replacement runtime's generation merely because its resolution
// record was never written.
func (d *Daemon) loadPermissionBaseline(subject string) ([]string, error) {
	db, err := store.Open()
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rec, err := d.runtimeRecordRaw(db, subject)
	if err != nil {
		return nil, err
	}
	open := runtimeevents.OpenPermissions(runtimeEventRecord(rec))
	ids := make([]string, 0, len(open))
	for _, pending := range open {
		ids = append(ids, pending.RequestID)
	}
	return ids, nil
}

func (d *Daemon) publishPermissionRuntime(subject string, create func() (any, error)) (any, string, error) {
	baseline, err := d.permissionBaseline(subject)
	if err != nil {
		return nil, "", err
	}
	return d.permissions.publishExcluding(subject, baseline, create)
}

func runtimeEventRecord(rec core.RuntimeRecord) runtimeevents.Record {
	return runtimeevents.Record{
		Runtime: rec.Runtime, Path: rec.Path, Permissions: rec.Permissions,
		Journal: rec.Journal, Structured: rec.Structured,
		PermissionBindings: rec.PermissionBindings,
	}
}

func (d *Daemon) bindRuntimeRecord(id string, rec core.RuntimeRecord) core.RuntimeRecord {
	rec.PermissionBindings = make(map[string]string)
	open := runtimeevents.OpenPermissions(runtimeEventRecord(rec))
	for _, pending := range open {
		generation, err := d.bindPermissionRequest(id, pending.RequestID)
		if err == nil && generation != "" {
			rec.PermissionBindings[pending.Occurrence] = generation
		}
	}
	return rec
}
