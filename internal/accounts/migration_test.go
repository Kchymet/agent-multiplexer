package accounts

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"amux/internal/amuxcfg"
)

// v1Fixture is a version 1 store: a host Claude default, legacy host/shared/
// environment bindings, a central Codex binding with delivered evidence, and an
// unknown-history central binding. It holds no credential.
func v1Fixture(t *testing.T, centralReady bool) []byte {
	t.Helper()
	s, err := initial()
	if err != nil {
		t.Fatal(err)
	}
	s.Version = 1
	s.LegacyFallback = nil
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
	b = bytes.Replace(b, []byte(`"revision":1,"origin":"machine-default"}`), []byte(`"revision":1,"origin":"machine-default","credentialUnknown":true}`), 1)
	return b
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

func usableLegacy(t *testing.T, usable bool) {
	t.Helper()
	prior := LegacyUsable
	LegacyUsable = map[string]func(string) bool{
		"claude": func(string) bool { return usable },
		"codex":  func(string) bool { return usable },
	}
	t.Cleanup(func() { LegacyUsable = prior })
}

func TestV1MigrationFollowsCentralAndKeepsUsableLegacyLogin(t *testing.T) {
	isolated(t)
	usableLegacy(t, true)
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
	if s.LegacyFallback["claude"] != "host" || s.LegacyFallback["codex"] != "host" {
		t.Fatalf("legacy default not kept as transition fallback: %+v", s.LegacyFallback)
	}
	for id, want := range map[string]string{"host-claude": "host", "shared-claude": "shared", "env-claude": "environment", "host-codex": "host", "central-codex": "", "unknown-claude": ""} {
		b := s.Bindings[id]
		if b.ProfileID != b.Harness+":central" || b.Source != "central" || b.LegacySource != want {
			t.Fatalf("%s not migrated: %+v", id, b)
		}
		if got := Mode(id, b.Harness); (want == "" && got != "central") || (want != "" && got != want) {
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

	// The first central login retires every transition fallback; the next
	// launch of every workload, including the previously legacy ones, is central.
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
	if s.LegacyFallback["claude"] != "" || s.LegacyFallback["codex"] != "host" {
		t.Fatalf("fallback retirement wrong: %+v", s.LegacyFallback)
	}
	for _, id := range []string{"host-claude", "shared-claude", "env-claude"} {
		if s.Bindings[id].LegacySource != "" || Mode(id, "claude") != "central" {
			t.Fatalf("%s still on legacy login after central login", id)
		}
	}
	if Mode("host-codex", "codex") != "host" {
		t.Fatal("Codex fallback retired by a Claude login")
	}
	// A later logout never restores the retired legacy login.
	if err := Update(func(s *State) error {
		p := s.Profiles["claude:central"]
		p.State = "logged-out"
		s.Profiles[p.ID] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if Mode("host-claude", "claude") != "central" {
		t.Fatal("logout restored a legacy login")
	}
}

func TestV1MigrationWithConfiguredCentralSkipsLegacy(t *testing.T) {
	isolated(t)
	usableLegacy(t, true)
	writeStore(t, v1Fixture(t, true))
	s, err := Read()
	if err != nil {
		t.Fatal(err)
	}
	if s.LegacyFallback["codex"] != "" || s.Bindings["host-codex"].LegacySource != "" || Mode("host-codex", "codex") != "central" {
		t.Fatalf("configured central did not win: %+v", s.Bindings["host-codex"])
	}
}

func TestUnusableLegacyLoginUsesCentralLoginFlow(t *testing.T) {
	isolated(t)
	usableLegacy(t, false)
	writeStore(t, v1Fixture(t, false))
	if got := Mode("host-codex", "codex"); got != "central" {
		t.Fatalf("unusable legacy login selected: %q", got)
	}
	if got := Mode("shared-claude", "claude"); got != "central" {
		t.Fatalf("unusable shared login selected: %q", got)
	}
}

func TestNewWorkloadDuringTransitionInheritsFallback(t *testing.T) {
	isolated(t)
	usableLegacy(t, true)
	if err := amuxcfg.SetAuthMode("claude", amuxcfg.AuthEnvironment); err != nil {
		t.Fatal(err)
	}
	if err := Bind("fresh", "claude"); err != nil {
		t.Fatal(err)
	}
	s, _ := Read()
	if s.Defaults["claude"] != "claude:central" || s.Bindings["fresh"].LegacySource != "environment" || Mode("fresh", "claude") != "environment" {
		t.Fatalf("transition fallback: %+v %+v", s.Defaults, s.Bindings["fresh"])
	}
	if s.Snapshot().Defaults["claude"] != "claude:environment" {
		t.Fatal("snapshot hides the login launches still use")
	}
}

func TestStoreFromNewerOrMalformedVersionIsRefusedAndKept(t *testing.T) {
	for _, body := range []string{`{"version":3,"profiles":{},"bindings":{},"defaults":{},"credentialVersion":{}}`, `{"version":`} {
		isolated(t)
		writeStore(t, []byte(body))
		if err := Bind("w", "codex"); err == nil {
			t.Fatalf("%s accepted", body)
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
	if err := Accept("w", b.Revision, p.DeliveryEpoch, "acct-b", 3, old); !errors.Is(err, ErrBlocked) {
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
	if got := s.Bindings["w"]; got.ExpectedAccount != "acct-b" || got.Launch != next || len(got.History) != 1 || got.History[0].Account != "acct-a" {
		t.Fatalf("binding: %+v", got)
	}
}
