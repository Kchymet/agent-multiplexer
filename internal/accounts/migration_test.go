package accounts

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"amux/internal/amuxcfg"
	"amux/internal/centralauth"
)

// v1Fixture is a version 1 store: host defaults, legacy host/shared/
// environment bindings, a central Codex binding with delivered evidence, and an
// unknown-history central binding. It holds no credential.
func v1Fixture(t *testing.T, centralReady bool) []byte {
	t.Helper()
	s, err := initial()
	if err != nil {
		t.Fatal(err)
	}
	s.Version = 1
	s.Migrated = nil
	s.Defaults["claude"] = "claude:host"
	s.Defaults["codex"] = "codex:host"
	if centralReady {
		p := s.Profiles["codex:central"]
		p.State = "ready"
		s.Profiles[p.ID] = p
		s.CredentialVersion["codex:central"] = 3
	}
	s.Bindings = map[string]Binding{
		"host-claude":    {ProfileID: "claude:host", Harness: "claude", Source: "host", Revision: 1, Origin: "machine-default"},
		"shared-claude":  {ProfileID: "claude:shared", Harness: "claude", Source: "shared", Revision: 2, Origin: "machine-default"},
		"env-claude":     {ProfileID: "claude:environment", Harness: "claude", Source: "environment", Revision: 1, Origin: "machine-default"},
		"host-codex":     {ProfileID: "codex:host", Harness: "codex", Source: "host", Revision: 1, Origin: "machine-default"},
		"central-codex":  {ProfileID: "codex:central", Harness: "codex", Source: "central", CredentialMode: "subscription", ExpectedAccount: "acct-old", DeliveredEpoch: 1, CredentialVersion: 3, Revision: 4, Origin: "explicit-rebind"},
		"unknown-claude": {ProfileID: "claude:central", Harness: "claude", Source: "central", Revision: 1, Origin: "machine-default"},
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	// Keep a v1-only field so the backup is provably the original bytes.
	return bytes.Replace(b, []byte(`"revision":1,"origin":"machine-default"}`), []byte(`"revision":1,"origin":"machine-default","credentialUnknown":true}`), 1)
}

func writeStore(t *testing.T, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(Path()), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestV1MigrationMovesEveryBindingToCentralOnce(t *testing.T) {
	isolated(t)
	original := v1Fixture(t, false)
	writeStore(t, original)

	// Reading migrates in memory only; the file is rewritten under the lock.
	s, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(Path()); !bytes.Equal(b, original) {
		t.Fatal("read rewrote the store")
	}
	if s.Version != StoreVersion || s.Defaults["claude"] != "claude:central" || s.Defaults["codex"] != "codex:central" {
		t.Fatalf("defaults not migrated: %+v", s.Defaults)
	}
	// The migration is pending (reported, not complete) until central login.
	if s.MigrationPending("claude") != "host" || s.MigrationPending("codex") != "host" {
		t.Fatalf("pending migration not reported: %+v", s.Migrated)
	}
	if snap := s.Snapshot(); snap.MigrationPending["claude"] != "host" || snap.Defaults["claude"] != "claude:central" {
		t.Fatalf("snapshot: %+v", snap)
	}
	for id, from := range map[string]string{"host-claude": "host", "shared-claude": "shared", "env-claude": "environment", "host-codex": "host", "central-codex": "", "unknown-claude": ""} {
		b := s.Bindings[id]
		if b.ProfileID != b.Harness+":central" || b.Source != "central" || b.MigratedFrom != from || b.Origin != "migrated" {
			t.Fatalf("%s not migrated: %+v", id, b)
		}
		if got := Mode(id, b.Harness); got != "central" {
			t.Fatalf("%s launches on %q", id, got)
		}
	}
	if c := s.Bindings["central-codex"]; c.ExpectedAccount != "acct-old" || c.DeliveredEpoch != 1 || c.CredentialVersion != 3 {
		t.Fatalf("delivered evidence lost: %+v", c)
	}

	if err := Update(func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(filepath.Dir(Path()), "accounts.v1.json")); err != nil || !bytes.Equal(b, original) {
		t.Fatal("original v1 metadata not preserved")
	}
	first, _ := Read()
	if err := Update(func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	second, _ := Read()
	if !reflect.DeepEqual(first, second) {
		t.Fatal("migration is not idempotent")
	}
	// New workloads during the pending migration are plain central bindings.
	if err := Bind("fresh", "codex"); err != nil {
		t.Fatal(err)
	}
	s, _ = Read()
	if b := s.Bindings["fresh"]; b.MigratedFrom != "" || b.Source != "central" {
		t.Fatalf("new workload seeded from legacy: %+v", b)
	}
	// The first central login completes the migration; nothing restores it.
	if err := Update(func(s *State) error {
		p := s.Profiles["claude:central"]
		p.State = "ready"
		s.Profiles[p.ID] = p
		s.CredentialVersion[p.ID] = 7
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s, _ = Read()
	if s.MigrationPending("claude") != "" || s.MigrationPending("codex") != "host" || s.Bindings["host-claude"].MigratedFrom != "host" {
		t.Fatalf("completion wrong: %+v", s.Migrated)
	}
}

func TestV1MigrationWithConfiguredCentralIsComplete(t *testing.T) {
	isolated(t)
	writeStore(t, v1Fixture(t, true))
	s, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	if s.MigrationPending("codex") != "" || Mode("host-codex", "codex") != "central" {
		t.Fatalf("configured central not used: %+v", s.Bindings["host-codex"])
	}
}

func TestLegacyConfigSeedsOnlyPendingEvidence(t *testing.T) {
	isolated(t)
	if err := amuxcfg.SetAuthMode("claude", amuxcfg.AuthEnvironment); err != nil {
		t.Fatal(err)
	}
	if err := Bind("fresh", "claude"); err != nil {
		t.Fatal(err)
	}
	s, _ := Read()
	if s.Defaults["claude"] != "claude:central" || Mode("fresh", "claude") != "central" || s.MigrationPending("claude") != "environment" {
		t.Fatalf("fresh install: %+v %+v", s.Defaults, s.Migrated)
	}
	// Unsetting the old config never resurrects a legacy source.
	if err := amuxcfg.SetAuthMode("claude", ""); err != nil {
		t.Fatal(err)
	}
	if Mode("fresh", "claude") != "central" {
		t.Fatal("config unset changed routing")
	}
}

func TestStoreFromNewerOrMalformedVersionIsRefusedAndKept(t *testing.T) {
	for _, body := range []string{`{"version":3,"profiles":{},"bindings":{},"defaults":{},"credentialVersion":{}}`, `{"version":`} {
		isolated(t)
		writeStore(t, []byte(body))
		if err := Bind("w", "codex"); err == nil {
			t.Fatalf("%s accepted", body)
		}
		if _, err := BeginLaunch("w", "codex"); err == nil {
			t.Fatalf("%s allocated a launch", body)
		}
		if b, _ := os.ReadFile(Path()); string(b) != body {
			t.Fatal("refused metadata was rewritten")
		}
	}
}

// Codex: same-account refresh within a process continues; another account is
// refused for that process but pinned by the next launch; a late request from
// the superseded process cannot re-pin the binding.
func TestCodexAccountReplacementAndOldRuntimeRace(t *testing.T) {
	isolated(t)
	if err := Update(func(s *State) error {
		p := s.Profiles["codex:central"]
		p.State = "ready"
		s.Profiles[p.ID] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	old, _ := BeginLaunch("w", "codex")
	b, p, _ := Delivery("w", "codex")
	if err := Accept("w", b.Revision, p.DeliveryEpoch, "acct-a", 1, old); err != nil {
		t.Fatal(err)
	}
	if err := Accept("w", b.Revision, p.DeliveryEpoch, "acct-a", 2, old); err != nil {
		t.Fatal("same-account refresh refused", err)
	}
	if err := Accept("w", b.Revision, p.DeliveryEpoch, "acct-b", 3, old); !errors.Is(err, ErrSuperseded) {
		t.Fatal("running process moved accounts", err)
	}
	next, _ := BeginLaunch("w", "codex")
	if err := Accept("w", b.Revision, p.DeliveryEpoch, "acct-b", 3, next); err != nil {
		t.Fatal("next launch refused the replacement account", err)
	}
	if err := Accept("w", b.Revision, p.DeliveryEpoch, "acct-a", 4, old); !errors.Is(err, ErrSuperseded) {
		t.Fatal("old process re-pinned the binding", err)
	}
	// A launch allocated but not yet delivered does not disturb the current one.
	if _, err := BeginLaunch("w", "codex"); err != nil {
		t.Fatal(err)
	}
	if err := Accept("w", b.Revision, p.DeliveryEpoch, "acct-b", 4, next); err != nil {
		t.Fatal("current process refresh refused after a pending launch", err)
	}
	s, _ := Read()
	if got := s.Bindings["w"]; got.ExpectedAccount != "acct-b" || got.Launch != next.Seq || len(got.History) != 1 || got.History[0].Account != "acct-a" {
		t.Fatalf("binding: %+v", got)
	}
}

func claudeReady(t *testing.T, token string) centralauth.Credential {
	t.Helper()
	secret, _ := centralauth.NewSecret(token)
	c, err := centralauth.ClaudeStore().Save(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := Update(func(s *State) error {
		p := s.Profiles["claude:central"]
		p.State = "ready"
		p.DeliveryEpoch++
		s.Profiles[p.ID] = p
		s.CredentialVersion[p.ID] = c.Generation
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

// Claude: a launch's repeated query (timeout, lost response, abandoned
// duplicate) receives the same snapshot; after a replacement login it is
// refused, and only a new daemon-allocated launch receives the replacement.
func TestClaudeLaunchIsIdempotentAndNeverAdoptsReplacement(t *testing.T) {
	isolated(t)
	first := claudeReady(t, "FIRST-SYNTHETIC")
	launch, err := BeginLaunch("w", "claude")
	if err != nil {
		t.Fatal(err)
	}
	// Resolves before the first delivery (pane reattach) reuse the pending launch.
	if again, _ := BeginLaunch("w", "claude"); again != launch {
		t.Fatal("pending launch displaced by a reattach")
	}
	deliver := func(l Launch, version uint64) error {
		b, p, err := Delivery("w", "claude")
		if err != nil {
			return err
		}
		return Accept("w", b.Revision, p.DeliveryEpoch, "", version, l)
	}
	if err := deliver(launch, first.Generation); err != nil {
		t.Fatal(err)
	}
	// The response was lost; the launcher retries the same launch.
	if err := deliver(launch, first.Generation); err != nil {
		t.Fatal("retry after a lost response refused", err)
	}
	second := claudeReady(t, "REPLACEMENT-SYNTHETIC")
	if err := deliver(launch, second.Generation); !errors.Is(err, ErrSuperseded) {
		t.Fatal("same launch adopted the replacement", err)
	}
	for name, forged := range map[string]Launch{
		"no launch":      {},
		"next seq":       {Seq: launch.Seq + 1, Nonce: launch.Nonce},
		"wrong nonce":    {Seq: launch.Seq, Nonce: "0000"},
		"far future seq": {Seq: launch.Seq + 100},
	} {
		if err := deliver(forged, second.Generation); err == nil {
			t.Fatalf("%s request received the replacement", name)
		}
	}
	next, _ := BeginLaunch("w", "claude")
	if next.Seq <= launch.Seq || next.Nonce == launch.Nonce {
		t.Fatalf("new launch reused the pinned one: %+v %+v", launch, next)
	}
	// A guessed nonce for the new pending launch is still refused.
	if err := deliver(Launch{Seq: next.Seq, Nonce: launch.Nonce}, second.Generation); err == nil {
		t.Fatal("old nonce claimed the new launch")
	}
	if err := deliver(next, second.Generation); err != nil {
		t.Fatal("new launch refused the replacement", err)
	}
	if err := deliver(launch, first.Generation); !errors.Is(err, ErrSuperseded) {
		t.Fatal("old launch accepted after a new one pinned", err)
	}
}

// Logout between allocation and first delivery delivers nothing; the same
// pending launch delivers after the next login.
func TestClaudeLogoutBeforeFirstDelivery(t *testing.T) {
	isolated(t)
	first := claudeReady(t, "FIRST-SYNTHETIC")
	launch, _ := BeginLaunch("w", "claude")
	b, p, _ := Delivery("w", "claude")
	if err := Update(func(s *State) error {
		q := s.Profiles["claude:central"]
		q.State = "logged-out"
		q.DeliveryEpoch++
		s.Profiles[q.ID] = q
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := Accept("w", b.Revision, p.DeliveryEpoch, "", first.Generation, launch); !errors.Is(err, ErrStale) {
		t.Fatal("logged-out credential delivered", err)
	}
	if _, _, err := Delivery("w", "claude"); !errors.Is(err, ErrBlocked) {
		t.Fatal("delivery open while logged out", err)
	}
	next := claudeReady(t, "RELOGIN-SYNTHETIC")
	b, p, _ = Delivery("w", "claude")
	if err := Accept("w", b.Revision, p.DeliveryEpoch, "", next.Generation, launch); err != nil {
		t.Fatal("pending launch refused after relogin", err)
	}
}
