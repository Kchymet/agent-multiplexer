package daemon

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"amux/internal/access"
	"amux/internal/core"
	"amux/internal/procstat"
)

// QueryResources answers from the cache whether or not a sampler is running,
// and never samples on the query path.
func TestResourcesQuery(t *testing.T) {
	d := New("", nil, time.Hour)
	got, err := d.readModel(core.Action{Query: core.QueryResources})
	if err != nil {
		t.Fatal(err)
	}
	if snap := got.(procstat.Snapshot); snap.Error == "" || snap.SampledAt != 0 {
		t.Fatalf("no sampler: %+v", snap)
	}

	if runtime.GOOS != "linux" {
		t.Skip("sampler needs procfs")
	}
	t.Setenv(resourceIntervalEnv, "1s")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	d.startResourceSampler(ctx, func(f func()) { go func() { defer close(done); f() }() })
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap := d.resourceSnapshot()
		if snap.SampledAt != 0 {
			if snap.Total.Processes == 0 || snap.IntervalMS != 1000 {
				t.Fatalf("sample = %+v", snap)
			}
			b, _ := json.Marshal(snap)
			if !strings.Contains(string(b), `"sampler":{`) {
				t.Fatalf("json = %s", b)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no sample: %+v", snap)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestResourcesDisabled(t *testing.T) {
	t.Setenv(resourceIntervalEnv, "off")
	d := New("", nil, time.Hour)
	d.startResourceSampler(context.Background(), func(func()) { t.Fatal("started a disabled sampler") })
	if snap := d.resourceSnapshot(); !strings.Contains(snap.Error, "disabled") && runtime.GOOS == "linux" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

// Restricted (agent) principals cannot read host-wide process data.
func TestResourcesQueryNotRestricted(t *testing.T) {
	if err := validateSessionQuery(access.Request{Route: access.RouteQuery, Verb: core.QueryResources}); err == nil {
		t.Fatal("restricted sessions accepted the host-only resources query")
	}
}

func TestResourcesUnsupportedFlag(t *testing.T) {
	d := New("", nil, time.Hour)
	d.startResourceSampler(context.Background(), func(f func()) {})
	snap := d.resourceSnapshot()
	if runtime.GOOS != "linux" && (!snap.Unsupported || snap.Sessions == nil) {
		t.Fatalf("non-linux snapshot = %+v", snap)
	}
	if runtime.GOOS == "linux" && snap.Unsupported {
		t.Fatalf("linux snapshot flagged unsupported: %+v", snap)
	}
}
