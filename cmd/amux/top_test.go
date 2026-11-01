package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"amux/internal/core"
	"amux/internal/procstat"
)

func topFixture() procstat.Snapshot {
	return procstat.Snapshot{
		SampledAt: time.Unix(100, 0).UnixMilli(), IntervalMS: 3000, WindowMS: 3000, CPUs: 32, MemTotalBytes: 62 << 30,
		Self: procstat.SelfUsage{
			Usage: procstat.Usage{CPUPercent: 175, RSSBytes: 400 << 20, Processes: 4},
			Procs: []procstat.Process{
				{PID: 10, Role: procstat.RoleDaemon, CPUPercent: 150},
				{PID: 11, Role: procstat.RoleTUI, CPUPercent: 20},
				{PID: 12, Role: procstat.RoleTUI, CPUPercent: 5},
				{PID: 13, Role: procstat.RoleChild, Comm: "git"},
			},
			Sampler: procstat.SamplerCost{CPUPercent: 0.4},
		},
		Sessions: []procstat.SessionUsage{
			{ID: "374179", WorkgroupID: "771951", Usage: procstat.Usage{CPUPercent: 110, RSSBytes: 5 << 30, Processes: 12},
				Top: []procstat.Process{{PID: 202, Comm: "qemu-system-x86", Cmd: "qemu-system-x86_64 -avd Pixel", CPUPercent: 108, RSSBytes: 5 << 30}}},
			{ID: "aaaaaa", WorkgroupID: "771951", Usage: procstat.Usage{CPUPercent: 1, RSSBytes: 8 << 30, Processes: 3}},
			{ID: "771951", WorkgroupID: "771951", Usage: procstat.Usage{CPUPercent: 2, RSSBytes: 1 << 20, Processes: 1}},
		},
		Workgroups: []procstat.WorkgroupUsage{{ID: "771951", Usage: procstat.Usage{CPUPercent: 113, RSSBytes: 13 << 30, Processes: 16}, Sessions: 3}},
	}
}

func TestRenderTop(t *testing.T) {
	var buf bytes.Buffer
	titles := map[string]string{"771951": "android", "374179": "emulator agent"}
	renderTop(&buf, topFixture(), titles, topOptions{procs: 3, now: time.Unix(102, 0)})
	out := buf.String()
	for _, want := range []string{
		"daemon 150.0% · tui 25.0% (×2)", "sampler 0.40%", "32 CPUs", "sampled 2s ago",
		"771951           android", "  374179         emulator agent", "qemu-system-x86_64 -avd Pixel",
		"(workgroup / coordinator)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// CPU order: 374179 before the coordinator before aaaaaa.
	if i, j := strings.Index(out, "374179"), strings.Index(out, "aaaaaa"); i > j {
		t.Errorf("cpu sort wrong:\n%s", out)
	}
	buf.Reset()
	renderTop(&buf, topFixture(), titles, topOptions{byMem: true, procs: 0, now: time.Unix(102, 0)})
	out = buf.String()
	if i, j := strings.Index(out, "aaaaaa"), strings.Index(out, "374179"); i > j {
		t.Errorf("mem sort wrong:\n%s", out)
	}
	if strings.Contains(out, "qemu") {
		t.Errorf("-n 0 should hide processes:\n%s", out)
	}
}

func TestStatusWithResources(t *testing.T) {
	snap := core.Snapshot{Type: "snapshot", Sessions: []core.Session{
		{ID: "771951", IsRoot: true}, {ID: "374179", RootID: "771951"}, {ID: "idle00", RootID: "771951"},
	}}
	res := topFixture()
	b, err := json.Marshal(statusWithResources(snap, &res))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Sessions  []map[string]json.RawMessage `json:"sessions"`
		Resources *struct {
			Self struct {
				CPUPercent float64 `json:"cpuPercent"`
			} `json:"self"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Resources == nil || got.Resources.Self.CPUPercent != 175 {
		t.Fatalf("resources = %s", b)
	}
	if _, ok := got.Sessions[0]["workgroupResources"]; !ok {
		t.Errorf("root lacks workgroupResources: %s", b)
	}
	if !strings.Contains(string(got.Sessions[1]["resources"]), `"cpuPercent":110`) {
		t.Errorf("agent resources = %s", got.Sessions[1]["resources"])
	}
	if _, ok := got.Sessions[2]["resources"]; ok {
		t.Errorf("idle session got resources: %s", b)
	}
	if _, ok := got.Sessions[1]["title"]; !ok {
		t.Errorf("session fields not embedded: %s", b)
	}
	// Without a sample the shape is the plain snapshot.
	b, _ = json.Marshal(statusWithResources(snap, nil))
	if strings.Contains(string(b), "resources") {
		t.Errorf("unexpected resources: %s", b)
	}
}
