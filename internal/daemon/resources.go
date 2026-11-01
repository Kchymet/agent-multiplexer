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
		d.resourcesOff = "resource sampling needs Linux procfs"
		return
	}
	interval, err := procstat.ParseInterval(os.Getenv(resourceIntervalEnv))
	if err != nil {
		log.Printf("resources: %s=%q: %v; using %s", resourceIntervalEnv, os.Getenv(resourceIntervalEnv), err, procstat.DefaultInterval)
		interval = procstat.DefaultInterval
	}
	if interval == 0 {
		d.resourcesOff = "resource sampling disabled (" + resourceIntervalEnv + "=off)"
		return
	}
	s := procstat.New(procstat.Config{
		Interval:      interval,
		SessionsDir:   core.SessionsDir(),
		WorkspacesDir: core.WorkspacesDir(),
	})
	d.resourcesMu.Lock()
	d.resources = s
	d.resourcesMu.Unlock()
	start(func() { s.Run(ctx) })
}

// resourceSnapshot is the QueryResources read model: the sampler's cached
// latest sample.
func (d *Daemon) resourceSnapshot() procstat.Snapshot {
	d.resourcesMu.Lock()
	s, off := d.resources, d.resourcesOff
	d.resourcesMu.Unlock()
	if s == nil && runtime.GOOS != "linux" {
		return procstat.Unsupported(off)
	}
	if s == nil {
		if off == "" {
			off = "resource sampler not running"
		}
		return procstat.Disabled(off)
	}
	return s.Latest()
}
