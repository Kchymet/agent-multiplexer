package selfupdate

import (
	"strings"
	"testing"
	"time"
)

func TestEnvDisabled(t *testing.T) {
	for v, want := range map[string]bool{"": false, "0": false, "false": false, "off": false, "1": true, "yes": true, "true": true, "anything": true} {
		if got := EnvDisabled(func(string) string { return v }); got != want {
			t.Errorf("EnvDisabled(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestAutoCheckGating(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	base := AutoInput{Running: "0.1.300", ConfigEnabled: true, Now: now}
	cases := []struct {
		name   string
		mutate func(*AutoInput)
		want   bool
		reason string
	}{
		{"first check", func(*AutoInput) {}, true, ""},
		{"env opt-out", func(in *AutoInput) { in.EnvDisabled = true }, false, DisableEnv},
		{"config opt-out", func(in *AutoInput) { in.ConfigEnabled = false }, false, "update.auto"},
		{"dev build", func(in *AutoInput) { in.Running = "0.1.300-dev+abc" }, false, "development build"},
		{"plain go build", func(in *AutoInput) { in.Running = "0.0.0-dev" }, false, "development build"},
		{"recent check", func(in *AutoInput) { in.LastCheck = now.Add(-time.Hour) }, false, "next check due"},
		{"check due", func(in *AutoInput) { in.LastCheck = now.Add(-DefaultCheckInterval) }, true, ""},
		{"custom interval", func(in *AutoInput) { in.LastCheck = now.Add(-2 * time.Hour); in.Interval = time.Hour }, true, ""},
		{"clock went back", func(in *AutoInput) { in.LastCheck = now.Add(48 * time.Hour) }, true, ""},
	}
	for _, c := range cases {
		in := base
		c.mutate(&in)
		got, why := AutoCheck(in)
		if got != c.want || (c.reason != "" && !strings.Contains(why, c.reason)) {
			t.Errorf("%s: AutoCheck = %v %q, want %v containing %q", c.name, got, why, c.want, c.reason)
		}
	}
}

func manifest(v string, protocol, min, max int) Manifest {
	return Manifest{Version: v, DaemonProtocol: protocol, DatabaseMinSchema: min, DatabaseMaxSchema: max}
}

func TestCompatibility(t *testing.T) {
	running := manifest("0.1.300", 2, 0, 3)
	cases := []struct {
		cand Manifest
		want Compat
	}{
		{manifest("0.1.301", 2, 0, 3), CompatHot},
		{manifest("0.1.301", 3, 0, 3), CompatRestart}, // protocol bump
		{manifest("0.1.301", 2, 0, 4), CompatRestart}, // schema migration
		{manifest("0.1.301", 2, 0, 2), CompatNever},   // cannot open schema 3
		{manifest("0.1.301", 2, 4, 5), CompatNever},   // cannot migrate from 3
		{manifest("0.1.301", 2, 3, 3), CompatHot},
	}
	for _, c := range cases {
		if got := Compatibility(c.cand, running); got != c.want {
			t.Errorf("Compatibility(%+v) = %v, want %v", c.cand, got, c.want)
		}
	}
}

func TestPlanInstall(t *testing.T) {
	running := manifest("0.1.300", 2, 0, 3)
	cases := []struct {
		name      string
		cand      Manifest
		installed string
		want      Action
	}{
		{"newer compatible", manifest("0.1.305", 2, 0, 3), "0.1.300", ActionSwap},
		{"same version", manifest("0.1.300", 2, 0, 3), "0.1.300", ActionNone},
		{"older release", manifest("0.1.299", 2, 0, 3), "0.1.300", ActionNone},
		{"already swapped in on disk", manifest("0.1.305", 2, 0, 3), "0.1.305", ActionNone},
		{"protocol change", manifest("0.1.305", 3, 0, 3), "0.1.300", ActionStage},
		{"schema change", manifest("0.1.305", 2, 0, 4), "0.1.300", ActionStage},
		{"incompatible database", manifest("0.1.305", 2, 0, 2), "0.1.300", ActionRefuse},
		// A checkout build installed by hand at the target is never replaced
		// automatically, even though the running daemon is a release.
		{"dev build on disk", manifest("0.1.305", 2, 0, 3), "0.1.290-dev+abc", ActionRefuse},
		{"unknown binary on disk", manifest("0.1.305", 2, 0, 3), "", ActionRefuse},
	}
	for _, c := range cases {
		if got, why := PlanInstall(c.cand, running, c.installed); got != c.want {
			t.Errorf("%s: PlanInstall = %v (%s), want %v", c.name, got, why, c.want)
		}
	}
}

func TestSafePointRequiresConsecutiveIdlePolls(t *testing.T) {
	var s SafePoint
	if s.Observe(0) {
		t.Fatal("one idle poll is not enough")
	}
	if !s.Observe(0) {
		t.Fatal("two consecutive idle polls are safe")
	}
	s = SafePoint{}
	s.Observe(0)
	if s.Observe(3) {
		t.Fatal("never safe while agents run")
	}
	if s.Observe(0) {
		t.Fatal("an agent resets the idle streak")
	}
	if !s.Observe(0) {
		t.Fatal("safe again after a fresh idle streak")
	}
	s = SafePoint{RequiredIdle: 1}
	if !s.Observe(0) {
		t.Fatal("RequiredIdle honored")
	}
}

func TestManifestValidate(t *testing.T) {
	if err := manifest("0.1.3", 2, 0, 3).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, m := range []Manifest{
		manifest("0.1.3-dev+abc", 2, 0, 3),
		manifest("", 2, 0, 3),
		manifest("0.1.3", 0, 0, 3),
		manifest("0.1.3", 2, 3, 1),
	} {
		if err := m.Validate(); err == nil {
			t.Errorf("Validate(%+v) accepted", m)
		}
	}
}

func TestStatePendingVersion(t *testing.T) {
	st := State{Installed: "0.1.305", Path: "/bin/amux"}
	if got := st.PendingVersion("0.1.300", "/bin/amux"); got != "0.1.305" {
		t.Fatalf("installed newer: %q", got)
	}
	if got := st.PendingVersion("0.1.305", "/bin/amux"); got != "" {
		t.Fatalf("already running it: %q", got)
	}
	if got := st.PendingVersion("0.1.300", "/other/amux"); got != "" {
		t.Fatalf("another binary's install: %q", got)
	}
	st.Staged = &Staged{Manifest: manifest("0.1.310", 3, 0, 3), Target: "/bin/amux"}
	if got := st.PendingVersion("0.1.300", "/bin/amux"); got != "0.1.310" {
		t.Fatalf("staged wins: %q", got)
	}
}

func TestStoreLockExcludesSecondHolder(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	unlock, err := s.Lock(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(150 * time.Millisecond); err != ErrLocked {
		t.Fatalf("second lock = %v, want ErrLocked", err)
	}
	unlock()
	unlock() // idempotent
	again, err := s.Lock(time.Second)
	if err != nil {
		t.Fatalf("relock after release: %v", err)
	}
	again()
}

func TestStoreRoundTrip(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	if st, err := s.Load(); err != nil || st.Latest != "" {
		t.Fatalf("missing state = %+v, %v", st, err)
	}
	if err := s.Update(func(st *State) { st.Latest = "0.1.9" }); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Load(); st.Latest != "0.1.9" {
		t.Fatalf("round trip = %+v", st)
	}
}
