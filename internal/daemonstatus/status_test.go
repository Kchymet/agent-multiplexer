package daemonstatus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReporterPublishesAndClears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	r := NewReporter(path)
	clock := time.UnixMilli(1_000_000)
	r.now = func() time.Time { return clock }

	r.Begin(OpStarting, PhaseReconciling)
	r.Count(3, 10)
	s, ok := Read(path)
	if !ok || s.Op != OpStarting || s.Phase != PhaseReconciling || s.Done != 3 || s.Total != 10 || s.PID != os.Getpid() || s.Started != clock.UnixMilli() {
		t.Fatalf("startup status = %+v (ok=%v)", s, ok)
	}

	r.Phase(PhaseListening)
	if s, _ := Read(path); s.Done != 0 || s.Total != 0 || s.Phase != PhaseListening {
		t.Fatalf("phase change kept stale counters: %+v", s)
	}

	r.Clear()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("cleared status still on disk: %v", err)
	}
	r.Count(1, 2) // late update after Clear must not resurrect the file
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("update after Clear rewrote the file: %v", err)
	}

	r.Begin(OpStopping, PhaseAgents)
	r.Agents([]Agent{{ID: "b", State: AgentBusy}, {ID: "a", Tab: 1, State: AgentTerminating}, {ID: "a", State: AgentKilling}})
	s, _ = Read(path)
	if len(s.Agents) != 3 || s.Agents[0].ID != "a" || s.Agents[0].Tab != 0 || s.Agents[1].Tab != 1 || s.Agents[2].ID != "b" {
		t.Fatalf("agents not sorted by id, tab: %+v", s.Agents)
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".daemon-status-*")); len(matches) != 0 {
		t.Fatalf("temporary files left behind: %v", matches)
	}
}

func TestNilReporterIsNoop(t *testing.T) {
	var r *Reporter
	r.Begin(OpStopping, PhaseDraining)
	r.Phase(PhaseAgents)
	r.Count(1, 2)
	r.Agents([]Agent{{ID: "x"}})
	r.Clear()
}

func TestReadRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status.json")
	if _, ok := Read(path); ok {
		t.Fatal("absent file read as a status")
	}
	for _, body := range []string{"{", "{}", `{"pid":1}`} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if s, ok := Read(path); ok {
			t.Fatalf("%q read as %+v", body, s)
		}
	}
}

func TestSummaryNamesWhatShutdownWaitsOn(t *testing.T) {
	now := time.UnixMilli(10_000_000)
	s := Status{Op: OpStopping, Phase: PhaseAgents, Agents: []Agent{
		{ID: "a1", Title: "fix login", State: AgentBusy, Until: now.Add(12 * time.Second).UnixMilli()},
		{ID: "a2", State: AgentTerminating, Until: now.Add(3 * time.Second).UnixMilli()},
		{ID: "a3", Tab: 2, Title: "api", State: AgentKilling},
		{ID: "a4", State: AgentStopped},
	}}
	got := Summary(s, now)
	for _, want := range []string{
		"waiting on 3 agents (1 busy, 1 terminating, 1 killing)",
		"SIGKILL in 3s", // the soonest escalation is a2's SIGTERM→SIGKILL
		"fix login (busy)", "a2 (terminating)", "api [tab 2] (killing)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "a4") {
		t.Errorf("summary %q lists a stopped agent", got)
	}

	s.Agents = []Agent{{ID: "a1", State: AgentBusy, Until: now.Add(20 * time.Second).UnixMilli()}}
	if got := Summary(s, now); !strings.Contains(got, "waiting on 1 agent (1 busy) — SIGTERM in 20s") {
		t.Errorf("single busy summary = %q", got)
	}
	s.Agents = []Agent{{ID: "a1", State: AgentStopped}}
	if got := Summary(s, now); got != "all 1 agents stopped" {
		t.Errorf("all stopped summary = %q", got)
	}
	if got := Summary(Status{Phase: PhaseReconciling, Done: 4, Total: 9}, now); got != "reconciling session mailboxes 4/9" {
		t.Errorf("reconcile summary = %q", got)
	}
	for _, phase := range []string{PhaseListening, PhaseDraining, PhaseCodex, PhaseReleasing} {
		if got := Summary(Status{Phase: phase}, now); got == "" || got == phase {
			t.Errorf("phase %s has no description: %q", phase, got)
		}
	}
}

func TestKeyIgnoresTickingDetail(t *testing.T) {
	base := Status{PID: 7, Op: OpStopping, Phase: PhaseAgents, Agents: []Agent{{ID: "a", State: AgentBusy, Until: 1}}}
	ticked := base
	ticked.Agents = []Agent{{ID: "a", State: AgentBusy, Until: 2}}
	ticked.Updated = 99
	if Key(base) != Key(ticked) {
		t.Fatal("deadline change altered the key")
	}
	moved := base
	moved.Agents = []Agent{{ID: "a", State: AgentTerminating, Until: 1}}
	if Key(base) == Key(moved) {
		t.Fatal("state change did not alter the key")
	}
	counting := Status{Op: OpStarting, Phase: PhaseReconciling, Done: 1, Total: 5}
	counted := counting
	counted.Done = 4
	if Key(counting) != Key(counted) {
		t.Fatal("mailbox counter altered the key")
	}
}
