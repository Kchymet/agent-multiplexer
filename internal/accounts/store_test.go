package accounts

import (
	"amux/internal/amuxcfg"
	"amux/internal/centralauth"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func isolated(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
}
func TestDefaultsAndBindingsPersistIndependently(t *testing.T) {
	isolated(t)
	if e := Bind("worker", "claude"); e != nil {
		t.Fatal(e)
	}
	s, _ := Read()
	machine := s.MachineID
	if e := Update(func(s *State) error { s.Defaults["claude"] = ID("claude", "host"); return nil }); e != nil {
		t.Fatal(e)
	}
	if e := Bind("next", "claude"); e != nil {
		t.Fatal(e)
	}
	s, _ = Read()
	if s.MachineID != machine || Mode("worker", "claude") != "central" || Mode("next", "claude") != "host" {
		t.Fatalf("bindings changed: %+v", s.Bindings)
	}
	if e := amuxcfg.SetAuthMode("claude", "environment"); e != nil {
		t.Fatal(e)
	}
	if Mode("worker", "claude") != "central" {
		t.Fatal("global config changed workload")
	}
	fi, e := os.Stat(Path())
	if e != nil || fi.Mode().Perm() != 0600 {
		t.Fatal("metadata must be private")
	}
}
func TestDeliveryFencesAccountLogoutAndBindingRevision(t *testing.T) {
	isolated(t)
	Bind("w", "codex")
	b, p, e := Delivery("w", "codex")
	if e != nil {
		t.Fatal(e)
	}
	if e = Accept("w", b.Revision, p.DeliveryEpoch, "account-a", 1); e != nil {
		t.Fatal(e)
	}
	if e = Accept("w", b.Revision, p.DeliveryEpoch, "account-b", 2); !errors.Is(e, ErrBlocked) {
		t.Fatal("account switched", e)
	}
	if e = Accept("w", b.Revision, p.DeliveryEpoch, "account-a", 2); e != nil {
		t.Fatal("same-account refresh rejected", e)
	}
	Update(func(s *State) error {
		p := s.Profiles[b.ProfileID]
		p.DeliveryEpoch++
		p.State = "logged-out"
		s.Profiles[p.ID] = p
		return nil
	})
	if e = Accept("w", b.Revision, p.DeliveryEpoch, "account-a", 3); !errors.Is(e, ErrStale) {
		t.Fatal("late refresh survived logout", e)
	}
	if _, _, e = Delivery("w", "codex"); !errors.Is(e, ErrBlocked) {
		t.Fatal(e)
	}
}
func TestOpaqueClaudeReplacementRequiresExplicitRebind(t *testing.T) {
	isolated(t)
	Bind("w", "claude")
	b, p, _ := Delivery("w", "claude")
	if e := Accept("w", b.Revision, p.DeliveryEpoch, "", 1); e != nil {
		t.Fatal(e)
	}
	if e := Accept("w", b.Revision, p.DeliveryEpoch, "", 2); !errors.Is(e, ErrBlocked) {
		t.Fatal("opaque token switched", e)
	}
	if e := Accept("w", b.Revision+1, p.DeliveryEpoch, "", 1); !errors.Is(e, ErrBlocked) {
		t.Fatal("binding revision ignored")
	}
}
func TestCorruptMetadataFailsClosed(t *testing.T) {
	isolated(t)
	os.MkdirAll(filepath.Dir(Path()), 0700)
	os.WriteFile(Path(), []byte(`{"version":99}`), 0600)
	if e := Bind("w", "codex"); e == nil {
		t.Fatal("corrupt store accepted")
	}
}
func TestClaudeObservationProvenanceAndEpoch(t *testing.T) {
	isolated(t)
	Bind("w", "claude")
	b, p, _ := Delivery("w", "claude")
	Accept("w", b.Revision, p.DeliveryEpoch, "", 1)
	raw := []byte(`{"five_hour":{"used_percentage":42,"resets_at":1892900800}}`)
	if e := ObserveClaude("w", "runtime-1", raw); e != nil {
		t.Fatal(e)
	}
	s, _ := Read()
	q := s.Profiles[p.ID].Limits
	if q.State != "known" || q.SessionID != "w" || q.RuntimeGeneration != "runtime-1" || q.DeliveryEpoch != p.DeliveryEpoch {
		t.Fatalf("provenance missing: %+v", q)
	}
	Update(func(s *State) error {
		p := s.Profiles[b.ProfileID]
		p.DeliveryEpoch++
		s.Profiles[p.ID] = p
		return nil
	})
	if e := ObserveClaude("w", "runtime-1", raw); !errors.Is(e, ErrStale) {
		t.Fatal("stale runtime quota accepted", e)
	}
}

func TestMigrationCapturesPreexistingClaudeCredential(t *testing.T) {
	isolated(t)
	// An older daemon created a workload before machine profiles existed.
	if err := Bind("legacy", "claude"); err != nil {
		t.Fatal(err)
	}
	token, _ := centralauth.NewSecret("LEGACY-OPAQUE-TOKEN")
	old, err := centralauth.ClaudeStore().Save(token, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = Bind("legacy", "claude"); err != nil {
		t.Fatal(err)
	}
	s, _ := Read()
	if s.Bindings["legacy"].CredentialVersion != old.Generation {
		t.Fatal("legacy credential not pinned")
	}
	token, _ = centralauth.NewSecret("REPLACEMENT-OPAQUE-TOKEN")
	next, err := centralauth.ClaudeStore().Save(token, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, p, err := Delivery("legacy", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if err = Accept("legacy", b.Revision, p.DeliveryEpoch, "", next.Generation); err == nil {
		t.Fatal("migration silently switched opaque accounts")
	}
}

func TestUnreadableClaudeCredentialRequiresExplicitRebind(t *testing.T) {
	isolated(t)
	dir := centralauth.ClaudeStore().Dir
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credential.json"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Bind("legacy", "claude"); err != nil {
		t.Fatal(err)
	}
	before, _ := Read()
	token, _ := centralauth.NewSecret("REPLACEMENT-SYNTHETIC-TOKEN")
	next, err := centralauth.ClaudeStore().Save(token, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = Bind("legacy", "claude"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = Delivery("legacy", "claude"); !errors.Is(err, ErrBlocked) {
		t.Fatal("unknown old account adopted replacement", err)
	}
	b := before.Bindings["legacy"]
	if err = Accept("legacy", b.Revision, before.Profiles[b.ProfileID].DeliveryEpoch, "", next.Generation); !errors.Is(err, ErrBlocked) {
		t.Fatal("direct acceptance bypassed unknown binding", err)
	}
	// Fresh workloads can use the now-readable credential; only the old binding is fenced.
	if err = Bind("fresh", "claude"); err != nil {
		t.Fatal(err)
	}
	fresh, profile, err := Delivery("fresh", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if err = Accept("fresh", fresh.Revision, profile.DeliveryEpoch, "", next.Generation); err != nil {
		t.Fatal(err)
	}
	// Explicit rebind is the sole recovery for an unknown old account.
	if err = Update(func(s *State) error {
		fresh.Revision = b.Revision + 1
		fresh.Origin = "explicit-rebind"
		s.Bindings["legacy"] = fresh
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = Delivery("legacy", "claude"); err != nil {
		t.Fatal(err)
	}
}

func TestNewWorkloadBeforeFirstClaudeLoginCanAcceptFirstCredential(t *testing.T) {
	isolated(t)
	if err := Bind("fresh", "claude"); err != nil {
		t.Fatal(err)
	}
	token, _ := centralauth.NewSecret("FIRST-SYNTHETIC-TOKEN")
	first, err := centralauth.ClaudeStore().Save(token, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, p, err := Delivery("fresh", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if err = Accept("fresh", b.Revision, p.DeliveryEpoch, "", first.Generation); err != nil {
		t.Fatal(err)
	}
}
