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
	generation, ok := d.permissions.generation(id)
	if !ok {
		rec.PermissionBindings = map[string]string{}
		return rec
	}
	open := runtimeevents.OpenPermissions(runtimeEventRecord(rec))
	rec.PermissionBindings = d.permissions.bindings(id, generation, open)
	return rec
}
