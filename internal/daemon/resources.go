package daemon

import (
	"context"
	"log"
	"os"
	"runtime"

	"amux/internal/core"
	"amux/internal/procstat"
)

// resourceIntervalEnv configures the resource sampler's period: a duration
// ("2s"), seconds ("5"), or "off". Unset means procstat.DefaultInterval.
const resourceIntervalEnv = "AMUX_RESOURCE_INTERVAL"

// startResourceSampler starts the background process sampler that backs
// core.QueryResources. The query only ever reads the cached snapshot, so
// sampling cost never lands on an RPC.
func (d *Daemon) startResourceSampler(ctx context.Context, start func(func())) {
	if runtime.GOOS != "linux" {
		d.setResourcesOff("resource sampling needs Linux procfs")
		return
	}
	interval, err := procstat.ParseInterval(os.Getenv(resourceIntervalEnv))
	if err != nil {
		log.Printf("resources: %s=%q: %v; using %s", resourceIntervalEnv, os.Getenv(resourceIntervalEnv), err, procstat.DefaultInterval)
		interval = procstat.DefaultInterval
	}
	if interval == 0 {
		d.setResourcesOff("resource sampling disabled (" + resourceIntervalEnv + "=off)")
		return
	}
	s := procstat.New(procstat.Config{
		Interval:      interval,
		SessionsDir:   core.SessionsDir(),
		WorkspacesDir: core.WorkspacesDir(),
		Owners:        d.resourceOwners,
	})
	d.resourcesMu.Lock()
	d.resources = s
	d.resourcesMu.Unlock()
	start(func() { s.Run(ctx) })
}

func (d *Daemon) setResourcesOff(reason string) {
	d.resourcesMu.Lock()
	d.resourcesOff = reason
	d.resourcesMu.Unlock()
}

// resourceOwners reports each polled session's stored directory and owner, so
// the sampler attributes by stored ownership rather than by path shape (a
// moved member's sandbox stays under its old workgroup's directory; a legacy
// sub-session's directory is not named after its id). The sampler calls it on
// its own cadence; it copies from the poll loop's cached rows and touches
// neither the store nor any RPC path.
func (d *Daemon) resourceOwners() []procstat.Owner {
	d.mu.RLock()
	defer d.mu.RUnlock()
	owners := make([]procstat.Owner, 0, len(d.sessions))
	for _, s := range d.sessions {
		if s.Cwd == "" || s.ID == "" {
			continue
		}
		wg := s.RootID
		if wg == "" {
			wg = s.ID // a root, the console or a repo home is its own group
		}
		owners = append(owners, procstat.Owner{Dir: s.Cwd, Session: s.ID, Workgroup: wg})
	}
	return owners
}

// resourceSnapshot is the QueryResources read model: the sampler's cached
// latest sample.
func (d *Daemon) resourceSnapshot() procstat.Snapshot {
	d.resourcesMu.Lock()
	s, off := d.resources, d.resourcesOff
	d.resourcesMu.Unlock()
	if s != nil {
		return s.Latest()
	}
	if runtime.GOOS != "linux" {
		if off == "" {
			off = "resource sampling needs Linux procfs"
		}
		return procstat.Unsupported(off)
	}
	if off == "" {
		off = "resource sampler not running"
	}
	return procstat.Disabled(off)
}
