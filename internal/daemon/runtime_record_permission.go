package daemon

import (
	"amux/internal/core"
	"amux/internal/runtimeevents"
)

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
		if err == nil {
			rec.PermissionBindings[pending.RequestID] = generation
		}
	}
	return rec
}
